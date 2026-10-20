// Scam-pricing gate: the serving-side "issuer is flagged" floor. The substance gate is blind to a
// liquid market run by a flagged fraud; for an asset whose ISSUER carries a scam-class directory
// tag, this gate withholds the AGGREGATED price claim (raw trade surfaces stay visible). It
// DELIBERATELY overturns the directory's "tags never gate pricing" invariant.
//
// A new price-claim surface (price reader, alerts, webhooks, stream, vwap/twap/chart handlers)
// must add its own call; the chokepoint is priceWithheld via [Gate].
//
// BOTH LEGS, always: withholding is a property of the MARKET, else `?base=native&quote=<FLAGGED>`
// would republish the reciprocal of the refused price ([ScamGate.WithheldPair] folds both legs).
//
// Fail-OPEN, like substance.go: a directory-reader error does not withhold (counted in
// obs.ScamGateLookupFailuresTotal); failing closed blanks every price.
//
// OPERATOR OVERRIDE of a false positive: `stellarindex-ops directory-override -clear-scam-flag`
// drops only the scam-class tags; the gate follows within scamCacheTTL. No allow-list here: it
// would disagree with the /v1/assets rank tier and the explorer flag pill.
package pricingguard

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

// scamFlagTagSet is the lookup form of timescale.DirectoryScamFlagTags (kept beside account_directory because storage cannot import
// this package). It MUST mirror web/explorer/src/lib/directory-tags.ts (DIRECTORY_SCAM_FLAG_TAGS); scam_test.go and
// directory-tags.test.ts pin each side.
var scamFlagTagSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(timescale.DirectoryScamFlagTags))
	for _, t := range timescale.DirectoryScamFlagTags {
		m[t] = struct{}{}
	}
	return m
}()

// IsDirectoryScamFlagged reports whether any tag is a scam-class flag (case-insensitive).
func IsDirectoryScamFlagged(tags []string) bool {
	for _, t := range tags {
		if _, ok := scamFlagTagSet[timescale.CanonicalDirectoryTag(t)]; ok {
			return true
		}
	}
	return false
}

// ScamDirectoryReader is the storage seam the gate needs: the curated account-directory lookup.
type ScamDirectoryReader interface {
	DirectoryEntryByAddress(ctx context.Context, address string) (timescale.DirectoryEntry, bool, error)
}

// scamFlaggedAssetLister is the optional seam behind the SAC index: every
// registered classic asset whose issuer is scam-flagged. A reader without
// it leaves the gate unable to recognise an unregistered SAC spelling.
type scamFlaggedAssetLister interface {
	DirectoryScamFlaggedClassicAssets(ctx context.Context) ([]canonical.Asset, error)
}

// Both binaries hand NewScamGate a *timescale.Store; if it stopped
// providing the listing the SAC index would disarm without a build error.
var _ scamFlaggedAssetLister = (*timescale.Store)(nil)

const (
	scamCacheTTL = 60 * time.Second
	scamCacheMax = 8192

	sacRefreshTimeout = 30 * time.Second
)

type scamVerdict struct {
	withheld bool
	expires  time.Time
}

// ScamGate is the serving-side scam-issuer price gate.
type ScamGate struct {
	dir    ScamDirectoryReader
	logger *slog.Logger
	now    func() time.Time // nil → time.Now

	mu    sync.Mutex
	cache map[string]scamVerdict // keyed by issuer G-address or contract C-address

	flaggedAssets scamFlaggedAssetLister // nil → no SAC index
	sacMu         sync.Mutex
	sacIndex      map[string]struct{} // SAC contract ids of flagged issuances
	sacExpires    time.Time
	sacRefreshing bool
	sacRefresh    sync.WaitGroup // tracks the background rebuild, for tests
}

// ScamGateOptions tunes a ScamGate.
type ScamGateOptions struct {
	Logger *slog.Logger
}

// NewScamGate builds a gate over the directory reader.
func NewScamGate(dir ScamDirectoryReader, opts ScamGateOptions) *ScamGate {
	if dir == nil {
		return nil
	}
	g := &ScamGate{dir: dir, logger: opts.Logger, cache: make(map[string]scamVerdict)}
	g.flaggedAssets, _ = dir.(scamFlaggedAssetLister)
	return g
}

