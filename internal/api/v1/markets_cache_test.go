package v1

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type fakeMarketsReader struct {
	allPoolsCalls atomic.Int64
	delay         time.Duration
	err           error
}

func (f *fakeMarketsReader) DistinctPairsExt(ctx context.Context, cursor string, limit int, order timescale.MarketsOrder) ([]Market, string, error) {
	return nil, "", nil
}

func (f *fakeMarketsReader) SourceMarkets(ctx context.Context, source, cursor string, limit int, order timescale.MarketsOrder) ([]Market, string, error) {
	return nil, "", nil
}

func (f *fakeMarketsReader) AssetMarkets(ctx context.Context, asset, cursor string, limit int, order timescale.MarketsOrder) ([]Market, string, error) {
	return nil, "", nil
}

func (f *fakeMarketsReader) AllPools(ctx context.Context, filter timescale.PoolsFilter, cursor string, limit int, order timescale.MarketsOrder) ([]Pool, string, error) {
	f.allPoolsCalls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return nil, "", ctx.Err()
		}
	}
	if f.err != nil {
		return nil, "", f.err
	}
	return []Pool{{Source: "aquarius", Base: "native"}}, "", nil
}

func (f *fakeMarketsReader) PairMarket(ctx context.Context, base, quote canonical.Asset) (Market, bool, error) {
	return Market{}, false, nil
}

func (f *fakeMarketsReader) GetPairsVolumeHistory24hBatch(ctx context.Context, pairs [][2]string) (map[string][]timescale.PairVolumePoint, error) {
	return nil, nil
}

// expireMarketsEntries walks every cached entry back past any ttl, so
// the next read takes the stale-while-revalidate branch. It replaces
// sleeping past a deliberately tiny ttl: the test below needs the entry
// a refresh WRITES to stay fresh while it asserts on it, which a 25 ms
// ttl cannot promise.
func expireMarketsEntries(c *CachedMarketsReader) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries {
		if !e.at.IsZero() {
			e.at = e.at.Add(-time.Hour)
		}
	}
}

func TestCachedMarketsReader_AllPoolsCachesByKey(t *testing.T) {
	up := &fakeMarketsReader{}
	c := NewCachedMarketsReader(up, 60*time.Second)
	filter := timescale.PoolsFilter{Sources: []string{"aquarius"}}

	for i := 0; i < 4; i++ {
		_, _, err := c.AllPools(context.Background(), filter, "", 50, timescale.MarketsOrderVolume24hDesc)
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := up.allPoolsCalls.Load(); got != 1 {
		t.Errorf("upstream called %d times for same key; want 1", got)
	}
}

func TestCachedMarketsReader_AllPoolsDifferentKeys(t *testing.T) {
	up := &fakeMarketsReader{}
	c := NewCachedMarketsReader(up, 60*time.Second)

	_, _, _ = c.AllPools(context.Background(),
		timescale.PoolsFilter{Sources: []string{"aquarius"}}, "", 50, timescale.MarketsOrderVolume24hDesc)
	_, _, _ = c.AllPools(context.Background(),
		timescale.PoolsFilter{Sources: []string{"phoenix"}}, "", 50, timescale.MarketsOrderVolume24hDesc)
	if got := up.allPoolsCalls.Load(); got != 2 {
		t.Errorf("upstream called %d times across 2 distinct keys; want 2", got)
	}
}

func TestCachedMarketsReader_AllPoolsSingleFlight(t *testing.T) {
	up := &fakeMarketsReader{delay: 100 * time.Millisecond}
	c := NewCachedMarketsReader(up, 60*time.Second)
	filter := timescale.PoolsFilter{Sources: []string{"aquarius"}}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _ = c.AllPools(context.Background(), filter, "", 50, timescale.MarketsOrderVolume24hDesc)
		}()
	}
	wg.Wait()

	if got := up.allPoolsCalls.Load(); got != 1 {
		t.Errorf("upstream called %d times under single-flight; want 1", got)
	}
}

func TestCachedMarketsReader_AllPoolsErrorIsNotCached(t *testing.T) {
	up := &fakeMarketsReader{err: errors.New("db down")}
	c := NewCachedMarketsReader(up, 60*time.Second)
	filter := timescale.PoolsFilter{Sources: []string{"aquarius"}}

	_, _, err := c.AllPools(context.Background(), filter, "", 50, timescale.MarketsOrderVolume24hDesc)
	if err == nil {
		t.Fatal("first call: want error")
	}
	up.err = nil
	_, _, err = c.AllPools(context.Background(), filter, "", 50, timescale.MarketsOrderVolume24hDesc)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got := up.allPoolsCalls.Load(); got != 2 {
		t.Errorf("upstream called %d times; want 2 (error wasn't cached)", got)
	}
}

