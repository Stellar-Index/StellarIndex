package orchestrator

import (
	"math"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// ratOf parses a decimal string into *big.Rat for test fixtures.
func ratOf(t *testing.T, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("parse rat %q", s)
	}
	return r
}

// makeTradeOn is buildTradeFrom for an arbitrary pair — the
// corroboration tests need trades on the TARGET of a chain, not on the
// package's fixed XLM/USDT fixture pair.
func makeTradeOn(t *testing.T, pair canonical.Pair, source string, base, quote int64, ts time.Time) canonical.Trade {
	t.Helper()
	return canonical.Trade{
		Source:      source,
		Ledger:      0,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000000",
		OpIndex:     0,
		Timestamp:   ts,
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(base)),
		QuoteAmount: canonical.NewAmount(big.NewInt(quote)),
	}
}

// TestTriangulationDivergencePct_UncheckedWithoutAComposite — the
// default state of the entire index: no chain configured, so the
// confidence step must report "unchecked" rather than a divergence of
// zero (which reads as perfect agreement).
func TestTriangulationDivergencePct_UncheckedWithoutAComposite(t *testing.T) {
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	o := New(nil, nil, Config{Windows: []time.Duration{5 * time.Minute}})

	if pct, ok := o.triangulationDivergencePct(xlmEUR, 5*time.Minute, ratOf(t, "0.072")); ok {
		t.Errorf("checked=true with no composite recorded (pct=%v)", pct)
	}
}

// TestTriangulationDivergencePct_MeasuresDirectAgainstComposite pins
// the number the confidence factor consumes: deviation measured
// AGAINST the composite (the route through the deep markets), matching
// divergence.Compare's orientation for the cross-oracle factor.
func TestTriangulationDivergencePct_MeasuresDirectAgainstComposite(t *testing.T) {
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := 5 * time.Minute
	o := New(nil, nil, Config{Windows: []time.Duration{window}})

	o.recordComposite(xlmEUR, window, ratOf(t, "0.0720"), 1, 0.8, false)

	// Direct 0.0756 vs composite 0.0720 → |0.0756-0.0720|/0.0720 = 5%.
	pct, ok := o.triangulationDivergencePct(xlmEUR, window, ratOf(t, "0.0756"))
	if !ok {
		t.Fatal("checked=false with a fresh composite recorded")
	}
	if math.Abs(pct-5.0) > 1e-9 {
		t.Errorf("divergence = %v%%, want 5%% (direct 0.0756 vs composite 0.0720)", pct)
	}

	// Symmetric in direction: a direct price BELOW the composite by the
	// same fraction reports the same magnitude.
	below, ok := o.triangulationDivergencePct(xlmEUR, window, ratOf(t, "0.0684"))
	if !ok || math.Abs(below-5.0) > 1e-9 {
		t.Errorf("divergence below composite = %v%% (checked=%v), want 5%%", below, ok)
	}
}

// TestTriangulationDivergencePct_StaleCompositeIsUnchecked — a chain
// that has STOPPED publishing must degrade to "unchecked", never to
// "disagrees". Otherwise a frozen or broken chain would drag a healthy
// pair's confidence down as the direct price walked away from a
// composite that stopped updating.
func TestTriangulationDivergencePct_StaleCompositeIsUnchecked(t *testing.T) {
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	window := 5 * time.Minute
	interval := 30 * time.Second
	o := New(nil, nil, Config{Windows: []time.Duration{window}, Interval: interval})

	o.recordComposite(xlmEUR, window, ratOf(t, "0.0720"), 1, 0.8, false)

	// One tick old: still trusted.
	o.lastComposites[compositeKey(xlmEUR, window)] = compositeSample{
		price: ratOf(t, "0.0720"),
		at:    time.Now().UTC().Add(-interval),
	}
	if _, ok := o.triangulationDivergencePct(xlmEUR, window, ratOf(t, "0.0756")); !ok {
		t.Error("a one-tick-old composite was rejected; the chain pass is one tick behind by construction")
	}

	// Past compositeMaxAgeTicks: unchecked.
	o.lastComposites[compositeKey(xlmEUR, window)] = compositeSample{
		price: ratOf(t, "0.0720"),
		at:    time.Now().UTC().Add(-(compositeMaxAgeTicks + 1) * interval),
	}
	if pct, ok := o.triangulationDivergencePct(xlmEUR, window, ratOf(t, "0.0756")); ok {
		t.Errorf("a stale composite was still trusted (pct=%v) — staleness must read as unchecked", pct)
	}
}