// Gate is the ONE withholding decision every binary consults before publishing an aggregated price: withhold when the substance gate refuses the pair or either leg's issuer is scam-flagged; nil-receiver safe (nil allows everything).
type Gate struct {
	Substance *SubstanceGate // nil → no thin-market floor
	Scam      *ScamGate      // nil → no scam-issuer gate
}

// PriceWithheld reports whether the pair's aggregated price must not be published.
func (g Gate) PriceWithheld(ctx context.Context, base, quote canonical.Asset, surface string) bool {
	return g.PriceWithholding(ctx, base, quote, surface) != NotWithheld
}

// Query names what a [Gate.Judge] call is asked about.
type Query struct {
	// PointInTime asks the substance half about At instead of now.
	PointInTime bool
	At          time.Time
	// AdmitThin serves a measured-thin pair instead of withholding it
	// (the include_thin opt-in). It never releases a flagged issuer.
	AdmitThin bool
}

// Verdict is [Gate.Judge]'s answer.
type Verdict struct {
	Withholding
	Substance *SubstanceEvidence
	// ThinAdmitted: the pair measured thin and was served under AdmitThin.
	ThinAdmitted bool
}

// Fold is the one fold of both halves; a scam flag wins and carries no evidence, and an unmeasured pair fails open.
func Fold(scamFlagged bool, v SubstanceVerdict, admitThin bool, surface string) Verdict {
	if scamFlagged {
		return Verdict{Withholding: WithheldFlaggedIssuer}
	}
	switch {
	case !v.Measured:
		obs.PriceServeSubstanceUnmeasuredTotal.WithLabelValues(surface).Inc()
	case !v.Allowed && admitThin:
		obs.PriceServeThinAdmittedTotal.WithLabelValues(surface, string(v.Floor)).Inc()
	case !v.Allowed:
		obs.PriceServeSubstanceWithheldTotal.WithLabelValues(surface, string(v.Floor)).Inc()
	}
	out := Verdict{Substance: v.Evidence}
	switch {
	case v.Measured && !v.Allowed && admitThin:
		out.ThinAdmitted = true
	case v.Measured && !v.Allowed:
		out.Withholding = WithheldThinMarket
	}
	return out
}

// Judge is the gate's full answer for one pair: which half withheld it, the substance evidence, and whether a thin market was admitted.
func (g Gate) Judge(ctx context.Context, base, quote canonical.Asset, surface string, q Query) Verdict {
	if g.Scam.WithheldPair(ctx, base, quote, surface) {
		return Fold(true, SubstanceVerdict{}, false, surface)
	}
	var v SubstanceVerdict
	if q.PointInTime {
		v = g.Substance.MeasureAt(ctx, base, quote, q.At)
	} else {
		v = g.Substance.Measure(ctx, base, quote)
	}
	return Fold(false, v, q.AdmitThin, surface)
}

// Withholding names which gate withheld a pair's price, so a surface can tell its reader the true cause.
type Withholding string

const (
	// NotWithheld: neither gate refused the pair.
	NotWithheld Withholding = ""
	// WithheldThinMarket: the substance gate refused — trailing market
	// activity is below the serve floor.
	WithheldThinMarket Withholding = "thin_market"
	// WithheldFlaggedIssuer: either leg's issuer is directory-scam-flagged.
	WithheldFlaggedIssuer Withholding = "flagged_issuer"
)

// PriceWithholding is [Gate.PriceWithheld] reporting WHICH gate fired.
func (g Gate) PriceWithholding(ctx context.Context, base, quote canonical.Asset, surface string) Withholding {
	return g.Judge(ctx, base, quote, surface, Query{}).Withholding
}

// PriceWithholdingAt is [Gate.PriceWithholding] for a point-in-time read: the substance half is asked about the instant served, but the scam half is deliberately NOT moved in time (an issuer flag withholds its history too).
func (g Gate) PriceWithholdingAt(ctx context.Context, base, quote canonical.Asset, at time.Time, surface string) Withholding {
	return g.Judge(ctx, base, quote, surface, Query{PointInTime: true, At: at}).Withholding
}

