// Scam-pricing gate — the serving-side "issuer is flagged" floor.
//
// The substance gate (substance.go) answers "is this a REAL market?".
// It is structurally blind to a different question: "is this a SCAM?".
// An issuer can run a genuinely liquid market and still be a curated-
// directory-flagged fraud — RIO-GBNLJIYH… cleared the substance floor
// ~40× on real trading (volume_character = market) yet is tagged
// `unsafe`/deprecated-scam, so we published a $0.0072 price and a $540k
// market cap on a scam token's asset page (the 2026-08-25 decision that
// motivated this gate).
//
// This gate closes that class: for an asset whose ISSUER carries a
// scam-class tag in the curated account directory (migration 0136), it
// withholds the AGGREGATED price claim — the same posture and the same
// `errors/price-withheld` problem type as the substance gate — while the
// raw trade surfaces (/v1/ohlc, /v1/observations, /v1/history) stay
// visible.
//
// WHERE IT IS CONSUMED, precisely. This comment used to say the gate
// sat "at the price-reader seam so every reader-backed surface
// (/v1/price, /v1/price/batch, /v1/twap, /v1/vwap, …) is covered by ONE
// gate". That was never true of /v1/twap and /v1/vwap: they do not go
// through the price reader at all — they compute from raw trades via
// their own fetch — so for as long as the claim stood they served a
// flagged issuer's aggregated price at 200 (wave-D MSP-02/EXR-04,
// reproduced live). PR #182's merged body repeated the same claim.
//
// The gate is consumed per-surface, and the honest way to state the
// invariant is per-surface rather than "one seam":
//
//   - the price-reader seam — /v1/price, /v1/price/batch, and the
//     asset headline, via [Gate] (cmd/stellarindex-api's
//     priceWithheld chokepoint delegates to it);
//   - the SEP-40 oracle price paths, /v1/price/at and the DEX-TVL
//     valuation, same chokepoint;
//   - the aggregator's price-alert evaluator and its anomaly.freeze and
//     divergence.firing webhooks, via the same [Gate] — customer
//     webhooks fire off the same closed VWAP buckets;
//   - /v1/price/tip, in computeTip (the reader seam covers only the
//     middle branch of that function);
//   - /v1/price/stream, in closedStreamWithheld — at connect AND on
//     every forwarded closed bucket, because the aggregator publishes
//     that bucket with no gate consultation on the producer path;
//   - /v1/vwap, /v1/twap and /v1/chart, in their handlers.
//
// Every one of those sites asks the PAIR question. Inside
// internal/api/v1 they all route through its scamWithheld helper. Its
// AST guard (TestScamGateIsAskedThePairQuestion) fails on any `Withheld`
// selector under internal/api/v1, subpackages included, other than
// scamWithheld's own fallback, and on a pair question that names one leg
// twice. It cannot see a surface that consults no gate at all.
//
// The handlers are the correct site for the last group, NOT their
// shared tradesInRangeWithStablecoinFallback: that helper is also the
// fetch behind the single-bar /v1/ohlc, which this very paragraph
// promises stays visible.
//
// A new price-claim surface must add its own call. There is no seam
// that covers them all, and asserting one in a comment is how this gap
// survived — gate_guard_test.go fails on any function in any cmd/*
// binary that reads a closed VWAP bucket without asking a [Gate], but it
// cannot see a handler that computes its own price.
//
// BOTH LEGS, always. The withholding decision is a property of the
// MARKET, not of whichever leg the client happened to name first: a
// price of X in a flagged issuer's asset is the flagged market's own
// price, inverted. Keying the gate on the base alone meant
// `?base=native&quote=<FLAGGED>` republished, at 200 and
// unauthenticated, the exact reciprocal of the number
// `?base=<FLAGGED>&quote=native` had just refused — together with its
// volumes and trade counts (F002/F019/F032/T039). [ScamGate.WithheldPair]
// folds both legs INSIDE this package so no call site can consult one
// leg and forget the other; that fold is the thing new surfaces
// inherit, and a hand-written `Withheld(base) || Withheld(quote)` at a
// call site is the per-site drift it exists to prevent.
//
// It DELIBERATELY overturns the directory's historical "display-only,
// tags never gate pricing" invariant (asset_directory_tags.go).
//
// Fail posture — fail-OPEN, matching substance.go and the directory
// overlay: a directory-reader error (the directory is a LOCAL synced
// table, so an error means the local DB is unreachable) does NOT
// withhold — failing closed would blank EVERY asset's price on a DB
// blip and take the whole money surface dark. Every fail-open serve
// increments obs.ScamGateLookupFailuresTotal, which the
// stellarindex_scam_gate_fail_open alert watches, and logs a Warn.
//
// OPERATOR OVERRIDE of a false positive. The tags are a third party's
// judgement, and a wrong one withholds a legitimate issuer's price
// across every gated surface. There is deliberately no allow-list in
// THIS package: a second opinion stored here would disagree with the
// /v1/assets rank tier and the explorer's flag pill, which read the
// directory row directly. The correction is made where all three read
// from — an operator-owned row in account_directory carrying
// timescale.DirectoryOperatorOverrideSource, which `directory-sync`
// neither updates nor prunes, written by `stellarindex-ops
// directory-override -clear-scam-flag` (timescale.Store.ClearDirectoryScamFlag),
// which drops only the scam-class tags. This gate stops withholding
// within scamCacheTTL.
package pricingguard

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// scamFlagTagSet is the lookup form of the curated-directory tag
// vocabulary that withholds an issuer's price. The LIST itself lives in
// timescale.DirectoryScamFlagTags — next to the account_directory table
// it reads — because the /v1/assets listing SQL ranks on the same set
// and a storage package cannot import this one. It MUST also mirror the
// frontend warning set in web/explorer/src/lib/directory-tags.ts
// (DIRECTORY_SCAM_FLAG_TAGS) so every asset that shows a scam BANNER
// also has its price withheld and is demoted in the ranking — a
// gate/warning/ranking split is exactly the drift this one set exists to
// prevent. Matched case-insensitively; the paired tests (scam_test.go
// here, directory-tags.test.ts there) pin each.
var scamFlagTagSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(timescale.DirectoryScamFlagTags))
	for _, t := range timescale.DirectoryScamFlagTags {
		m[t] = struct{}{}
	}
	return m
}()

