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
// The gate is now consumed at SIX sites, and the honest way to state
// the invariant is per-surface rather than "one seam":
//
//   - the price-reader seam — /v1/price, /v1/price/batch, and the
//     asset headline, via cmd/stellarindex-api's priceWithheld
//     chokepoint;
//   - the SEP-40 oracle price paths, same chokepoint;
//   - /v1/price/tip, in computeTip (the reader seam covers only the
//     middle branch of that function);
//   - /v1/vwap and /v1/twap, in their handlers.
//
// The handlers are the correct site for the last two, NOT their shared
// tradesInRangeWithStablecoinFallback: that helper is also the fetch
// behind the single-bar /v1/ohlc, which this very paragraph promises
// stays visible.
//
// A new price-claim surface must add its own call. There is no seam
// that covers them all, and asserting one in a comment is how this gap
// survived — cmd/stellarindex-api's TestPriceServingSeamsAreGated
// enumerates the reader-backed ones so a new ungated seam fails CI,
// but it cannot see a handler that computes its own price.
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

// Withheld reports whether the aggregated price for `base` must be
// withheld because its issuer is directory-scam-flagged. `surface`
// labels the metric (obs.PriceServeScamWithheldTotal) — a
// low-cardinality constant ("price_read", "tip", "asset_headline", …),
// never a pair string. Nil-receiver safe. Fail-open on directory error.
//
// Only CLASSIC assets have a directory-flaggable issuer G-address;
// native / fiat / crypto-CEX / bare-Soroban assets return false.
func (g *ScamGate) Withheld(ctx context.Context, base canonical.Asset, surface string) bool {
	if g == nil {
		return false
	}
	if base.Type != canonical.AssetClassic || base.Issuer == "" {
		return false
	}
	key := base.Issuer
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
