package v1

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeAssetsUpstream stubs every AssetsReader method with a per-call
// counter so tests can assert how many times the underlying call
// reached upstream vs. served from cache.
type fakeAssetsUpstream struct {
	listCalls    atomic.Int64
	hist24hCalls atomic.Int64
	hist7dCalls  atomic.Int64
}

func (f *fakeAssetsUpstream) ListAssetsExt(ctx context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	f.listCalls.Add(1)
	return []timescale.AssetRow{{AssetID: "native"}}, nil
}

func (f *fakeAssetsUpstream) GetAssetsPriceHistory24hBatch(ctx context.Context, ids []string) (map[string][]timescale.AssetPricePoint, error) {
	f.hist24hCalls.Add(1)
	return map[string][]timescale.AssetPricePoint{"native": nil}, nil
}

func (f *fakeAssetsUpstream) GetAssetsPriceHistory7dBatch(ctx context.Context, ids []string) (map[string][]timescale.AssetPricePoint, error) {
	f.hist7dCalls.Add(1)
	return map[string][]timescale.AssetPricePoint{"native": nil}, nil
}

func (f *fakeAssetsUpstream) GetAssetsATHBatch(ctx context.Context, ids []string) (map[string]timescale.AssetATH, error) {
	return nil, nil
}

func (f *fakeAssetsUpstream) GetAssetBySlug(ctx context.Context, slug string) (timescale.AssetRow, error) {
	return timescale.AssetRow{AssetID: slug}, nil
}

func (f *fakeAssetsUpstream) GetAssetByAssetID(ctx context.Context, assetID string) (timescale.AssetRow, error) {
	return timescale.AssetRow{AssetID: assetID}, nil
}

func (f *fakeAssetsUpstream) GetNativeAssetRow(ctx context.Context) (timescale.AssetRow, error) {
	return timescale.AssetRow{AssetID: "native"}, nil
}

func (f *fakeAssetsUpstream) GetAssetTopMarkets(ctx context.Context, assetID string, limit int) ([]timescale.AssetTopMarket, error) {
	return nil, nil
}

func (f *fakeAssetsUpstream) GetAssetPriceHistory24h(ctx context.Context, assetID string) ([]timescale.AssetPricePoint, error) {
	return nil, nil
}

func (f *fakeAssetsUpstream) GetAssetPriceHistory7d(ctx context.Context, assetID string) ([]timescale.AssetPricePoint, error) {
	return nil, nil
}

func (f *fakeAssetsUpstream) GetAssetMarketsCount(ctx context.Context, assetID string) (int64, error) {
	return 0, nil
}

func (f *fakeAssetsUpstream) GetAssetATH(ctx context.Context, assetID string) (*timescale.AssetATH, error) {
	return nil, nil
}

func (f *fakeAssetsUpstream) GetAssetTradeCount24h(ctx context.Context, assetID string) (int64, error) {
	return 0, nil
}