// readCacheCounter pulls the current stellarindex_api_cache_ops_total
// value for one (cache, op, result) combination. Returns 0 when the
// label set hasn't been incremented yet (Prometheus auto-creates on
// first .Inc()). Lets the metric tests read absolute values without
// depending on test ordering.
func readCacheCounter(t *testing.T, cache, op, result string) float64 {
	t.Helper()
	m := &dto.Metric{}
	if err := obs.APICacheOpsTotal.WithLabelValues(cache, op, result).Write(m); err != nil {
		t.Fatalf("metric write: %v", err)
	}
	return m.Counter.GetValue()
}

// TestCachedMarketsReader_HitMissCounter pins the contract:
// AllPools' miss-on-first-call + hit-on-repeat-call increments the
// stellarindex_api_cache_ops_total counter on the right label set.
// Detection target: a future refactor that drops the metric inc on
// either branch. Three earlier session bugs (#1185 / #1194 / #1195)
// were prewarm-key drifts; this test guards the OBSERVABILITY of
// future drifts by ensuring the counter actually moves.
func TestCachedMarketsReader_HitMissCounter(t *testing.T) {
	up := &fakeMarketsReader{}
	c := NewCachedMarketsReader(up, 60*time.Second)
	filter := timescale.PoolsFilter{Sources: []string{"aquarius"}}

	// Counters are process-global; capture the baseline so we can
	// assert deltas instead of absolute values (other tests + parallel
	// runs increment the same counter).
	missBefore := readCacheCounter(t, "markets", "all_pools", "miss")
	hitBefore := readCacheCounter(t, "markets", "all_pools", "hit")

	// First call → miss (+1 miss, +0 hit).
	if _, _, err := c.AllPools(context.Background(), filter, "", 50, timescale.MarketsOrderVolume24hDesc); err != nil {
		t.Fatal(err)
	}
	// Second call → hit (+0 miss, +1 hit).
	if _, _, err := c.AllPools(context.Background(), filter, "", 50, timescale.MarketsOrderVolume24hDesc); err != nil {
		t.Fatal(err)
	}

	missDelta := readCacheCounter(t, "markets", "all_pools", "miss") - missBefore
	hitDelta := readCacheCounter(t, "markets", "all_pools", "hit") - hitBefore

	if missDelta != 1 {
		t.Errorf("miss counter delta = %v, want 1", missDelta)
	}
	if hitDelta != 1 {
		t.Errorf("hit counter delta = %v, want 1", hitDelta)
	}
}

// TestCachedMarketsReader_AllPoolsLeaderFailsWaitersDontPanic pins the
// regression for a runtime panic observed on r1 production
// (2026-05-10 15:36:20 UTC, GET /v1/markets):
//
//	panic: runtime error: invalid memory address or nil pointer
//	dereference
//	  …markets_cache.go: out := c.entries[key]
//	  …                  return out.pairs, out.cursor, nil
//
// Root cause: under single-flight, the leader's failing upstream call
// removed the entry from the map (we don't TTL-cache errors) BEFORE
// closing the flight chan. Waiters then woke and re-read
// c.entries[key], got nil, and derefed `out.pairs`.
//
// Fix: waiters hold a pointer to the SAME entry they joined on and
// read entry.err / entry.pairs there, surviving the leader's delete.
func TestCachedMarketsReader_AllPoolsLeaderFailsWaitersDontPanic(t *testing.T) {
	up := &fakeMarketsReader{
		delay: 100 * time.Millisecond,
		err:   errors.New("simulated db down"),
	}
	c := NewCachedMarketsReader(up, 60*time.Second)
	filter := timescale.PoolsFilter{Sources: []string{"aquarius"}}

	// Fire the leader plus 9 waiters concurrently. With the bug
	// present, at least one waiter would panic on out.pairs deref.
	var wg sync.WaitGroup
	results := make(chan error, 10)
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					results <- errors.New("panic: " + toString(r))
				}
			}()
			_, _, err := c.AllPools(context.Background(), filter, "", 50, timescale.MarketsOrderVolume24hDesc)
			results <- err
		}()
	}
	wg.Wait()
	close(results)

	gotErrs := 0
	for err := range results {
		if err == nil {
			t.Errorf("want error from every caller, got nil")
			continue
		}
		if err.Error() == "simulated db down" || err.Error() == `panic: not allowed` {
			gotErrs++
			continue
		}
		// Anything else (especially "panic: ...") is a regression.
		if len(err.Error()) >= 6 && err.Error()[:6] == "panic:" {
			t.Errorf("waiter panicked: %v", err)
			continue
		}
		// Wrapped errors are fine as long as they aren't panics.
		gotErrs++
	}
	if gotErrs == 0 {
		t.Fatal("no callers returned an error; want all 10")
	}
	if got := up.allPoolsCalls.Load(); got != 1 {
		t.Errorf("upstream called %d times under single-flight; want 1", got)
	}
}