// IsDirectoryScamFlagged reports whether any tag is a scam-class flag
// (case-insensitive). Exported for the API layer's payload suppression
// (listing/detail market_cap/fdv) so the reader gate and the payload
// suppression share ONE predicate.
func IsDirectoryScamFlagged(tags []string) bool {
	for _, t := range tags {
		if _, ok := scamFlagTagSet[strings.ToLower(strings.TrimSpace(t))]; ok {
			return true
		}
	}
	return false
}

// ScamDirectoryReader is the storage seam the gate needs: the curated
// account-directory lookup. *timescale.Store satisfies it.
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
)

type scamVerdict struct {
	withheld bool
	expires  time.Time
}

// ScamGate is the serving-side scam-issuer price gate. Construct with
// [NewScamGate]; a nil *ScamGate is a valid no-op gate that withholds
// nothing (so callers don't need their own nil checks).
type ScamGate struct {
	dir    ScamDirectoryReader
	logger *slog.Logger
	now    func() time.Time // nil → time.Now

	mu    sync.Mutex
	cache map[string]scamVerdict // keyed by issuer G-address

	flaggedAssets scamFlaggedAssetLister // nil → no SAC index
	sacMu         sync.Mutex
	sacIndex      map[string]struct{} // SAC contract ids of flagged issuances
	sacExpires    time.Time
}

// ScamGateOptions tunes a ScamGate. Logger rides here (not as a
// positional constructor param) per the repo's constructor idiom —
// mirrors SubstanceGateOptions.
type ScamGateOptions struct {
	Logger *slog.Logger
}

// NewScamGate builds a gate over the directory reader. A nil reader
// yields a nil gate (withholds nothing) so deployments without the
// directory table stay unguarded transparently — same shape as the
// directory overlay's nil handling.
func NewScamGate(dir ScamDirectoryReader, opts ScamGateOptions) *ScamGate {
	if dir == nil {
		return nil
	}
	g := &ScamGate{dir: dir, logger: opts.Logger, cache: make(map[string]scamVerdict)}
	g.flaggedAssets, _ = dir.(scamFlaggedAssetLister)
	return g
}