// TestCachedAssetsReader_ListAssetsExtHitMissCounter pins both the
// upstream-call dedup behaviour AND the
// stellarindex_api_cache_ops_total{cache="coins",op="list_coins"}
// counter deltas in one test. First call is a miss (+1 miss), the
// repeat call is a hit (+1 hit) and must NOT call upstream a
// second time.
func TestCachedAssetsReader_ListAssetsExtHitMissCounter(t *testing.T) {
	up := &fakeAssetsUpstream{}
	c := NewCachedAssetsReader(up, 60*time.Second)
	opts := timescale.ListAssetsOptions{Limit: 50}

	missBefore := readCacheCounter(t, "coins", "list_coins", "miss")
	hitBefore := readCacheCounter(t, "coins", "list_coins", "hit")

	if _, err := c.ListAssetsExt(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListAssetsExt(context.Background(), opts); err != nil {
		t.Fatal(err)
	}

	if got := up.listCalls.Load(); got != 1 {
		t.Errorf("upstream called %d times for same key; want 1", got)
	}
	if delta := readCacheCounter(t, "coins", "list_coins", "miss") - missBefore; delta != 1 {
		t.Errorf("miss counter delta = %v, want 1", delta)
	}
	if delta := readCacheCounter(t, "coins", "list_coins", "hit") - hitBefore; delta != 1 {
		t.Errorf("hit counter delta = %v, want 1", delta)
	}
}

// TestCachedAssetsReader_HistoryBatchHitMissCounter exercises both
// the 24h and 7d history-batch caches in one go. They share the
// generic fetchHistoryMap helper; this test catches a regression
// where one branch (24h vs 7d) silently drops the metric inc.
func TestCachedAssetsReader_HistoryBatchHitMissCounter(t *testing.T) {
	up := &fakeAssetsUpstream{}
	c := NewCachedAssetsReader(up, 60*time.Second)
	ids := []string{"native"}

	for _, tc := range []struct {
		op   string
		call func() error
	}{
		{"price_history_24h", func() error {
			_, err := c.GetAssetsPriceHistory24hBatch(context.Background(), ids)
			return err
		}},
		{"price_history_7d", func() error {
			_, err := c.GetAssetsPriceHistory7dBatch(context.Background(), ids)
			return err
		}},
	} {
		t.Run(tc.op, func(t *testing.T) {
			missBefore := readCacheCounter(t, "coins", tc.op, "miss")
			hitBefore := readCacheCounter(t, "coins", tc.op, "hit")

			if err := tc.call(); err != nil {
				t.Fatal(err)
			}
			if err := tc.call(); err != nil {
				t.Fatal(err)
			}
			if delta := readCacheCounter(t, "coins", tc.op, "miss") - missBefore; delta != 1 {
				t.Errorf("miss counter delta = %v, want 1", delta)
			}
			if delta := readCacheCounter(t, "coins", tc.op, "hit") - hitBefore; delta != 1 {
				t.Errorf("hit counter delta = %v, want 1", delta)
			}
		})
	}
}

// TestCachedAssetsReader_TTLZeroBypasses pins that ttl=0 disables
// the cache entirely (every call goes through to upstream + no
// metric counted, since the pre-cache code path doesn't hit the
// fetch helper).
func TestCachedAssetsReader_TTLZeroBypasses(t *testing.T) {
	up := &fakeAssetsUpstream{}
	c := NewCachedAssetsReader(up, 0)

	for i := 0; i < 3; i++ {
		if _, err := c.ListAssetsExt(context.Background(), timescale.ListAssetsOptions{Limit: 50}); err != nil {
			t.Fatal(err)
		}
	}
	if got := up.listCalls.Load(); got != 3 {
		t.Errorf("upstream called %d times with ttl=0; want 3 (cache must be bypassed)", got)
	}
}

// codeEchoUpstream tags each ListAssetsExt result with opts.Code so a
// cache-key collision (Code omitted from the key) surfaces as a wrong
// returned value. Embeds *fakeAssetsUpstream for the other methods.
type codeEchoUpstream struct {
	*fakeAssetsUpstream
	calls atomic.Int64
}

func (u *codeEchoUpstream) ListAssetsExt(_ context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	u.calls.Add(1)
	return []timescale.AssetRow{{AssetID: "code=" + opts.Code}}, nil
}

// TestCachedAssetsReader_CodeInCacheKey pins that the Code filter is
// part of the ListAssetsExt cache key (BACKLOG #54 lockstep): two
// requests differing only by Code must NOT collide — each gets its own
// upstream call + its own rows, and a repeat of the same Code hits
// the cache.
func TestCachedAssetsReader_CodeInCacheKey(t *testing.T) {
	up := &codeEchoUpstream{fakeAssetsUpstream: &fakeAssetsUpstream{}}
	c := NewCachedAssetsReader(up, 60*time.Second)

	a, err := c.ListAssetsExt(context.Background(), timescale.ListAssetsOptions{Limit: 50, Code: "AAA"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.ListAssetsExt(context.Background(), timescale.ListAssetsOptions{Limit: 50, Code: "BBB"})
	if err != nil {
		t.Fatal(err)
	}
	if a[0].AssetID != "code=AAA" || b[0].AssetID != "code=BBB" {
		t.Fatalf("Code missing from cache key — got %q then %q (collision)", a[0].AssetID, b[0].AssetID)
	}
	if got := up.calls.Load(); got != 2 {
		t.Errorf("distinct Code values must miss separately; upstream calls=%d want 2", got)
	}
	// Same Code repeated → served from cache (no 3rd upstream call).
	if _, err := c.ListAssetsExt(context.Background(), timescale.ListAssetsOptions{Limit: 50, Code: "AAA"}); err != nil {
		t.Fatal(err)
	}
	if got := up.calls.Load(); got != 2 {
		t.Errorf("repeat of Code=AAA must hit cache; upstream calls=%d want 2", got)
	}
}

// swrAssetsUpstream is a configurable ListAssetsExt stub for the
// stale-while-revalidate tests: a per-call counter plus a settable
// delay / return value / failure. Embeds fakeAssetsUpstream so the
// other AssetsReader methods are satisfied.
type swrAssetsUpstream struct {
	*fakeAssetsUpstream
	calls atomic.Int64
	mu    sync.Mutex
	delay time.Duration
	val   string
	fail  bool
}

func (s *swrAssetsUpstream) ListAssetsExt(ctx context.Context, _ timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	s.calls.Add(1)
	s.mu.Lock()
	d, v, f := s.delay, s.val, s.fail
	s.mu.Unlock()
	if d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f {
		return nil, errors.New("swr upstream boom")
	}
	return []timescale.AssetRow{{AssetID: v}}, nil
}

func (s *swrAssetsUpstream) set(delay time.Duration, val string, fail bool) {
	s.mu.Lock()
	s.delay, s.val, s.fail = delay, val, fail
	s.mu.Unlock()
}

func swrGet(t *testing.T, c *CachedAssetsReader) (string, error) {
	t.Helper()
	rows, err := c.ListAssetsExt(context.Background(), timescale.ListAssetsOptions{Limit: 7})
	if err != nil {
		return "", err
	}
	if len(rows) == 0 {
		return "", nil
	}
	return rows[0].AssetID, nil
}

func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// TestCachedAssetsReader_SWRServesStaleAndRefreshes: an expired entry
// returns the stale value IMMEDIATELY (not blocked on the slow
// upstream refetch — the #22 fix), a single background refresh runs,
// and afterwards the fresh value is served.
func TestCachedAssetsReader_SWRServesStaleAndRefreshes(t *testing.T) {
	up := &swrAssetsUpstream{fakeAssetsUpstream: &fakeAssetsUpstream{}, val: "v1"}
	c := NewCachedAssetsReader(up, 25*time.Millisecond)

	if v, err := swrGet(t, c); err != nil || v != "v1" {
		t.Fatalf("cold fetch: got %q err=%v, want v1", v, err)
	}
	if up.calls.Load() != 1 {
		t.Fatalf("cold calls=%d, want 1", up.calls.Load())
	}

	time.Sleep(50 * time.Millisecond)         // let it expire
	up.set(300*time.Millisecond, "v2", false) // slow refresh, new value

	start := time.Now()
	v, err := swrGet(t, c)
	elapsed := time.Since(start)
	if err != nil || v != "v1" {
		t.Fatalf("SWR must serve stale v1; got %q err=%v", v, err)
	}
	if elapsed > 120*time.Millisecond {
		t.Fatalf("SWR blocked %v on the 300ms refresh; must serve stale ~instantly", elapsed)
	}
	if !waitFor(2*time.Second, func() bool { return up.calls.Load() == 2 }) {
		t.Fatalf("background refresh not started; calls=%d want 2", up.calls.Load())
	}
	if !waitFor(2*time.Second, func() bool { vv, _ := swrGet(t, c); return vv == "v2" }) {
		t.Fatalf("post-refresh did not serve fresh v2")
	}
}

// TestCachedAssetsReader_SWRSingleFlight: many concurrent reads of an
// expired entry all get stale immediately and trigger EXACTLY ONE
// background refresh (1 cold + 1 refresh = 2 upstream calls), never
// a stampede, regardless of concurrency.
func TestCachedAssetsReader_SWRSingleFlight(t *testing.T) {
	up := &swrAssetsUpstream{fakeAssetsUpstream: &fakeAssetsUpstream{}, val: "v1"}
	c := NewCachedAssetsReader(up, 20*time.Millisecond)

	if _, err := swrGet(t, c); err != nil { // cold → calls=1
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)         // expire
	up.set(250*time.Millisecond, "v2", false) // slow refresh window

	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v, err := swrGet(t, c); err != nil || v != "v1" {
				t.Errorf("concurrent SWR got %q err=%v, want stale v1", v, err)
			}
		}()
	}
	wg.Wait()

	if !waitFor(2*time.Second, func() bool { return up.calls.Load() == 2 }) {
		t.Fatalf("want exactly 2 upstream calls (1 cold + 1 single-flighted refresh); got %d", up.calls.Load())
	}
	time.Sleep(350 * time.Millisecond) // let the 250ms refresh finish; no new reads
	if got := up.calls.Load(); got != 2 {
		t.Fatalf("single-flight violated: %d upstream calls for 25 concurrent stale reads", got)
	}
}

// TestCachedAssetsReader_SWRKeepsStaleOnError: when the background
// refresh fails, the user keeps getting the stale value (never an
// error, never a block) and the refresh is retried on the next
// expired request.
func TestCachedAssetsReader_SWRKeepsStaleOnError(t *testing.T) {
	up := &swrAssetsUpstream{fakeAssetsUpstream: &fakeAssetsUpstream{}, val: "v1"}
	c := NewCachedAssetsReader(up, 20*time.Millisecond)

	if _, err := swrGet(t, c); err != nil { // cold v1, calls=1
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond) // expire
	up.set(0, "v2", true)             // refresh will error (fast)

	v, err := swrGet(t, c)
	if err != nil || v != "v1" {
		t.Fatalf("stale-with-failing-refresh must serve stale v1, no error; got %q err=%v", v, err)
	}
	if !waitFor(2*time.Second, func() bool { return up.calls.Load() == 2 }) {
		t.Fatalf("refresh not attempted; calls=%d want 2", up.calls.Load())
	}

	time.Sleep(40 * time.Millisecond) // re-expire
	v2, err2 := swrGet(t, c)
	if err2 != nil || v2 != "v1" {
		t.Fatalf("after a failed refresh, still serve stale v1 no error; got %q err=%v", v2, err2)
	}
	if !waitFor(2*time.Second, func() bool { return up.calls.Load() >= 3 }) {
		t.Fatalf("failed refresh was not retried on the next request; calls=%d", up.calls.Load())
	}
}

// swrAssetByIDUpstream is a race-safe configurable GetAssetByAssetID
// stub for the generic swr[T] tests (#24): atomic call counter,
// fixed construction-time delay, atomic "fail on call >= 2" toggle
// (deterministic by call number → no mid-test field mutation, so
// `go test -race` is clean under the concurrent background
// refresh). Embeds *fakeAssetsUpstream for the other methods.
type swrAssetByIDUpstream struct {
	*fakeAssetsUpstream
	calls   atomic.Int64
	delay   time.Duration
	failGE2 atomic.Bool
}

func (s *swrAssetByIDUpstream) GetAssetByAssetID(ctx context.Context, assetID string) (timescale.AssetRow, error) {
	n := s.calls.Add(1)
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return timescale.AssetRow{}, ctx.Err()
		}
	}
	if n >= 2 && s.failGE2.Load() {
		return timescale.AssetRow{}, errors.New("swr asset boom")
	}
	return timescale.AssetRow{AssetID: assetID}, nil
}

