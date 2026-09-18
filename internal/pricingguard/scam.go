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
//     asset headline, via [PriceWithheld] (cmd/stellarindex-api's
//     priceWithheld chokepoint delegates to it);
//   - the SEP-40 oracle price paths, /v1/price/at and the DEX-TVL
//     valuation, same chokepoint;
//   - the aggregator's price-alert evaluator, same chokepoint —
//     customer webhooks fire off the same closed VWAP buckets;
//   - /v1/price/tip, in computeTip (the reader seam covers only the
//     middle branch of that function);
//   - /v1/price/stream, in closedStreamWithheld — at connect AND on
//     every forwarded closed bucket, because the aggregator publishes
//     that bucket with no gate consultation on the producer path;
//   - /v1/vwap, /v1/twap and /v1/chart, in their handlers.
//
// Every one of those sites asks the PAIR question. Inside
// internal/api/v1 they all route through its scamWithheld helper, whose
// AST guard (TestScamGateIsAskedThePairQuestion) permits no base-only
// consultation and no exemption for one.
//
// The handlers are the correct site for the last group, NOT their
// shared tradesInRangeWithStablecoinFallback: that helper is also the
// fetch behind the single-bar /v1/ohlc, which this very paragraph
// promises stays visible.
//
// A new price-claim surface must add its own call. There is no seam
// that covers them all, and asserting one in a comment is how this gap
// survived — cmd/stellarindex-api's TestPriceServingSeamsAreGated
// enumerates the reader-backed ones so a new ungated seam fails CI,
// but it cannot see a handler that computes its own price.
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
// blip and take the whole money surface dark. The fail-open path logs
// (transition-only) so a silent re-exposure is observable.
//
// OPERATOR OVERRIDE of a false positive. The tags are a third party's
// judgement, and a wrong one withholds a legitimate issuer's price
// across every gated surface. There is deliberately no allow-list in
// THIS package: a second opinion stored here would disagree with the
// /v1/assets rank tier and the explorer's flag pill, which read the
// directory row directly. The correction is made where all three read
// from — an operator-owned row in account_directory carrying
// timescale.DirectoryOperatorOverrideSource, which `directory-sync`
// neither updates nor prunes (timescale.Store.UpsertDirectoryOverride,
// .DeleteDirectoryOverride). Drop the scam-class tags there and this
// gate stops withholding within scamCacheTTL.
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
	return &ScamGate{dir: dir, logger: opts.Logger, cache: make(map[string]scamVerdict)}
}

// PriceWithheld is the ONE withholding decision, consumed by BOTH
// binaries: withhold when the thin-market substance gate refuses the
// pair, OR when either leg's issuer is directory-scam-flagged.
//
// It lives here rather than in cmd/stellarindex-api because a decision
// spelled once per binary drifts once per binary. The API binary had
// the only copy, so the aggregator's price-alert evaluator — which
// fires customer webhooks off the same closed VWAP buckets the API
// serves — consulted the substance gate alone and the scam gate not at
// all: an alert could name a price /v1/price refuses to publish
// (F002/K001). cmd/stellarindex-api's priceWithheld and the
// aggregator's price-alert reader both delegate here.
//
// Both gates are nil-receiver safe (nil == allow everything), so an
// operator who disabled [pricing_guard] keeps today's behaviour.
func PriceWithheld(
	ctx context.Context,
	substance *SubstanceGate,
	scam *ScamGate,
	base, quote canonical.Asset,
	surface string,
) bool {
	return !substance.Allowed(ctx, base, quote, surface) || scam.WithheldPair(ctx, base, quote, surface)
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
// TestScamGateIsAskedThePairQuestion now permits ZERO base-only
// consultations and carries no exemption mechanism, so one cannot be
// reintroduced there without failing CI.
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
// (docs/audit/d7-thin-pool-third-alias-vwap-review-2026-09-04.md, R8).
// Resolving HERE rather than at each caller is what makes the
// consultations agree: a new price surface inherits it — and now so
// does the quote leg, which the SAC bypass would otherwise re-open one
// orientation at a time.
//
// Direction matters and is one-way. A configured classic↔SAC family is
// ordered classic-first (canonical.NewAliasRegistry), so the canonical
// form of a classic asset is itself and a classic-keyed request is
// bit-for-bit unchanged; only the SAC spelling moves. A deployment with
// no `[supply].sac_wrappers` entry for the asset resolves it to itself,
// so the resolution is a no-op there rather than a behaviour change.
// XLM's SAC canonicalises to `native`, which has no issuer and so still
// returns false — as it did before.
func (g *ScamGate) withheldLeg(ctx context.Context, asset canonical.Asset, surface string) bool {
	if g == nil {
		return false
	}
	asset = canonical.CanonicalAsset(asset)
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
		// Fail-OPEN. Do NOT cache (re-ask next time), and log on the
		// live path only so a silent re-exposure is observable without
		// spamming on client cancellations.
		if g.logger != nil && ctx.Err() == nil {
			g.logger.Warn("scam pricing gate: directory lookup failed — serving unguarded",
				"issuer", key, "surface", surface, "err", err)
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

func (g *ScamGate) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}
