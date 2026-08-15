package orchestrator

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ratFromDecimal parses an exact decimal string into a *big.Rat for the
// tests below; fails the test on a malformed literal.
func ratFromDecimal(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("SetString(%q) failed", s)
	}
	return r
}

// TestFormatRatFixed_TinyPositiveNeverReparsesToZero is R-1: a strictly-
// positive rational below 10^-decimals must NOT render to a string that
// reparses to zero. Before the magnitude-relative render, formatRatFixed
// truncated any 0<r<1e-12 to "0.000000000000", which big.Rat.SetString
// reads back as 0 — served as price 0 AND (read back as a chain leg the
// next tick) collapses the whole window edge graph.
func TestFormatRatFixed_TinyPositiveNeverReparsesToZero(t *testing.T) {
	tiny := []string{
		"0.0000000000005",      // 5e-13 — the finding's exact value
		"0.0000000000009",      // 9e-13
		"0.000000000000001234", // ~1.2e-15, a high-decimal token priced in BTC
	}
	for _, s := range tiny {
		r := ratFromDecimal(t, s)
		got := formatRatFixed(r, 12)
		parsed, ok := new(big.Rat).SetString(got)
		if !ok {
			t.Fatalf("formatRatFixed(%s,12)=%q did not reparse", s, got)
		}
		if parsed.Sign() <= 0 {
			t.Errorf("formatRatFixed(%s,12)=%q reparses to Sign()=%d — a strictly-positive "+
				"price rendered to a zero-reparsing string (R-1)", s, got, parsed.Sign())
		}
	}

	// Regression-safety half: a NORMAL-magnitude price must render
	// byte-identically to the pre-fix 12-decimal truncation, so the fix
	// only extends precision for values that would otherwise vanish.
	unchanged := map[string]string{
		"0.4968":  "0.496800000000",
		"1.5":     "1.500000000000",
		"0.01":    "0.010000000000",
		"123.456": "123.456000000000",
	}
	for in, want := range unchanged {
		if got := formatRatFixed(ratFromDecimal(t, in), 12); got != want {
			t.Errorf("formatRatFixed(%s,12)=%q, want byte-identical %q (normal magnitude must be untouched)",
				in, got, want)
		}
	}
}

// TestBuildWindowEdges_ZeroLegDoesNotCollapseWindow is the R-1 belt-and-
// suspenders: a single leg whose cached VWAP parses to a non-positive
// price must be dropped from the graph, NOT abort BuildEdges and nil the
// ENTIRE window (which turned one micro-valued pair into a window-wide
// triangulation outage). Every other priced pair in the window must
// survive.
func TestBuildWindowEdges_ZeroLegDoesNotCollapseWindow(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD") // the poisoned leg
	usdEUR := mkPair(t, "fiat", "USD", "fiat", "EUR")   // (absent) sibling leg
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR") // triangulation target
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP") // an unrelated priced pair
	gbpEUR := mkPair(t, "fiat", "GBP", "fiat", "EUR")   // keeps xlmGBP reachable
	window := time.Minute

	cache, _ := newTestRedis(t)
	// The poisoned leg: a legacy zero-reparsing string in the shared key.
	setLegVWAP(t, cache, xlmUSD, window, "0.000000000000")

	o := New(&mockStore{}, cache, Config{
		Windows: []time.Duration{window},
		Triangulations: []TriangulationChain{
			{Target: xlmEUR, Legs: []canonical.Pair{xlmUSD, usdEUR}},
		},
	})
	// An unrelated, healthy priced pair this tick — it must still yield
	// edges even though the chain leg above is poisoned.
	o.tickEdgeQuotes = map[time.Duration][]aggregate.Quote{
		window: {
			{Pair: xlmGBP, Price: big.NewRat(5, 100), Confidence: 0.9},
			{Pair: gbpEUR, Price: big.NewRat(8, 10), Confidence: 0.9},
		},
	}

	edges, _ := o.buildWindowEdges(context.Background(), window, time.Now().UTC())
	if len(edges) == 0 {
		t.Fatal("buildWindowEdges returned no edges — one zero-valued leg collapsed the ENTIRE window (R-1)")
	}
	// The healthy xlmGBP↔gbpEUR edges must be present.
	var sawXLMGBP bool
	for _, e := range edges {
		if e.From.Equal(xlmGBP.Base) && e.To.Equal(xlmGBP.Quote) {
			sawXLMGBP = true
		}
	}
	if !sawXLMGBP {
		t.Error("healthy XLM/GBP edge missing from the graph after a poisoned leg — window not preserved")
	}
}