// TestCachedAssetsReader_GenericSWRServesStaleSingleFlight pins the
// generic swr[T] via the now-cached GetAssetByAssetID: an expired
// entry serves stale IMMEDIATELY under heavy concurrency and
// triggers EXACTLY ONE single-flighted background refresh — the
// #24 fix for /v1/assets/{id}.
func TestCachedAssetsReader_GenericSWRServesStaleSingleFlight(t *testing.T) {
	up := &swrAssetByIDUpstream{fakeAssetsUpstream: &fakeAssetsUpstream{}, delay: 300 * time.Millisecond}
	c := NewCachedAssetsReader(up, 25*time.Millisecond)

	if r, err := c.GetAssetByAssetID(context.Background(), "X"); err != nil || r.AssetID != "X" {
		t.Fatalf("cold fetch: got %q err=%v, want X", r.AssetID, err) // blocks ~300ms, calls=1
	}
	if up.calls.Load() != 1 {
		t.Fatalf("cold calls=%d, want 1", up.calls.Load())
	}
	time.Sleep(50 * time.Millisecond) // expire

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			st := time.Now()
			r, err := c.GetAssetByAssetID(context.Background(), "X")
			if err != nil || r.AssetID != "X" {
				t.Errorf("SWR must serve stale X no err; got %q err=%v", r.AssetID, err)
			}
			if d := time.Since(st); d > 120*time.Millisecond {
				t.Errorf("SWR blocked %v; must serve stale ~instantly (refresh is 300ms)", d)
			}
		}()
	}
	wg.Wait()

	if !waitFor(2*time.Second, func() bool { return up.calls.Load() == 2 }) {
		t.Fatalf("want exactly 2 upstream calls (1 cold + 1 single-flighted refresh); got %d", up.calls.Load())
	}
	time.Sleep(400 * time.Millisecond) // let the 300ms refresh finish; no new reads
	if got := up.calls.Load(); got != 2 {
		t.Fatalf("single-flight violated: %d upstream calls for 20 concurrent stale reads", got)
	}
}