// Gate is the ONE withholding decision every binary consults before it
// publishes an aggregated price: withhold when the thin-market substance
// gate refuses the pair, OR when either leg's issuer is
// directory-scam-flagged.
//
// It lives here rather than in a binary because a decision spelled once
// per binary drifts once per binary: the aggregator's price-alert
// evaluator once consulted the substance half alone, so an alert could
// name a price /v1/price refuses to publish. Its methods are the only
// exported expression that folds the two halves; the AST guard in
// gate_guard_test.go fails on any cmd/* file that consults a half
// directly or reads a closed VWAP bucket without asking a Gate.
//
// Both halves are nil-receiver safe (nil == allow everything), so an
// operator who disabled [pricing_guard] keeps today's behaviour, and the
// zero Gate withholds nothing.
type Gate struct {
	Substance *SubstanceGate // nil → no thin-market floor
	Scam      *ScamGate      // nil → no scam-issuer gate
}

// PriceWithheld reports whether the pair's aggregated price must not be
// published. `surface` is a low-cardinality metric label.
func (g Gate) PriceWithheld(ctx context.Context, base, quote canonical.Asset, surface string) bool {
	return g.PriceWithholding(ctx, base, quote, surface) != NotWithheld
}

// Withholding names which gate withheld a pair's price, so a surface
// can tell its reader the true cause. The zero value is [NotWithheld].
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
// The scam gate is asked FIRST: when both would withhold, the flag is the
// true reason, and reporting the pair as merely thin would hand the
// client the recompute-it-yourself advice the flag exists to refuse.
func (g Gate) PriceWithholding(ctx context.Context, base, quote canonical.Asset, surface string) Withholding {
	return WithholdingFor(g.Scam.WithheldPair(ctx, base, quote, surface), g.Substance.Allowed(ctx, base, quote, surface))
}

// PriceWithholdingAt is [Gate.PriceWithholding] for a point-in-time read:
// the substance half is asked about the instant being served instead of
// about now. The scam half is deliberately NOT moved in time — a
// directory flag is an owner-level trust decision about the issuer, and
// it withholds that issuer's history along with its present.
func (g Gate) PriceWithholdingAt(ctx context.Context, base, quote canonical.Asset, at time.Time, surface string) Withholding {
	return WithholdingFor(g.Scam.WithheldPair(ctx, base, quote, surface), g.Substance.AllowedAt(ctx, base, quote, at, surface))
}

// WithholdingFor folds the two gates' verdicts into one; a flagged issuer
// takes precedence. Callers holding their own gate seams fold through here.
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

// WithheldPair reports whether the aggregated price for the pair
// base/quote must be withheld because EITHER leg's issuer is
// directory-scam-flagged. This is the form every price surface should
// consult: the decision is a property of the market, and the same
// market is named by both orientations of the pair (see the "BOTH
// LEGS, always" paragraph in the package doc).
//
// The fold is a short-circuiting OR, so a flagged base costs exactly
// the one directory lookup it always did and increments the metric
// once.
func (g *ScamGate) WithheldPair(ctx context.Context, base, quote canonical.Asset, surface string) bool {
	return g.withheldLeg(ctx, base, surface) || g.withheldLeg(ctx, quote, surface)
}

// Withheld is the BASE-ONLY spelling of the decision. It answers HALF
// the question and NO price-serving path asks it any more.
//
// Every surface that once did was migrated to [ScamGate.WithheldPair]
// (via internal/api/v1's scamWithheld helper): /v1/twap and /v1/chart
// under F019/F032, and /v1/price/tip plus the closed-bucket price
// stream — the last two — under F002/K001. internal/api/v1's
// TestScamGateIsAskedThePairQuestion fails on any selector of this
// method under internal/api/v1 — whatever its receiver, subpackages
// included — outside scamWithheld's fallback, with no exemption list.
//
// It survives only because the v1.PriceScamGate interface declares it,
// and that interface is what v1.PriceScamPairGate embeds; production
// wires *ScamGate, which satisfies the pair form
// (TestProductionScamGateIsPairAware pins that, so v1.scamWithheld's
// base-only fallback is unreachable in production). Deleting the method
// outright means editing internal/api/v1/price.go and the fakes in
// several test files — a follow-up, not a change this one can make.
// Until then: it is not a second policy — it shares withheldLeg with
// the pair form — but it must not be the form a new surface reaches
// for.
//
// `surface` labels the metric (obs.PriceServeScamWithheldTotal) — a
// low-cardinality constant ("price_read", "tip", "asset_headline", …),
// never a pair string. Nil-receiver safe. Fail-open on directory error.
func (g *ScamGate) Withheld(ctx context.Context, base canonical.Asset, surface string) bool {
	return g.withheldLeg(ctx, base, surface)
}