// AssetValueWithholding is the gate's answer for an ASSET's USD value rather than one pair: withheld when its issuer is flagged, or when no backing quote ([AssetSubstanceVerdict]) clears the substance floor.
func (g Gate) AssetValueWithholding(ctx context.Context, asset canonical.Asset, usdPegs []canonical.Asset, surface string) Withholding {
	if g.Scam.withheldLeg(ctx, asset, surface) {
		return WithheldFlaggedIssuer
	}
	allowed, measured := AssetSubstanceVerdict(ctx, g.Substance, asset, usdPegs, surface)
	return WithholdingFor(false, allowed || !measured)
}

// WithholdingFor folds the two gates' verdicts into one; a flagged issuer takes precedence.
func WithholdingFor(scamFlagged, substanceAllowed bool) Withholding {
	switch {
	case scamFlagged:
		return WithheldFlaggedIssuer
	case !substanceAllowed:
		return WithheldThinMarket
	default:
		return NotWithheld
	}
}

// WithheldPair reports whether the aggregated price for base/quote must be withheld because EITHER leg's issuer is directory-scam-flagged.
func (g *ScamGate) WithheldPair(ctx context.Context, base, quote canonical.Asset, surface string) bool {
	return g.withheldLeg(ctx, base, surface) || g.withheldLeg(ctx, quote, surface)
}

// Withheld is the BASE-ONLY spelling of the decision: no price-serving path may ask it (use [ScamGate.WithheldPair]); `surface` is a low-cardinality metric label, and it fails open on directory error.
func (g *ScamGate) Withheld(ctx context.Context, base canonical.Asset, surface string) bool {
	return g.withheldLeg(ctx, base, surface)
}

// withheldLeg is the single-asset predicate both exported forms fold over: is THIS asset's issuer directory-scam-flagged? Nil-receiver
// safe; fail-open on directory error. Classic assets are judged on the issuer G-address, contract tokens on their C-address; the asset
// is first resolved via canonical.CanonicalAsset, else a SAC wrapper would keep a flagged issuer's price servable ([ScamGate.flaggedSAC]).
func (g *ScamGate) withheldLeg(ctx context.Context, asset canonical.Asset, surface string) bool {
	if g == nil {
		return false
	}
	asset = canonical.CanonicalAsset(asset)
	if asset.Type == canonical.AssetSoroban {
		if asset.ContractID == "" {
			return false
		}
		return g.flaggedSAC(ctx, asset.ContractID, surface) ||
			g.directoryFlagged(ctx, asset.ContractID, surface)
	}
	if asset.Type != canonical.AssetClassic || asset.Issuer == "" {
		return false
	}
	return g.directoryFlagged(ctx, asset.Issuer, surface)
}

// directoryFlagged reports whether the directory entry for key (an issuer
// G-address or a contract C-address) carries a scam-class tag. Cached per
// address; fail-open on a directory error, counted and not cached.
func (g *ScamGate) directoryFlagged(ctx context.Context, key, surface string) bool {
	now := g.clock()

	g.mu.Lock()
	if v, ok := g.cache[key]; ok && now.Before(v.expires) {
		g.mu.Unlock()
		if v.withheld {
			obs.PriceServeScamWithheldTotal.WithLabelValues(surface).Inc()
		}
		return v.withheld
	}
	g.mu.Unlock()

	e, found, err := g.dir.DirectoryEntryByAddress(ctx, key)
	if err != nil {
		// Fail-OPEN. Do NOT cache (re-ask next time). Counted and logged
		// on the live path only — a cancelled request serves nothing, so
		// it is not an unguarded serve.
		if ctx.Err() == nil {
			obs.ScamGateLookupFailuresTotal.WithLabelValues(surface).Inc()
			if g.logger != nil {
				g.logger.Warn("scam pricing gate: directory lookup failed — serving unguarded",
					"address", key, "surface", surface, "err", err)
			}
		}
		return false
	}
	withheld := found && IsDirectoryScamFlagged(e.Tags)

	g.mu.Lock()
	if len(g.cache) >= scamCacheMax {
		g.cache = make(map[string]scamVerdict)
	}
	g.cache[key] = scamVerdict{withheld: withheld, expires: now.Add(scamCacheTTL)}
	g.mu.Unlock()

	if withheld {
		obs.PriceServeScamWithheldTotal.WithLabelValues(surface).Inc()
	}
	return withheld
}