// metaFailCache wraps a Cache and forces Set on the composite_meta key to
// fail, so the R-2 test can prove the value is NOT overwritten when its
// quality flags cannot be persisted.
type metaFailCache struct {
	Cache
	failSubstr string
}

func (c metaFailCache) Set(ctx context.Context, key string, value any, ttl time.Duration) *redis.StatusCmd {
	if strings.Contains(key, c.failSubstr) {
		cmd := redis.NewStatusCmd(ctx)
		cmd.SetErr(redis.ErrClosed)
		return cmd
	}
	return c.Cache.Set(ctx, key, value, ttl)
}

// TestPublishComposite_DivergedNeverOverwritesDirectWithoutMeta is R-2: a
// diverged composite's quality flags are load-bearing. If the composite-
// meta write fails, publishComposite must REFUSE to overwrite the served
// direct price — otherwise a self-disagreeing (diverged) composite is
// served as a clean direct price with no warning flags.
func TestPublishComposite_DivergedNeverOverwritesDirectWithoutMeta(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdEUR := mkPair(t, "fiat", "USD", "fiat", "EUR")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := time.Minute
	const directPrice = "0.050000000000"

	inner, _ := newTestRedis(t)
	// Direct price already serving on the target's shared VWAP key.
	setLegVWAP(t, inner, xlmEUR, window, directPrice)
	cache := metaFailCache{Cache: inner, failSubstr: ":composite_meta"}

	o := New(&mockStore{}, cache, Config{Windows: []time.Duration{window}})
	chain := TriangulationChain{Target: xlmEUR, Legs: []canonical.Pair{xlmUSD, usdEUR}}

	// composite 0.072, DIVERGED — must not be served without its diverged flag.
	outcome := o.publishComposite(context.Background(), chain, window,
		big.NewRat(72, 1000), 2 /*pathCount*/, 2 /*corroboration*/, 0.9 /*conf*/, true /*diverged*/, false)

	if outcome == "ok" {
		t.Errorf("publishComposite returned %q — a diverged composite published despite its "+
			"quality-flags write failing (R-2)", outcome)
	}
	got, err := inner.Get(context.Background(),
		cachekeys.VWAP(xlmEUR.Base, xlmEUR.Quote, window).String()).Result()
	if err != nil {
		t.Fatalf("target VWAP read: %v", err)
	}
	if got != directPrice {
		t.Errorf("target VWAP = %q, want the untouched direct price %q — a diverged composite "+
			"overwrote the direct value while its diverged flag failed to persist (R-2)", got, directPrice)
	}
}

// TestRefreshPairWindow_DirectWriteClearsStaleProvenance is
// W1-flow-price-serve-2: when the direct per-pair refresh overwrites the
// shared VWAP key with a DIRECT value, it must clear any stale
// "triangulated" provenance a prior tick's composite left, so
// LookupTriangulatedVWAP cannot serve the thin direct price mislabeled as
// a robust composite.
func TestRefreshPairWindow_DirectWriteClearsStaleProvenance(t *testing.T) {
	store := &mockStore{
		trades: []canonical.Trade{
			buildTrade(t, big.NewInt(10_000_000_000), big.NewInt(1_758_200_000), time.Now().Add(-2*time.Minute)),
			buildTrade(t, big.NewInt(20_000_000_000), big.NewInt(3_518_000_000), time.Now().Add(-1*time.Minute)),
		},
	}
	rdb, mr := newTestRedis(t)
	window := 5 * time.Minute
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usdt, _ := canonical.NewCryptoAsset("USDT")

	// A PRIOR tick's composite left a stale triangulated provenance marker.
	provKey := cachekeys.VWAPProvenance(xlm, usdt, window).String()
	if err := rdb.Set(context.Background(), provKey, cachekeys.VWAPProvenanceTriangulated, window).Err(); err != nil {
		t.Fatalf("seed provenance: %v", err)
	}

	o := New(store, rdb, Config{Pairs: []canonical.Pair{xlmUsdtPair(t)}, Windows: []time.Duration{window}})
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// The direct value was published...
	if !mr.Exists("vwap:" + xlm.String() + ":" + usdt.String() + ":300") {
		t.Fatal("direct VWAP not published — test precondition broken")
	}
	// ...so the stale triangulated marker must be gone.
	if mr.Exists(provKey) {
		v, _ := mr.Get(provKey)
		t.Errorf("stale provenance marker survived a direct refresh (=%q) — a thin direct price "+
			"would be served flagged triangulated (W1-flow-price-serve-2)", v)
	}
}
