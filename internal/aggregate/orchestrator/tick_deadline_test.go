package orchestrator

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// wedgedStore is a Store whose trades query for one pair stops
// answering, the way a query on a half-open connection does: it returns
// only when its context ends. Every other pair is served normally.
type wedgedStore struct {
	*mockStore
	mu     sync.Mutex
	wedged string // pair.String(); "" = healed
}

func (s *wedgedStore) setWedged(pair string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.wedged = pair
}

func (s *wedgedStore) TradesInRange(ctx context.Context, p canonical.Pair, from, to time.Time, limit int) ([]canonical.Trade, error) {
	s.mu.Lock()
	wedged := s.wedged
	s.mu.Unlock()
	if p.String() == wedged {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return s.mockStore.TradesInRange(ctx, p, from, to, limit)
}

// TestTick_WedgedStoreCallIsCutAndTheNextTickRecovers is K025's
// aggregator leg. Tick ran on the process-lifetime context with no
// deadline of its own, so one store call that stopped answering held
// the tick — and with it every price the aggregator publishes — until
// the process was restarted.
//
// No TickTimeout is set: the bound under test is the DEFAULT one, so
// this proves a production-shaped Config is protected, and the test
// compiles against code that predates the field.
func TestTick_WedgedStoreCallIsCutAndTheNextTickRecovers(t *testing.T) {
	btc, usd := mustCrypto(t, "BTC"), mustFiat(t, "USD")
	pair := mustStalenessPair(t, btc, usd)

	store := &wedgedStore{mockStore: &mockStore{perPair: map[string][]canonical.Trade{}}}
	store.setWedged(pair.String())
	cache, _ := newTestRedis(t)
	o := New(store, cache, Config{
		Pairs:    []canonical.Pair{pair},
		Windows:  []time.Duration{5 * time.Minute},
		Interval: 25 * time.Millisecond, // default wedge guard = 4 × this
	})

	// The caller's context stays live throughout; it is cancelled only
	// on the way out, to release the goroutine if the tick never returns.
	parent, stop := context.WithCancel(context.Background())
	defer stop()

	obs.PriceStalenessSeconds.WithLabelValues("crypto:BTC").Set(-1)
	errTicksBefore := testutil.ToFloat64(obs.AggregatorTicksTotal.WithLabelValues("error"))
	done := make(chan error, 1)
	go func() { done <- o.Tick(parent) }()

	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Tick is still blocked on a store call that stopped answering — nothing bounds the tick, " +
			"so one wedged query stalls every price until the process is restarted")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Tick error = %v, want one wrapping context.DeadlineExceeded (the tick's own wedge guard)", err)
	}
	if parent.Err() != nil {
		t.Fatal("precondition: the caller's context must still be live — the tick has to be cut by its own deadline")
	}
	// A cut tick is an ERROR tick and still does its accounting: the
	// staleness gauge is emitted, not left at whatever it last read.
	if d := testutil.ToFloat64(obs.AggregatorTicksTotal.WithLabelValues("error")) - errTicksBefore; d != 1 {
		t.Errorf("ticks_total{outcome=error} delta = %v, want 1", d)
	}
	if got := testutil.ToFloat64(obs.PriceStalenessSeconds.WithLabelValues("crypto:BTC")); got == -1 {
		t.Error("the cut tick did not emit the staleness gauge — a wedge would freeze it at its last reading")
	}

	// The store heals. The next tick has a fresh budget and publishes.
	store.setWedged("")
	store.perPair[pair.String()] = liveTrades(pair, time.Now().UTC())
	writes := o.Stats().VWAPWrites
	if err := o.Tick(parent); err != nil {
		t.Fatalf("Tick after the store healed: %v", err)
	}
	if o.Stats().VWAPWrites == writes {
		t.Error("the tick after the wedge cleared published nothing — the guard must not outlive the tick it cut")
	}
}