// toString renders a recovered panic value for the regression test
// above. Avoids a fmt import in the production path.
func toString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if e, ok := v.(error); ok {
		return e.Error()
	}
	return "unknown"
}

// swrPoolsStaleTrades / swrPoolsFreshTrades are what tell the pool row
// a cold fetch cached from the row a refresh replaces it with: the
// stub moves the pool's 24h trade count on every call after the first,
// which is what a re-scan of the trades window returning later data
// looks like. The stub used to return one fixed row, identical before
// and after a refresh, so nothing but a stopwatch separated a caller
// served the cached entry from one handed the refresh's result.
const (
	swrPoolsStaleTrades = 100
	swrPoolsFreshTrades = 200
)

// swrPoolsUpstream is a race-safe configurable AllPools stub for the
// #23 stale-while-revalidate tests: atomic call counter, an optional
// hold that parks a call until the test releases it, and an atomic
// "fail on call >= 2" toggle (deterministic by call number → no
// mid-test field mutation, so `go test -race` is clean even under the
// concurrent background refresh). Embeds *fakeMarketsReader for the
// other interface methods.
type swrPoolsUpstream struct {
	*fakeMarketsReader
	calls   atomic.Int64
	hold    *upstreamHold
	failGE2 atomic.Bool
}

func (s *swrPoolsUpstream) AllPools(ctx context.Context, _ timescale.PoolsFilter, _ string, _ int, _ timescale.MarketsOrder) ([]Pool, string, error) {
	n := s.calls.Add(1)
	if err := s.hold.park(ctx, n); err != nil {
		return nil, "", err
	}
	if n >= 2 && s.failGE2.Load() {
		return nil, "", errors.New("swr pools boom")
	}
	trades := int64(swrPoolsStaleTrades)
	if n >= 2 {
		trades = swrPoolsFreshTrades
	}
	return []Pool{{Source: "aquarius", Base: "native", TradeCount24h: trades}}, "c", nil
}

