package orchestrator

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// stalenessTestClock is a hand-advanced time source for the
// orchestrator's injectable clock.
type stalenessTestClock struct{ now time.Time }

func (c *stalenessTestClock) Now() time.Time { return c.now }

func mustStalenessPair(t *testing.T, base, quote canonical.Asset) canonical.Pair {
	t.Helper()
	p, err := canonical.NewPair(base, quote)
	if err != nil {
		t.Fatalf("NewPair(%s, %s): %v", base, quote, err)
	}
	return p
}

func mustCrypto(t *testing.T, code string) canonical.Asset {
	t.Helper()
	a, err := canonical.NewCryptoAsset(code)
	if err != nil {
		t.Fatalf("NewCryptoAsset(%s): %v", code, err)
	}
	return a
}

func mustFiat(t *testing.T, code string) canonical.Asset {
	t.Helper()
	a, err := canonical.NewFiatAsset(code)
	if err != nil {
		t.Fatalf("NewFiatAsset(%s): %v", code, err)
	}
	return a
}

// pairStaleness is the staleness gauge series for p.
func pairStaleness(p canonical.Pair) prometheus.Gauge {
	return obs.PriceStalenessSeconds.WithLabelValues(p.Base.String(), p.Quote.String())
}

// liveTrades returns two exchange-class trades on `pair` stamped at
// `ts` — enough for a VWAP publish through the real Tick path.
func liveTrades(pair canonical.Pair, ts time.Time) []canonical.Trade {
	mk := func(op uint32, base, quote int64) canonical.Trade {
		return canonical.Trade{
			Source:      "binance",
			TxHash:      "0000000000000000000000000000000000000000000000000000000000000000",
			OpIndex:     op,
			Timestamp:   ts,
			Pair:        pair,
			BaseAmount:  canonical.NewAmount(big.NewInt(base)),
			QuoteAmount: canonical.NewAmount(big.NewInt(quote)),
		}
	}
	return []canonical.Trade{mk(0, 100_000_000, 21_000_000), mk(1, 200_000_000, 42_000_000)}
}

// compositeStalenessHarness is the composite-served shape: BTC/USD
// publishes from direct trades on every Tick; BTC/EUR is a CONFIGURED
// pair with no direct trades at all, reachable only through the chain
// BTC/USD × USD/EUR. Whether the chain publishes is up to the caller
// (the FX store and the route-confidence floor).
//
// BTC, not XLM, so the native ↔ crypto:XLM merge is not in play.
type compositeStalenessHarness struct {
	o              *Orchestrator
	clk            *stalenessTestClock
	store          *mockStore
	direct, target canonical.Pair
}

// liveFXObservedAt is an FX quote an hour before the harness clock: well
// inside the snap's freshness budget.
var liveFXObservedAt = time.Date(2026, 3, 1, 11, 0, 0, 0, time.UTC)

func newCompositeStalenessHarness(t *testing.T, fx FXStore, minRouteConfidence float64) *compositeStalenessHarness {
	t.Helper()
	btc, usd, eur := mustCrypto(t, "BTC"), mustFiat(t, "USD"), mustFiat(t, "EUR")
	direct := mustStalenessPair(t, btc, usd)
	target := mustStalenessPair(t, btc, eur)
	fxLeg := mustStalenessPair(t, usd, eur)

	clk := &stalenessTestClock{now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	cache, _ := newTestRedis(t)
	store := &mockStore{perPair: map[string][]canonical.Trade{}}
	o := New(store, cache, Config{
		Pairs:              []canonical.Pair{direct, target},
		Windows:            []time.Duration{5 * time.Minute},
		Triangulations:     []TriangulationChain{{Target: target, Legs: []canonical.Pair{direct, fxLeg}}},
		FXStore:            fx,
		MinRouteConfidence: minRouteConfidence,
	})
	o.clock = clk.Now
	return &compositeStalenessHarness{o: o, clk: clk, store: store, direct: direct, target: target}
}

// tick publishes the direct pair, runs one real Tick, and returns how
// many times the chain ended in `outcome` during it.
func (h *compositeStalenessHarness) tick(t *testing.T, outcome string) float64 {
	t.Helper()
	h.store.perPair[h.direct.String()] = liveTrades(h.direct, h.clk.now)
	before := testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues(outcome))
	writes := h.o.Stats().VWAPWrites
	if err := h.o.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if h.o.Stats().VWAPWrites == writes {
		t.Fatalf("precondition: the direct leg %s did not publish — the chain has nothing to route through", h.direct)
	}
	return testutil.ToFloat64(obs.AggregatorTriangulationsTotal.WithLabelValues(outcome)) - before
}