// TestTick_CallerCancellationIsNotATimeout — shutdown is unchanged: the
// caller's cancellation still ends the tick at once with
// context.Canceled (which Run deliberately does not log), and is not
// dressed up as a wedge.
func TestTick_CallerCancellationIsNotATimeout(t *testing.T) {
	pair := mustStalenessPair(t, mustCrypto(t, "BTC"), mustFiat(t, "USD"))
	cache, _ := newTestRedis(t)
	o := New(&mockStore{perPair: map[string][]canonical.Trade{}}, cache, Config{
		Pairs: []canonical.Pair{pair}, Windows: []time.Duration{5 * time.Minute},
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := o.Tick(ctx)
	if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Tick on a cancelled caller context = %v, want context.Canceled and not a deadline", err)
	}
}

// fxCallRecord is what one FXQuoteAtOrBefore call looked like.
type fxCallRecord struct {
	cutoff    time.Time
	bounded   bool
	remaining time.Duration
}

// deadlineRecordingFX answers every FX query and records whether the
// context it was given carried a deadline.
type deadlineRecordingFX struct {
	mu         sync.Mutex
	observedAt time.Time
	calls      []fxCallRecord
}

func (f *deadlineRecordingFX) FXQuoteAtOrBefore(ctx context.Context, _ canonical.Pair, cutoff time.Time, _ []string) (*big.Rat, time.Time, string, error) {
	dl, ok := ctx.Deadline()
	f.mu.Lock()
	f.calls = append(f.calls, fxCallRecord{cutoff: cutoff, bounded: ok, remaining: time.Until(dl)})
	f.mu.Unlock()
	return big.NewRat(80, 100), f.observedAt, "massive", nil
}

// TestTick_EveryFXQueryInheritsTheTickDeadline pins the two FX query
// sites K025 names — the triangulation leg (triangulate.go legPrice) and
// the composite-reference evaluator (composite_reference.go) — to the
// tick's deadline. Neither sets one of its own; both are bounded only
// because Tick hands them a bounded context, so this is the test that
// fails if either is ever given a fresh one.
//
// The scenario is the market-wide move from the composite-reference
// suite: a single-venue XLM/GBP print jumps on tick 2, which is what
// sends the phase-2 freeze to consult the composite reference and so
// reach its FX query. The two sites are told apart by their cutoff: the
// triangulation leg asks at the window-aligned bucket end, the
// evaluator at the tick's own unaligned `now`.
func TestTick_EveryFXQueryInheritsTheTickDeadline(t *testing.T) {
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	usdGBP := mkPair(t, "fiat", "USD", "fiat", "GBP")
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP")
	window := time.Minute
	now := time.Now().UTC()
	// The DEFAULT wedge guard for the 1h Interval below (4 × Interval),
	// written as a literal so this file compiles against code that
	// predates the guard and fails there on behaviour, not on a symbol.
	const budget = 4 * time.Hour

	store := &mockStore{perPair: map[string][]canonical.Trade{}}
	cache, _ := newTestRedis(t)
	fx := &deadlineRecordingFX{observedAt: now.Add(-time.Hour)}
	o := New(store, cache, Config{
		Pairs:          []canonical.Pair{xlmGBP, xlmUSD},
		Windows:        []time.Duration{window},
		Interval:       time.Hour,
		Triangulations: []TriangulationChain{{Target: xlmGBP, Legs: []canonical.Pair{xlmUSD, usdGBP}}},
		FXStore:        fx,
		FreezeWriter:   &recordingFreezeMarker{},
		Baselines: stubBaselineSource{
			multi:      baseline.MultiBaseline{Day30: &baseline.Baseline{Median: 0, MAD: 0.01, N: 100_000}},
			computedAt: now,
		},
		CompositeReference: CompositeReferenceConfig{Enabled: true, Targets: []canonical.Pair{xlmGBP}},
	})
	setTrades := func(legQuote, targetQuote int64, ts time.Time) {
		store.perPair[xlmUSD.String()] = []canonical.Trade{
			makeTradeOn(t, xlmUSD, "kraken", 100_000_000, legQuote, ts),
			makeTradeOn(t, xlmUSD, "coinbase", 100_000_000, legQuote, ts),
		}
		store.perPair[xlmGBP.String()] = []canonical.Trade{
			makeTradeOn(t, xlmGBP, "soroswap", 100_000_000, targetQuote, ts),
		}
	}

	setTrades(10_000_000, 8_000_000, now.Add(-30*time.Second))
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	setTrades(15_000_000, 12_000_000, now.Add(-10*time.Second))
	if err := o.Tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}

	fx.mu.Lock()
	defer fx.mu.Unlock()
	var fromTriangulation, fromEvaluator int
	for i, c := range fx.calls {
		if c.cutoff.Equal(c.cutoff.Truncate(window)) {
			fromTriangulation++
		} else {
			fromEvaluator++
		}
		if !c.bounded {
			t.Errorf("FX query %d (cutoff %s) ran with NO deadline — it can hang the tick forever", i, c.cutoff)
			continue
		}
		if c.remaining <= 0 || c.remaining > budget {
			t.Errorf("FX query %d had %v left, want within the tick's %v budget", i, c.remaining, budget)
		}
	}
	if fromTriangulation == 0 {
		t.Error("precondition: the triangulation leg never queried the FX store — that site is unproven")
	}
	if fromEvaluator == 0 {
		t.Error("precondition: the composite-reference evaluator never queried the FX store — that site is unproven")
	}
}
