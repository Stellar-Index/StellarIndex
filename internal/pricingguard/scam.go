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
// visible. Consumed at the price-reader seam so every reader-backed
// surface (/v1/price, /v1/price/batch, /v1/twap, /v1/vwap, the SEP-40
// oracle price paths, the asset headline, …) is covered by ONE gate.
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

// scamFlagTagSet is the set of curated-directory tags that withhold an
// issuer's price. It MUST mirror the frontend warning set in
// web/explorer/src/lib/directory-tags.ts (DIRECTORY_SCAM_FLAG_TAGS) so
// every asset that shows a scam BANNER also has its price withheld — a
// gate/warning split is exactly the drift this set exists to prevent.
// Matched case-insensitively. Keep the two lists identical; the paired
// tests (scam_test.go here, directory-tags.test.ts there) pin each.
var scamFlagTagSet = map[string]struct{}{
	"malicious": {},
	"unsafe":    {},
	"fraud":     {},
	"scam":      {},
	"hack":      {},
	"phishing":  {},
}

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

// NewScamGate builds a gate over the directory reader. A nil reader
// yields a nil gate (withholds nothing) so deployments without the
// directory table stay unguarded transparently — same shape as the
// directory overlay's nil handling.
func NewScamGate(dir ScamDirectoryReader, logger *slog.Logger) *ScamGate {
	if dir == nil {
		return nil
	}
	return &ScamGate{dir: dir, logger: logger, cache: make(map[string]scamVerdict)}
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