// TestCachedAssetsReader_GenericSWRKeepsStaleOnError: a failing
// background refresh keeps serving stale (never an error, never a
// block) and is retried on the next expired request.
func TestCachedAssetsReader_GenericSWRKeepsStaleOnError(t *testing.T) {
	up := &swrAssetByIDUpstream{fakeAssetsUpstream: &fakeAssetsUpstream{}}
	up.failGE2.Store(true) // call 1 (cold) OK; call >=2 (refresh) errors
	c := NewCachedAssetsReader(up, 20*time.Millisecond)

	if r, err := c.GetAssetByAssetID(context.Background(), "X"); err != nil || r.AssetID != "X" {
		t.Fatalf("cold: got %q err=%v, want X", r.AssetID, err) // calls=1
	}
	time.Sleep(40 * time.Millisecond) // expire

	r, err := c.GetAssetByAssetID(context.Background(), "X")
	if err != nil || r.AssetID != "X" {
		t.Fatalf("stale-with-failing-refresh must serve stale X no err; got %q err=%v", r.AssetID, err)
	}
	if !waitFor(2*time.Second, func() bool { return up.calls.Load() == 2 }) {
		t.Fatalf("refresh not attempted; calls=%d want 2", up.calls.Load())
	}

	time.Sleep(40 * time.Millisecond) // re-expire
	r2, err2 := c.GetAssetByAssetID(context.Background(), "X")
	if err2 != nil || r2.AssetID != "X" {
		t.Fatalf("after a failed refresh, still serve stale no err; got %q err=%v", r2.AssetID, err2)
	}
	if !waitFor(2*time.Second, func() bool { return up.calls.Load() >= 3 }) {
		t.Fatalf("failed refresh was not retried; calls=%d", up.calls.Load())
	}
}