// TestCachedMarketsReader_PoolsSWRServesStaleSingleFlight: every one
// of 20 concurrent reads of an expired pools entry is served THE STALE
// ROW while exactly one single-flighted background refresh runs (the
// #23 fix), and the row that refresh returns is what replaces it.
//
// What a stale read is served is a value, so it is asserted as one.
// The stub used to return one fixed row, so the row before the refresh
// and the row after it were the same row and no assertion here could
// tell a read served the entry from a read handed the refresh's
// result — the 120 ms ceiling on each reader was the whole evidence,
// and what it measured was how quickly the runner rescheduled 20
// goroutines. The stub now moves the pool's 24h trade count on the
// refresh, and the refresh is HELD until every read has returned, so a
// read that waited for it could only be holding the refreshed count
// and a read served the entry can only be holding the stale one — on
// any runner at any load.
//
// The tail releases the hold and pins the refreshed row, which is what
// keeps the assertion above a discrimination rather than a restatement
// of the fixture.
func TestCachedMarketsReader_PoolsSWRServesStaleSingleFlight(t *testing.T) {
	hold := newUpstreamHold(2) // the cold leader runs free; the refresh is parked
	t.Cleanup(hold.release)
	up := &swrPoolsUpstream{fakeMarketsReader: &fakeMarketsReader{}, hold: hold}
	c := NewCachedMarketsReader(up, time.Minute)
	f := timescale.PoolsFilter{Sources: []string{"aquarius"}}

	cold, _, err := c.AllPools(context.Background(), f, "", 50, timescale.MarketsOrderVolume24hDesc)
	if err != nil || len(cold) != 1 || cold[0].TradeCount24h != swrPoolsStaleTrades {
		t.Fatalf("cold leader: rows=%d err=%v, want 1 row at %d trades", len(cold), err, swrPoolsStaleTrades)
	}
	if up.calls.Load() != 1 {
		t.Fatalf("cold calls=%d, want 1", up.calls.Load())
	}
	expireMarketsEntries(c)

	// The reads report back on a buffered channel rather than failing
	// from their own goroutines: a caller blocked on the refresh is a
	// failure this test has to name, and naming it ends the test while
	// its siblings are still running.
	const reads = 20
	type staleRead struct {
		rows   int
		trades int64
		err    error
	}
	served := make(chan staleRead, reads)
	for i := 0; i < reads; i++ {
		go func() {
			rows, _, err := c.AllPools(context.Background(), f, "", 50, timescale.MarketsOrderVolume24hDesc)
			r := staleRead{rows: len(rows), err: err}
			if len(rows) > 0 {
				r.trades = rows[0].TradeCount24h
			}
			served <- r
		}()
	}
	for i := 0; i < reads; i++ {
		select {
		case got := <-served:
			if got.err != nil || got.rows != 1 {
				t.Fatalf("stale read %d: rows=%d err=%v, want 1 row", i, got.rows, got.err)
			}
			if got.trades != swrPoolsStaleTrades {
				t.Fatalf("stale read %d was served %d trades, want the stale %d — it waited for the refresh instead of being served the entry that was already there",
					i, got.trades, swrPoolsStaleTrades)
			}
		case <-time.After(blockedCallerBailout):
			t.Fatalf("only %d of %d stale reads returned; the rest are still waiting on a refresh that has not finished", i, reads)
		}
	}

	hold.awaitCall(t, "the background refresh")
	if n := hold.completed.Load(); n != 0 {
		t.Fatalf("%d held refreshes had already finished; the reads above were not ordered against a refresh in flight", n)
	}
	if got := up.calls.Load(); got != 2 {
		t.Fatalf("single-flight violated: %d upstream calls for %d concurrent stale reads, want 2 (1 cold + 1 refresh)", got, reads)
	}

	// Released, the one refresh settles the entry. Polling by READ adds
	// no upstream call of its own: a stale read looks at the in-flight
	// marker, which the refresh holds until it has written what it
	// found, so every poll before that point is served the stale entry
	// and the poll that succeeds is served the refreshed one. Because
	// the ttl outlives the test, the entry then stays fresh and the
	// call count is final.
	hold.release()
	var fresh []Pool
	var freshErr error
	if !waitFor(2*time.Second, func() bool {
		fresh, _, freshErr = c.AllPools(context.Background(), f, "", 50, timescale.MarketsOrderVolume24hDesc)
		return freshErr == nil && len(fresh) == 1 && fresh[0].TradeCount24h == swrPoolsFreshTrades
	}) {
		t.Fatalf("after the refresh: rows=%d err=%v, want 1 row at the refreshed %d trades", len(fresh), freshErr, swrPoolsFreshTrades)
	}
	// Nothing further may reach the upstream — see upstreamSettleWindow
	// for why an absence gets a window and why that window fails
	// nothing correct.
	if waitFor(upstreamSettleWindow, func() bool { return up.calls.Load() != 2 }) {
		t.Fatalf("single-flight violated: %d upstream calls once the refresh had settled, want 2 (1 cold + 1 refresh)", up.calls.Load())
	}
}

// TestCachedMarketsReader_PoolsSWRKeepsStaleOnError: a failing
// background refresh keeps serving stale (never an error, never a
// block) and is retried on the next expired request.
func TestCachedMarketsReader_PoolsSWRKeepsStaleOnError(t *testing.T) {
	up := &swrPoolsUpstream{fakeMarketsReader: &fakeMarketsReader{}}
	up.failGE2.Store(true) // call 1 (cold) OK; call >=2 (refresh) errors
	c := NewCachedMarketsReader(up, 20*time.Millisecond)
	f := timescale.PoolsFilter{Sources: []string{"aquarius"}}

	if _, _, err := c.AllPools(context.Background(), f, "", 50, timescale.MarketsOrderVolume24hDesc); err != nil {
		t.Fatal(err) // cold OK, calls=1
	}
	time.Sleep(40 * time.Millisecond) // expire

	rows, _, err := c.AllPools(context.Background(), f, "", 50, timescale.MarketsOrderVolume24hDesc)
	if err != nil || len(rows) == 0 {
		t.Fatalf("stale-with-failing-refresh must serve stale rows no err; got %d rows err=%v", len(rows), err)
	}
	if !waitFor(2*time.Second, func() bool { return up.calls.Load() == 2 }) {
		t.Fatalf("refresh not attempted; calls=%d want 2", up.calls.Load())
	}

	time.Sleep(40 * time.Millisecond) // re-expire
	rows2, _, err2 := c.AllPools(context.Background(), f, "", 50, timescale.MarketsOrderVolume24hDesc)
	if err2 != nil || len(rows2) == 0 {
		t.Fatalf("after a failed refresh, still serve stale no err; got %d rows err=%v", len(rows2), err2)
	}
	if !waitFor(2*time.Second, func() bool { return up.calls.Load() >= 3 }) {
		t.Fatalf("failed refresh was not retried; calls=%d", up.calls.Load())
	}
}

func (f *fakeMarketsReader) FirstTradeBatch(_ context.Context, _ [][2]string) (map[string]time.Time, error) {
	return map[string]time.Time{}, nil
}
