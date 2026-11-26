package orchestrator

import (
	"context"
	"fmt"
	"maps"
	"math/big"
	"math/rand"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/baseline"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// shuffleStore serves each pair's trades in a fresh seeded order on every read.
type shuffleStore struct {
	perPair map[string][]canonical.Trade
	rng     *rand.Rand
}

func (s *shuffleStore) TradesInRange(_ context.Context, p canonical.Pair, _, _ time.Time, _ int) ([]canonical.Trade, error) {
	out := slices.Clone(s.perPair[p.String()])
	s.rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	return out, nil
}

// writeLog records every cache write, in order, with its value and TTL.
type writeLog struct {
	*redis.Client
	ops []string
}

type writeLogPipe struct {
	redis.Pipeliner
	log *writeLog
}

func (p *writeLogPipe) Set(ctx context.Context, key string, value any, exp time.Duration) *redis.StatusCmd {
	p.log.ops = append(p.log.ops, fmt.Sprintf("tx set %s=%v ttl=%s", key, value, exp))
	return p.Pipeliner.Set(ctx, key, value, exp)
}

func (p *writeLogPipe) Del(ctx context.Context, keys ...string) *redis.IntCmd {
	p.log.ops = append(p.log.ops, "tx del "+strings.Join(keys, ","))
	return p.Pipeliner.Del(ctx, keys...)
}

func (p *writeLogPipe) Expire(ctx context.Context, key string, exp time.Duration) *redis.BoolCmd {
	p.log.ops = append(p.log.ops, fmt.Sprintf("tx expire %s ttl=%s", key, exp))
	return p.Pipeliner.Expire(ctx, key, exp)
}

func (l *writeLog) TxPipelined(ctx context.Context, fn func(redis.Pipeliner) error) ([]redis.Cmder, error) {
	l.ops = append(l.ops, "multi")
	return l.Client.TxPipelined(ctx, func(p redis.Pipeliner) error { return fn(&writeLogPipe{Pipeliner: p, log: l}) })
}

func (l *writeLog) Set(ctx context.Context, key string, value any, exp time.Duration) *redis.StatusCmd {
	l.ops = append(l.ops, fmt.Sprintf("set %s=%v ttl=%s", key, value, exp))
	return l.Client.Set(ctx, key, value, exp)
}

type contributionLog struct{ recs []string }

func (c *contributionLog) RecordContributions(_ context.Context, rec ContributionRecord) error {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s at=%s", rec.Pair, rec.Window, rec.ComputedAt.Format(time.RFC3339Nano))
	for _, sc := range rec.Contributions {
		fmt.Fprintf(&b, " %s:%s/%s/%s/%d", sc.Source, sc.Weight.RatString(), sc.BaseVolume, sc.QuoteVolume, sc.TradeCount)
	}
	for _, src := range slices.Sorted(maps.Keys(rec.SourceUSDVolume)) {
		fmt.Fprintf(&b, " usd[%s]=%s", src, rec.SourceUSDVolume[src].RatString())
	}
	c.recs = append(c.recs, b.String())
	return nil
}

func detOrchTrade(pair canonical.Pair, source string, n int, ts time.Time, base, quote *big.Int) canonical.Trade {
	return canonical.Trade{
		Source: source, Ledger: uint32(ts.Unix() % 1_000_000), TxHash: fmt.Sprintf("%064x", n), Timestamp: ts, Pair: pair,
		BaseAmount: canonical.NewAmount(base), QuoteAmount: canonical.NewAmount(quote),
	}
}

// detOrchWindow is one tick's trades for each pair: several venues on one ledger close, equal prices, and a leg print above 2^70.
// spike multiplies the leg's price so the second tick exercises the freeze path.
func detOrchWindow(pairs []canonical.Pair, ts time.Time, spike int64) map[string][]canonical.Trade {
	huge := new(big.Int).Lsh(big.NewInt(1), 70)
	n := 0
	mk := func(p canonical.Pair, src string, base, quote *big.Int) canonical.Trade {
		n++
		return detOrchTrade(p, src, n, ts, base, quote)
	}
	out := make(map[string][]canonical.Trade, len(pairs))
	for i, p := range pairs {
		m, venue := int64(1), func(s string) string { return s }
		if i == 0 && spike != 1 {
			// A single-venue move is what the anomaly checker freezes; a corroborated one only warns.
			m, venue = spike, func(string) string { return "soroswap" }
		}
		q := func(v int64) *big.Int { return big.NewInt(v * m) }
		out[p.String()] = []canonical.Trade{
			mk(p, venue("sdex"), big.NewInt(100_000_000), q(9_000_000)), mk(p, venue("sdex"), big.NewInt(100_000_000), q(9_000_000)),
			mk(p, venue("soroswap"), big.NewInt(50_000_000), q(4_510_000)), mk(p, venue("aquarius"), big.NewInt(70_000_000), q(6_290_000)),
		}
		if i == 0 {
			out[p.String()] = append(out[p.String()], mk(p, "soroswap", huge, new(big.Int).Mul(new(big.Int).Div(huge, big.NewInt(10)), big.NewInt(m))))
		}
	}
	return out
}

// runDeterminismScenario drives a fresh orchestrator through three ticks (healthy, spike, healthy) from seeded prior state, with the
// store re-shuffling every read, and returns everything it wrote.
func runDeterminismScenario(t *testing.T, seed int64) string {
	t.Helper()
	xlmUSD := mkPair(t, "crypto", "XLM", "fiat", "USD")
	xlmGBP := mkPair(t, "crypto", "XLM", "fiat", "GBP")
	xlmEUR := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	// CHF and JPY route through a BTC leg with no trades, so the triangulation pass settles them on their held direct values.
	xlmBTC := mkPair(t, "crypto", "XLM", "crypto", "BTC")
	xlmCHF := mkPair(t, "crypto", "XLM", "fiat", "CHF")
	xlmJPY := mkPair(t, "crypto", "XLM", "fiat", "JPY")
	traded := []canonical.Pair{xlmUSD, xlmGBP, xlmEUR, xlmCHF, xlmJPY}
	now := time.Date(2026, 11, 25, 12, 0, 30, 0, time.UTC)
	rdb, _ := newTestRedis(t)
	cache := &writeLog{Client: rdb}
	store := &shuffleStore{rng: rand.New(rand.NewSource(seed))} //nolint:gosec // deterministic test data
	marker := &recordingFreezeMarker{}
	sink := &contributionLog{}
	windows := []time.Duration{time.Minute, 5 * time.Minute}
	o := New(store, cache, Config{
		Pairs:    []canonical.Pair{xlmJPY, xlmGBP, xlmCHF, xlmEUR, xlmUSD, xlmBTC},
		Windows:  windows,
		Interval: time.Hour,
		Triangulations: []TriangulationChain{
			{Target: xlmGBP, Legs: []canonical.Pair{xlmUSD, mkPair(t, "fiat", "USD", "fiat", "GBP")}},
			{Target: xlmEUR, Legs: []canonical.Pair{xlmUSD, mkPair(t, "fiat", "USD", "fiat", "EUR")}},
			{Target: xlmCHF, Legs: []canonical.Pair{xlmBTC, mkPair(t, "crypto", "BTC", "fiat", "CHF")}},
			{Target: xlmJPY, Legs: []canonical.Pair{xlmBTC, mkPair(t, "crypto", "BTC", "fiat", "JPY")}},
		},
		FXStore:          &fakeFXStore{quote: big.NewRat(80, 100), observedAt: now.Add(-time.Hour), source: "massive"},
		Anomaly:          newAnomalyChecker(t, xlmUSD),
		FreezeWriter:     marker,
		ContributionSink: sink,
		Baselines: stubBaselineSource{
			multi:      baseline.MultiBaseline{Day30: &baseline.Baseline{Median: 0, MAD: 0.01, N: maxDay30Returns}},
			computedAt: now,
		},
	})
	o.clock = func() time.Time { return now }
	for _, p := range traded {
		for _, w := range windows {
			o.setComparator(p.String()+":"+w.String(), big.NewRat(9, 100), now.Add(-time.Minute), now.Add(-30*time.Second))
			if err := rdb.Set(context.Background(), cachekeys.VWAP(p.Base, p.Quote, w).String(), "0.09", time.Hour).Err(); err != nil {
				t.Fatalf("seed LKG: %v", err)
			}
		}
	}
	for i, spike := range []int64{1, 3, 1} {
		store.perPair = detOrchWindow(traded, now.Add(-40*time.Second), spike) // inside the last CLOSED minute
		if err := o.Tick(context.Background()); err != nil {
			t.Fatalf("seed %d tick %d: %v", seed, i, err)
		}
		now = now.Add(time.Minute)
	}
	var b strings.Builder
	b.WriteString(strings.Join(cache.ops, "\n"))
	b.WriteString("\n--contributions\n" + strings.Join(sink.recs, "\n"))
	fmt.Fprintf(&b, "\n--freeze marks=%+v clears=%d", marker.marks, marker.clears)
	return b.String()
}

// TestOrchestrator_SameInputsSameWrites pins that two fresh orchestrators given the same trades, clock and prior state write the
// same cache values, in the same order, and record the same contributions and freeze decisions, whatever order the store returns.
func TestOrchestrator_SameInputsSameWrites(t *testing.T) {
	want := runDeterminismScenario(t, 1)
	if !strings.Contains(want, "Action:freeze") || strings.Count(want, " at=") < 4 || strings.Count(want, "multi") < 4 {
		t.Fatalf("scenario wrote too little to be a check:\n%s", want)
	}
	for seed := int64(2); seed <= 12; seed++ {
		if got := runDeterminismScenario(t, seed); got != want {
			t.Fatalf("seed %d diverged from seed 1\n got:\n%s\nwant:\n%s", seed, got, want)
		}
	}
}