// withheldLeg is the single-asset predicate both exported forms fold
// over: it answers "is THIS asset's issuer directory-scam-flagged?".
// Nil-receiver safe. Fail-open on directory error.
//
// Only CLASSIC assets have a directory-flaggable issuer G-address;
// native / fiat / crypto-CEX / bare-Soroban assets return false.
//
// The asset is resolved to its CANONICAL family form before that check
// (canonical.CanonicalAsset), because a Stellar Asset Contract wrapper
// is the same asset as the classic issuance it wraps while carrying no
// G-address of its own. Without the resolution the classic check
// rejected every SAC spelling as "nothing to flag", so a flagged
// issuer's price stayed servable to anyone who named the wrapper's
// C-address instead of `CODE-ISSUER` — on /v1/price, /v1/vwap,
// /v1/twap, /v1/price/tip and /v1/chart alike, since every consultation
// passes the raw requested asset straight through
// (docs/methodology/d7-thin-pool-third-alias-vwap-review-2026-09-04.md, R8).
// Resolving HERE rather than at each caller is what makes the
// consultations agree: a new price surface inherits it — and now so
// does the quote leg, which the SAC bypass would otherwise re-open one
// orientation at a time.
//
// Direction matters and is one-way. A configured classic↔SAC family is
// ordered classic-first (canonical.NewAliasRegistry), so the canonical
// form of a classic asset is itself and a classic-keyed request is
// bit-for-bit unchanged; only the SAC spelling moves. XLM's SAC
// canonicalises to `native`, which has no issuer and so returns false.
//
// A SAC with no `[supply].sac_wrappers` entry resolves to itself — a
// C-address cannot be inverted to its classic asset without that table —
// so a contract leg is matched against [ScamGate.flaggedSAC]'s index
// instead: the derived SAC ids of every registered classic asset whose
// issuer is flagged. Derivation, not metadata, is the trust anchor, so a
// genuine SEP-41 token can never match. What stays unrecognised is a SAC
// whose classic asset is absent from classic_assets.
func (g *ScamGate) withheldLeg(ctx context.Context, asset canonical.Asset, surface string) bool {
	if g == nil {
		return false
	}
	asset = canonical.CanonicalAsset(asset)
	if asset.Type == canonical.AssetSoroban {
		return g.flaggedSAC(ctx, asset.ContractID, surface)
	}
	if asset.Type != canonical.AssetClassic || asset.Issuer == "" {
		return false
	}
	key := asset.Issuer
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
					"issuer", key, "surface", surface, "err", err)
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

// flaggedSACIndex returns the cached SAC-id set, rebuilding it once per
// scamCacheTTL so a cleared flag stops withholding on the same clock as
// the issuer cache.
func (g *ScamGate) flaggedSACIndex(ctx context.Context) (map[string]struct{}, error) {
	now := g.clock()
	g.sacMu.Lock()
	defer g.sacMu.Unlock()
	if g.sacIndex != nil && now.Before(g.sacExpires) {
		return g.sacIndex, nil
	}
	assets, err := g.flaggedAssets.DirectoryScamFlaggedClassicAssets(ctx)
	if err != nil {
		return nil, err
	}
	index := make(map[string]struct{}, len(assets))
	for _, a := range assets {
		// An invalid stored code or issuer has no derivable SAC to match.
		if cid, derr := a.SacContractID(); derr == nil {
			index[cid] = struct{}{}
		}
	}
	g.sacIndex, g.sacExpires = index, now.Add(scamCacheTTL)
	return index, nil
}

func (g *ScamGate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}