// flaggedSAC reports whether contractID is the SAC of a flagged issuer's
// classic asset. Fail-open on a listing error, counted like a directory
// error; the failure is not cached.
func (g *ScamGate) flaggedSAC(ctx context.Context, contractID, surface string) bool {
	if g.flaggedAssets == nil || contractID == "" {
		return false
	}
	index, err := g.flaggedSACIndex(ctx)
	if err != nil {
		if ctx.Err() == nil {
			obs.ScamGateLookupFailuresTotal.WithLabelValues(surface).Inc()
			if g.logger != nil {
				g.logger.Warn("scam pricing gate: flagged-asset listing failed — serving contract leg unguarded",
					"contract", contractID, "surface", surface, "err", err)
			}
		}
		return false
	}
	if _, ok := index[contractID]; !ok {
		return false
	}
	obs.PriceServeScamWithheldTotal.WithLabelValues(surface).Inc()
	return true
}

// flaggedSACIndex returns the cached SAC-id set. Only the first build blocks: the listing is
// ~180k rows, so an expired index is served while one background rebuild replaces it, and a
// cleared or new flag takes effect within scamCacheTTL plus one rebuild.
func (g *ScamGate) flaggedSACIndex(ctx context.Context) (map[string]struct{}, error) {
	g.sacMu.Lock()
	defer g.sacMu.Unlock()
	if g.sacIndex == nil {
		return g.buildSACIndexLocked(ctx)
	}
	if !g.clock().Before(g.sacExpires) && !g.sacRefreshing {
		g.sacRefreshing = true
		g.sacRefresh.Add(1)
		go g.refreshSACIndex(context.WithoutCancel(ctx))
	}
	return g.sacIndex, nil
}

// WarmSACIndex builds the SAC index before the first priced request would; a failure is left to that request.
func (g *ScamGate) WarmSACIndex(ctx context.Context) {
	if g == nil || g.flaggedAssets == nil {
		return
	}
	_, _ = g.flaggedSACIndex(ctx)
}

// refreshSACIndex rebuilds the index off the request path. On failure the previous index stays
// (safer than the fail-open first build) and the next expired read retries.
func (g *ScamGate) refreshSACIndex(ctx context.Context) {
	defer g.sacRefresh.Done()
	// A panic must still clear sacRefreshing, or no rebuild would ever start again.
	defer func() {
		if r := recover(); r != nil {
			worker.Report(g.logger, "scam-sac-index-refresh", r)
			g.sacMu.Lock()
			g.sacRefreshing = false
			g.sacMu.Unlock()
		}
	}()
	ctx, cancel := context.WithTimeout(ctx, sacRefreshTimeout)
	defer cancel()
	assets, err := g.flaggedAssets.DirectoryScamFlaggedClassicAssets(ctx)
	g.sacMu.Lock()
	defer g.sacMu.Unlock()
	g.sacRefreshing = false
	if err != nil {
		obs.ScamGateLookupFailuresTotal.WithLabelValues("sac_index_refresh").Inc()
		if g.logger != nil {
			g.logger.Warn("scam pricing gate: flagged-asset listing refresh failed — keeping the previous SAC index", "err", err)
		}
		return
	}
	g.sacIndex, g.sacExpires = sacIndexOf(assets), g.clock().Add(scamCacheTTL)
}

func (g *ScamGate) buildSACIndexLocked(ctx context.Context) (map[string]struct{}, error) {
	assets, err := g.flaggedAssets.DirectoryScamFlaggedClassicAssets(ctx)
	if err != nil {
		return nil, err
	}
	g.sacIndex, g.sacExpires = sacIndexOf(assets), g.clock().Add(scamCacheTTL)
	return g.sacIndex, nil
}

func sacIndexOf(assets []canonical.Asset) map[string]struct{} {
	index := make(map[string]struct{}, len(assets))
	for _, a := range assets {
		// An invalid stored code or issuer has no derivable SAC to match.
		if cid, derr := a.SacContractID(); derr == nil {
			index[cid] = struct{}{}
		}
	}
	return index
}

func (g *ScamGate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}
