package v1

import (
	"bytes"
	"context"
	"errors"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type fakeUpstream struct {
	statsCalls atomic.Int64
	histCalls  atomic.Int64
	statsDelay time.Duration
	statsErr   error
}

func (f *fakeUpstream) GetSourceStats(ctx context.Context) ([]timescale.SourceStats, error) {
	f.statsCalls.Add(1)
	if f.statsDelay > 0 {
		select {
		case <-time.After(f.statsDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.statsErr != nil {
		return nil, f.statsErr
	}
	return []timescale.SourceStats{{Source: "binance", TradeCount24h: 42}}, nil
}

func (f *fakeUpstream) GetSourceVolumeHistory24h(ctx context.Context) ([]timescale.SourceVolumeBucket, error) {
	f.histCalls.Add(1)
	return []timescale.SourceVolumeBucket{{Source: "binance", Hour: time.Now()}}, nil
}

func (f *fakeUpstream) GetSourceVolumeHistory7d(ctx context.Context) ([]timescale.SourceVolumeBucket, error) {
	f.histCalls.Add(1)
	return []timescale.SourceVolumeBucket{{Source: "binance", Hour: time.Now()}}, nil
}

// TestCachedSourcesStatsReader_HitsCachedValue — once warmed, the
// upstream must NOT be called again within the TTL window.
func TestCachedSourcesStatsReader_HitsCachedValue(t *testing.T) {
	up := &fakeUpstream{}
	c := NewCachedSourcesStatsReader(up, 60*time.Second)
	for i := 0; i < 5; i++ {
		_, err := c.GetSourceStats(context.Background())
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := up.statsCalls.Load(); got != 1 {
		t.Errorf("upstream called %d times; want 1 (4 cache hits expected)", got)
	}
}

// TestCachedSourcesStatsReader_RefetchesAfterTTL — after the TTL
// window the next call must hit upstream again.
func TestCachedSourcesStatsReader_RefetchesAfterTTL(t *testing.T) {
	up := &fakeUpstream{}
	c := NewCachedSourcesStatsReader(up, 50*time.Millisecond)
	if _, err := c.GetSourceStats(context.Background()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(70 * time.Millisecond)
	if _, err := c.GetSourceStats(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := up.statsCalls.Load(); got != 2 {
		t.Errorf("upstream called %d times; want 2 (one before + one after TTL)", got)
	}
}

// TestCachedSourcesStatsReader_SingleFlight — concurrent calls
// during a slow upstream refetch must share ONE upstream call.
// This is the property that protects the DB during a thundering-
// herd page load.
func TestCachedSourcesStatsReader_SingleFlight(t *testing.T) {
	up := &fakeUpstream{statsDelay: 100 * time.Millisecond}
	c := NewCachedSourcesStatsReader(up, 60*time.Second)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.GetSourceStats(context.Background()); err != nil {
				t.Errorf("call: %v", err)
			}
		}()
	}
	wg.Wait()

	if got := up.statsCalls.Load(); got != 1 {
		t.Errorf("upstream called %d times under single-flight; want 1", got)
	}
}

// TestCachedSourcesStatsReader_TTLZeroIsBypass — ttl=0 disables
// the cache entirely. Every call hits upstream. Useful for tests
// that want the wrapper inert.
func TestCachedSourcesStatsReader_TTLZeroIsBypass(t *testing.T) {
	up := &fakeUpstream{}
	c := NewCachedSourcesStatsReader(up, 0)
	for i := 0; i < 3; i++ {
		_, _ = c.GetSourceStats(context.Background())
	}
	if got := up.statsCalls.Load(); got != 3 {
		t.Errorf("upstream called %d times; want 3 (no caching at ttl=0)", got)
	}
}

// TestCachedSourcesStatsReader_ErrorIsNotCached — if upstream
// errors, the cache should NOT remember the error. Next caller
// retries.
func TestCachedSourcesStatsReader_ErrorIsNotCached(t *testing.T) {
	up := &fakeUpstream{statsErr: errors.New("db is down")}
	c := NewCachedSourcesStatsReader(up, 60*time.Second)

	if _, err := c.GetSourceStats(context.Background()); err == nil {
		t.Fatal("first call: want error, got nil")
	}
	up.statsErr = nil
	if _, err := c.GetSourceStats(context.Background()); err != nil {
		t.Fatalf("second call: %v", err)
	}
	// Second call must have hit upstream (error wasn't cached).
	if got := up.statsCalls.Load(); got != 2 {
		t.Errorf("upstream called %d times; want 2", got)
	}
}

// TestCachedSourcesStatsReader_WaitersSeeLeaderError — RLT-097: when
// the single-flight leader's upstream call errors, every goroutine
// that waited on it must get that error back too, not a fabricated
// cache "hit" of the stale/nil field. Before the fix, a waiter did
// `<-ch` and unconditionally returned (c.stats, nil).
func TestCachedSourcesStatsReader_WaitersSeeLeaderError(t *testing.T) {
	wantErr := errors.New("timescale: query failed")
	up := &fakeUpstream{statsDelay: 80 * time.Millisecond, statsErr: wantErr}
	c := NewCachedSourcesStatsReader(up, 60*time.Second)

	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_, err := c.GetSourceStats(context.Background())
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	if got := up.statsCalls.Load(); got != 1 {
		t.Fatalf("upstream called %d times under single-flight; want 1", got)
	}
	for i, err := range errs {
		if err == nil {
			t.Errorf("goroutine %d: got nil error; want the leader's upstream error surfaced", i)
		}
	}
}

// TestCachedSourcesStatsReader_HitMissCounter pins the
// stellarindex_api_cache_ops_total{cache="sources_stats"} counter
// for both ops on the wrapper. Same regression-guard rationale as
// the markets + assetsReader variants — if a future refactor drops the
// .Inc() on either branch, the cache-ops alert silently stops firing
// for these surfaces.
func TestCachedSourcesStatsReader_HitMissCounter(t *testing.T) {
	for _, tc := range []struct {
		op   string
		call func(c *CachedSourcesStatsReader) error
	}{
		{"source_stats", func(c *CachedSourcesStatsReader) error {
			_, err := c.GetSourceStats(context.Background())
			return err
		}},
		{"volume_history_24h", func(c *CachedSourcesStatsReader) error {
			_, err := c.GetSourceVolumeHistory24h(context.Background())
			return err
		}},
	} {
		t.Run(tc.op, func(t *testing.T) {
			up := &fakeUpstream{}
			c := NewCachedSourcesStatsReader(up, 60*time.Second)

			missBefore := readCacheCounter(t, "sources_stats", tc.op, "miss")
			hitBefore := readCacheCounter(t, "sources_stats", tc.op, "hit")

			if err := tc.call(c); err != nil {
				t.Fatal(err)
			}
			if err := tc.call(c); err != nil {
				t.Fatal(err)
			}

			if delta := readCacheCounter(t, "sources_stats", tc.op, "miss") - missBefore; delta != 1 {
				t.Errorf("miss counter delta = %v, want 1", delta)
			}
			if delta := readCacheCounter(t, "sources_stats", tc.op, "hit") - hitBefore; delta != 1 {
				t.Errorf("hit counter delta = %v, want 1", delta)
			}
		})
	}
}

// danglingPRRef is the RSWP-112 citation this file's HitMissCounter doc
// comment used to carry: no such PR exists, and the bare number now
// resolves to unrelated content. Built from parts so this guard's own
// source doesn't itself trip the check it performs.
var danglingPRRef = "#" + "1197"

// TestSourceStatsCacheTestFileHasNoDanglingPRReference guards RSWP-112:
// strip dangling citations instead of reintroducing them, and don't
// invent a replacement PR number we can't verify.
func TestSourceStatsCacheTestFileHasNoDanglingPRReference(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed to resolve this test file's path")
	}
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	if bytes.Contains(src, []byte(danglingPRRef)) {
		t.Errorf("this file cites PR %s, which does not exist and now resolves to unrelated content (RSWP-112)", danglingPRRef)
	}
}
