package main

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// The GlobalAssetView headline has THREE tiers and only the first one
// was gated (RLT-350's "the vwap: keys are all ungated").
//
// globalPriceReader.LatestVWAP — tier 1, the prices_1m bucket — routes
// through the priceWithheld chokepoint. globalPriceReader.
// LookupTriangulated — tier 3 — reads the aggregator's
// `vwap:<base>:<quote>:<window>` key and had no gate reference at all,
// and tier 3 is precisely the tier a Stellar-only token reaches: its
// literal <asset>/fiat:USD pair has no prices_1m rows, so tier 1 misses
// by construction and the headline comes from the cache.
//
// That is the shape of the 2026-08-25 decision this gate was built for:
// a flagged issuer's ASSET PAGE showing a price and a market cap.
//
// The gate here is the scam half only, so this test pins the scam
// verdict; the substance half is deliberately not asked on this tier
// (see the call site's comment) and the unflagged case below proves the
// tier still serves.

// headlineFlaggedIssuer is the directory-flagged issuer account. Same
// fixture the chokepoint's SAC test uses.
const headlineFlaggedIssuer = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"

// headlineCacheFixture publishes a triangulated VWAP for base/quote at
// `window`, exactly as the aggregator's triangulation worker does, and
// returns a reader wired to a directory that flags `flagged`.
func headlineCacheFixture(
	t *testing.T, base, quote canonical.Asset, window time.Duration, value string, flagged map[string]bool,
) (globalPriceReader, *flaggingScamDirectory) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	if err := mr.Set(cachekeys.VWAP(base, quote, window).String(), value); err != nil {
		t.Fatalf("seed vwap key: %v", err)
	}
	if err := mr.Set(
		cachekeys.VWAPProvenance(base, quote, window).String(),
		cachekeys.VWAPProvenanceTriangulated,
	); err != nil {
		t.Fatalf("seed provenance key: %v", err)
	}
	if err := mr.Set(
		cachekeys.VWAPObservedAt(base, quote, window).String(),
		cachekeys.FormatVWAPObservedAt(time.Now()),
	); err != nil {
		t.Fatalf("seed observed_at key: %v", err)
	}

	dir := &flaggingScamDirectory{flagged: flagged}
	return globalPriceReader{
		tri:  redisTriangulatedLooker{rdb: rdb},
		scam: pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{}),
	}, dir
}

func headlinePair(t *testing.T) (base, quote canonical.Asset) {
	t.Helper()
	base, err := canonical.NewClassicAsset("RIO", headlineFlaggedIssuer)
	if err != nil {
		t.Fatalf("classic asset: %v", err)
	}
	quote, err = canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatalf("build fiat:USD: %v", err)
	}
	return base, quote
}

// TestGlobalHeadlineTriangulatedTierWithholdsFlaggedIssuer is the
// headline case: the cache HOLDS a triangulated value for the flagged
// issuer's pair, and the tier must decline to publish it.
func TestGlobalHeadlineTriangulatedTierWithholdsFlaggedIssuer(t *testing.T) {
	base, quote := headlinePair(t)
	const window = 5 * time.Minute
	reader, dir := headlineCacheFixture(t, base, quote, window, "0.00723",
		map[string]bool{headlineFlaggedIssuer: true})

	price, _, ok, err := reader.LookupTriangulated(context.Background(), base, quote, window)
	if err != nil {
		t.Fatalf("LookupTriangulated: %v", err)
	}
	if ok {
		t.Errorf("tier 3 served %q for a directory-flagged issuer — the asset page's headline "+
			"price is the aggregated claim the scam gate exists to refuse (RLT-350); tier 1 of "+
			"this same reader already withholds it", price)
	}
	if price != "" {
		t.Errorf("withheld tier returned a price %q", price)
	}
	if len(dir.asked) == 0 || dir.asked[0] != headlineFlaggedIssuer {
		t.Errorf("directory asked about %v, want the issuer %q — the mechanism is the point, "+
			"not the bare miss", dir.asked, headlineFlaggedIssuer)
	}
}

// TestGlobalHeadlineTriangulatedTierServesUnflaggedIssuer is the
// non-vacuity and non-regression half: the same fixture, an unflagged
// directory, and the tier must still serve the cached value. Without
// this, a tier that simply stopped working would pass the test above.
func TestGlobalHeadlineTriangulatedTierServesUnflaggedIssuer(t *testing.T) {
	base, quote := headlinePair(t)
	const window = 5 * time.Minute
	reader, _ := headlineCacheFixture(t, base, quote, window, "0.00723", map[string]bool{})

	price, _, ok, err := reader.LookupTriangulated(context.Background(), base, quote, window)
	if err != nil {
		t.Fatalf("LookupTriangulated: %v", err)
	}
	if !ok || price != "0.00723" {
		t.Errorf("tier 3 returned (%q, ok=%v), want the cached triangulated value — an "+
			"unflagged issuer's headline must be unaffected", price, ok)
	}
}
