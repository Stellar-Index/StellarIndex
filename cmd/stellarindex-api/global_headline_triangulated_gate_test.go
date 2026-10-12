package main

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/cmd/stellarindex-api/internal/wiring"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// The GlobalAssetView headline has three tiers, all gated, including the `vwap:` keys. Tier 3 (LookupTriangulated) is
// what a Stellar-only token reaches, and ungated it would show price and market cap for a flagged issuer. This pins the
// scam verdict; the unflagged case proves the tier still serves.

// headlineFlaggedIssuer is the directory-flagged issuer account. Same
// fixture the chokepoint's SAC test uses.
const headlineFlaggedIssuer = "GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"

// headlineCacheFixture publishes a triangulated VWAP for base/quote at
// `window`, exactly as the aggregator's triangulation worker does, and
// returns a reader wired to a directory that flags `flagged`.
func headlineCacheFixture(
	t *testing.T, base, quote canonical.Asset, window time.Duration, value string, flagged map[string]bool,
) (wiring.GlobalPriceReader, *flaggingScamDirectory) {
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
	return wiring.GlobalPriceReader{
		Tri:  wiring.RedisTriangulatedLooker{RDB: rdb},
		Scam: pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{}),
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

// TestGlobalHeadlineTriangulatedTierServesUnflaggedIssuer is the non-vacuity half: same fixture, unflagged directory,
// the tier must still serve the cached value, else a tier that simply stopped working would pass the test above.
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
