package v1

import (
	"context"
	"errors"
	"strings"
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

// TestCachedAssetsReader_ListAssetsExtHitMissCounter pins both the upstream-call dedup AND
// the stellarindex_api_cache_ops_total{cache="coins",op="list_coins"} counter deltas: the
// first call is a miss (+1 miss), the repeat is a hit (+1 hit) and must NOT call upstream
// again.
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

// TestCachedAssetsReader_HistoryBatchHitMissCounter exercises both the 24h and 7d history-batch caches, which share
// historyBatch, and catches either branch silently dropping the metric increment.
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

// TestCachedAssetsReader_TTLZeroBypasses pins that ttl=0 disables the cache entirely: every call goes to upstream and no
// metric is counted.
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
// part of the ListAssetsExt cache key: two requests differing only by Code must NOT collide,
// each getting its own upstream call and rows, and a repeat of the same Code hits the cache.
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

// typeEchoUpstream tags each result with opts.Type, the same trick
// codeEchoUpstream plays on Code.
type typeEchoUpstream struct {
	*fakeAssetsUpstream
	calls atomic.Int64
}

func (u *typeEchoUpstream) ListAssetsExt(_ context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	u.calls.Add(1)
	return []timescale.AssetRow{{AssetID: "type=" + opts.Type}}, nil
}

// TestCachedAssetsReader_TypeInCacheKey: Type narrows the listing spine, so it is result-changing. Left out of the key,
// type=classic and type=soroban would share a slot and serve each other's rows.
func TestCachedAssetsReader_TypeInCacheKey(t *testing.T) {
	up := &typeEchoUpstream{fakeAssetsUpstream: &fakeAssetsUpstream{}}
	c := NewCachedAssetsReader(up, 60*time.Second)

	classic, err := c.ListAssetsExt(context.Background(), timescale.ListAssetsOptions{Limit: 50, Type: "classic"})
	if err != nil {
		t.Fatal(err)
	}
	soroban, err := c.ListAssetsExt(context.Background(), timescale.ListAssetsOptions{Limit: 50, Type: "soroban"})
	if err != nil {
		t.Fatal(err)
	}
	if classic[0].AssetID != "type=classic" || soroban[0].AssetID != "type=soroban" {
		t.Fatalf("Type missing from cache key — got %q then %q (collision)",
			classic[0].AssetID, soroban[0].AssetID)
	}
	if got := up.calls.Load(); got != 2 {
		t.Errorf("distinct Type values must miss separately; upstream calls=%d want 2", got)
	}
	// An unfiltered listing is a third, distinct slot — it must not be
	// served either of the narrowed page-sets.
	all, err := c.ListAssetsExt(context.Background(), timescale.ListAssetsOptions{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	if all[0].AssetID != "type=" {
		t.Errorf("unfiltered listing served a type-filtered slot: %q", all[0].AssetID)
	}
}

// swrAssetsUpstream is a configurable ListAssetsExt stub for the stale-while-revalidate tests (call counter, settable delay,
// return value and failure). It embeds fakeAssetsUpstream for the other AssetsReader methods.
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

// blockedCallerBailout bounds how long a test waits for a caller a working cache serves immediately. Only a cache that blocks
// on background work reaches it, so it is a hang guard, not a budget, and names the defect instead of hitting the package
// deadline.
const blockedCallerBailout = 5 * time.Second

// upstreamSettleWindow is how long a test watches for an upstream call that must NOT arrive. It bounds no correct behaviour,
// so a loaded runner makes it less sensitive, never red, and it exits early on a violation.
const upstreamSettleWindow = 100 * time.Millisecond

// expireSWREntries walks every generic swr[T] entry back past any ttl,
// so the next read takes the stale-while-revalidate branch. Sleeping past a tiny ttl is not
// an option: the entry a refresh WRITES must stay fresh while the test asserts on it.
func expireSWREntries(c *CachedAssetsReader) {
	c.single.mu.Lock()
	defer c.single.mu.Unlock()
	for _, e := range c.single.entries {
		if !e.at.IsZero() {
			e.at = e.at.Add(-time.Hour)
		}
	}
}

// upstreamHold parks an upstream call inside the stub until released, so non-blocking claims are asserted by event order and
// differing values, not racy delays. from is the first covered call number; entered closes on the first covered call;
// completed and cancelled count finished and ctx-cut calls. Tests release it in t.Cleanup.
type upstreamHold struct {
	from      int64
	entered   chan struct{}
	released  chan struct{}
	enterOnce sync.Once
	freeOnce  sync.Once
	completed atomic.Int64
	cancelled atomic.Int64
}

func newUpstreamHold(from int64) *upstreamHold {
	return &upstreamHold{
		from:     from,
		entered:  make(chan struct{}),
		released: make(chan struct{}),
	}
}

// park blocks call number n inside the stub until the hold is released; it is a no-op for a nil hold or an uncovered call. It
// returns ctx.Err() if the call's own context is cancelled first, so request-bound upstream work still aborts.
func (h *upstreamHold) park(ctx context.Context, n int64) error {
	if h == nil || n < h.from {
		return nil
	}
	h.enterOnce.Do(func() { close(h.entered) })
	select {
	case <-h.released:
		h.completed.Add(1)
		return nil
	case <-ctx.Done():
		h.cancelled.Add(1)
		return ctx.Err()
	}
}

// release lets every held call, and every later one, through.
// Idempotent, so a test can release inline and still register it with
// t.Cleanup for the paths that fail before reaching that line.
func (h *upstreamHold) release() { h.freeOnce.Do(func() { close(h.released) }) }

// awaitCall blocks until a covered call has reached the upstream,
// failing the test if none does. An arrival is an event, so it is
// waited for rather than polled on a call count.
func (h *upstreamHold) awaitCall(t *testing.T, what string) {
	t.Helper()
	select {
	case <-h.entered:
	case <-time.After(blockedCallerBailout):
		t.Fatalf("%s never reached the upstream", what)
	}
}

// TestCachedAssetsReader_SWRServesStaleAndRefreshes: an expired entry returns the stale value immediately, not blocked on the
// slow upstream, one background refresh runs, and the fresh value is served afterwards.
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

// TestCachedAssetsReader_SWRSingleFlight: many concurrent reads of an expired entry all get stale immediately and trigger
// exactly one background refresh (2 upstream calls total), never a stampede.
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

// TestCachedAssetsReader_SWRKeepsStaleOnError: when the background refresh fails, callers keep getting the stale value (no
// error, no block) and the refresh is retried on the next expired request.
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

// ── HLT-01: the history-batch cache must have the SAME SWR posture
// as its ListAssetsExt sibling ────────────────────────────────────
//
// historyBatch needs the (A') stale-while-revalidate branch: without it an EXPIRED history
// batch falls through to the blocking cold-leader path and every caller waits on the slow
// upstream, the stampede-on-expiry the rows path avoids. Both caches serve the SAME request
// (/v1/assets?include=sparkline), so a missing branch brings the latency cliff back.

// GetAssetsPriceHistory24hBatch overrides the embedded fake so the SWR harness knobs drive the history path too. It returns
// one point per asset carrying the configured value in P, which distinguishes v1 from v2.
func (s *swrAssetsUpstream) GetAssetsPriceHistory24hBatch(ctx context.Context, _ []string) (map[string][]timescale.AssetPricePoint, error) {
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
		return nil, errors.New("swr history upstream boom")
	}
	val := v
	return map[string][]timescale.AssetPricePoint{
		"native": {{T: "2026-05-11T00:00:00Z", P: &val}},
	}, nil
}

func swrHistoryGet(t *testing.T, c *CachedAssetsReader) (string, error) {
	t.Helper()
	m, err := c.GetAssetsPriceHistory24hBatch(context.Background(), []string{"native"})
	if err != nil {
		return "", err
	}
	pts := m["native"]
	if len(pts) == 0 || pts[0].P == nil {
		return "", nil
	}
	return *pts[0].P, nil
}

// TestCachedAssetsReader_HistorySWRServesStaleAndRefreshes mirrors the plain SWR test for the history batch: stale map served
// immediately, one background refresh, fresh value afterwards.
func TestCachedAssetsReader_HistorySWRServesStaleAndRefreshes(t *testing.T) {
	up := &swrAssetsUpstream{fakeAssetsUpstream: &fakeAssetsUpstream{}, val: "v1"}
	c := NewCachedAssetsReader(up, 25*time.Millisecond)

	if v, err := swrHistoryGet(t, c); err != nil || v != "v1" {
		t.Fatalf("cold fetch: got %q err=%v, want v1", v, err)
	}
	if up.calls.Load() != 1 {
		t.Fatalf("cold calls=%d, want 1", up.calls.Load())
	}

	time.Sleep(50 * time.Millisecond)         // let it expire
	up.set(300*time.Millisecond, "v2", false) // slow refresh, new value

	start := time.Now()
	v, err := swrHistoryGet(t, c)
	elapsed := time.Since(start)
	if err != nil || v != "v1" {
		t.Fatalf("SWR must serve the stale history map; got %q err=%v", v, err)
	}
	if elapsed > 120*time.Millisecond {
		t.Fatalf("expired history batch BLOCKED %v on the 300ms upstream refetch; "+
			"it must serve stale ~instantly like the rows path", elapsed)
	}
	if !waitFor(2*time.Second, func() bool { return up.calls.Load() == 2 }) {
		t.Fatalf("background refresh not started; calls=%d want 2", up.calls.Load())
	}
	if !waitFor(2*time.Second, func() bool { vv, _ := swrHistoryGet(t, c); return vv == "v2" }) {
		t.Fatalf("post-refresh did not serve fresh v2")
	}
}

// TestCachedAssetsReader_HistorySWRSingleFlight mirrors the rows
// single-flight guarantee: many concurrent reads of an expired history
// batch all get stale immediately and trigger EXACTLY ONE refresh.
func TestCachedAssetsReader_HistorySWRSingleFlight(t *testing.T) {
	up := &swrAssetsUpstream{fakeAssetsUpstream: &fakeAssetsUpstream{}, val: "v1"}
	c := NewCachedAssetsReader(up, 20*time.Millisecond)

	if _, err := swrHistoryGet(t, c); err != nil { // cold → calls=1
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)         // expire
	up.set(250*time.Millisecond, "v2", false) // slow refresh window

	var wg sync.WaitGroup
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v, err := swrHistoryGet(t, c); err != nil || v != "v1" {
				t.Errorf("concurrent history SWR got %q err=%v, want stale v1", v, err)
			}
		}()
	}
	wg.Wait()

	if !waitFor(2*time.Second, func() bool { return up.calls.Load() == 2 }) {
		t.Fatalf("want exactly 2 upstream calls (1 cold + 1 single-flighted refresh); got %d", up.calls.Load())
	}
	time.Sleep(350 * time.Millisecond) // let the 250ms refresh finish
	if got := up.calls.Load(); got != 2 {
		t.Fatalf("single-flight violated: %d upstream calls for 25 concurrent stale reads", got)
	}
}

// TestCachedAssetsReader_HistorySWRKeepsStaleOnError mirrors the rows
// failure posture: a failing background refresh keeps serving stale
// (never an error, never a block) and is retried next time.
func TestCachedAssetsReader_HistorySWRKeepsStaleOnError(t *testing.T) {
	up := &swrAssetsUpstream{fakeAssetsUpstream: &fakeAssetsUpstream{}, val: "v1"}
	c := NewCachedAssetsReader(up, 20*time.Millisecond)

	if _, err := swrHistoryGet(t, c); err != nil { // cold v1, calls=1
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond) // expire
	up.set(0, "v2", true)             // refresh will error (fast)

	v, err := swrHistoryGet(t, c)
	if err != nil || v != "v1" {
		t.Fatalf("stale-with-failing-refresh must serve stale v1, no error; got %q err=%v", v, err)
	}
	if !waitFor(2*time.Second, func() bool { return up.calls.Load() == 2 }) {
		t.Fatalf("refresh not attempted; calls=%d want 2", up.calls.Load())
	}

	time.Sleep(40 * time.Millisecond) // re-expire
	v2, err2 := swrHistoryGet(t, c)
	if err2 != nil || v2 != "v1" {
		t.Fatalf("after a failed refresh, still serve stale v1 no error; got %q err=%v", v2, err2)
	}
	if !waitFor(2*time.Second, func() bool { return up.calls.Load() >= 3 }) {
		t.Fatalf("failed refresh was not retried on the next request; calls=%d", up.calls.Load())
	}
}

// swrAssetStaleLedger / swrAssetFreshLedger distinguish the cold-fetched row from its refresh: the stub advances the asset's
// last-seen ledger after the first call, so stale-served and refresh-waiting callers hold different rows without relying on
// timing.
const (
	swrAssetStaleLedger = 100
	swrAssetFreshLedger = 200
)

// swrAssetByIDUpstream is a race-safe GetAssetByAssetID stub for the generic swr[T] tests: atomic call counter, optional hold,
// and a deterministic "fail on call >= 2" toggle so `go test -race` stays clean. It embeds *fakeAssetsUpstream.
type swrAssetByIDUpstream struct {
	*fakeAssetsUpstream
	calls   atomic.Int64
	hold    *upstreamHold
	failGE2 atomic.Bool
}

func (s *swrAssetByIDUpstream) GetAssetByAssetID(ctx context.Context, assetID string) (timescale.AssetRow, error) {
	n := s.calls.Add(1)
	if err := s.hold.park(ctx, n); err != nil {
		return timescale.AssetRow{}, err
	}
	if n >= 2 && s.failGE2.Load() {
		return timescale.AssetRow{}, errors.New("swr asset boom")
	}
	seen := uint32(swrAssetStaleLedger)
	if n >= 2 {
		seen = swrAssetFreshLedger
	}
	return timescale.AssetRow{AssetID: assetID, LastSeenLedger: seen}, nil
}

// TestCachedAssetsReader_GenericSWRServesStaleSingleFlight pins generic swr[T] via GetAssetByAssetID: 20 concurrent reads of
// an expired entry get the stale row while one background refresh runs and replaces it. The refresh is held until all reads
// return, so it asserts values, not latency.
func TestCachedAssetsReader_GenericSWRServesStaleSingleFlight(t *testing.T) {
	hold := newUpstreamHold(2) // the cold fetch runs free; the refresh is parked
	t.Cleanup(hold.release)
	up := &swrAssetByIDUpstream{fakeAssetsUpstream: &fakeAssetsUpstream{}, hold: hold}
	c := NewCachedAssetsReader(up, time.Minute)

	cold, err := c.GetAssetByAssetID(context.Background(), "X")
	if err != nil || cold.AssetID != "X" || cold.LastSeenLedger != swrAssetStaleLedger {
		t.Fatalf("cold fetch: got %q at ledger %d err=%v, want X at ledger %d",
			cold.AssetID, cold.LastSeenLedger, err, swrAssetStaleLedger)
	}
	if up.calls.Load() != 1 {
		t.Fatalf("cold calls=%d, want 1", up.calls.Load())
	}
	expireSWREntries(c)

	// The reads report back on a buffered channel rather than failing
	// from their own goroutines: a caller blocked on the refresh is a
	// failure this test has to name, and naming it ends the test while
	// its siblings are still running.
	const reads = 20
	type staleRead struct {
		assetID string
		ledger  uint32
		err     error
	}
	served := make(chan staleRead, reads)
	for i := 0; i < reads; i++ {
		go func() {
			r, err := c.GetAssetByAssetID(context.Background(), "X")
			served <- staleRead{assetID: r.AssetID, ledger: r.LastSeenLedger, err: err}
		}()
	}
	for i := 0; i < reads; i++ {
		select {
		case got := <-served:
			if got.err != nil || got.assetID != "X" {
				t.Fatalf("stale read %d: got %q err=%v, want X", i, got.assetID, got.err)
			}
			if got.ledger != swrAssetStaleLedger {
				t.Fatalf("stale read %d was served ledger %d, want the stale %d — it waited for the refresh instead of being served the entry that was already there",
					i, got.ledger, swrAssetStaleLedger)
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

	// Released, the one refresh settles the entry. Polling by READ adds no upstream call: a
	// stale read looks at the in-flight marker, which the refresh holds until it has written
	// its result, so every earlier poll is served the stale entry and the succeeding poll the
	// refreshed one. The ttl outlives the test, so the entry stays fresh and the call count
	// is final.
	hold.release()
	var fresh timescale.AssetRow
	var freshErr error
	if !waitFor(2*time.Second, func() bool {
		fresh, freshErr = c.GetAssetByAssetID(context.Background(), "X")
		return freshErr == nil && fresh.LastSeenLedger == swrAssetFreshLedger
	}) {
		t.Fatalf("after the refresh: got ledger %d err=%v, want the refreshed %d",
			fresh.LastSeenLedger, freshErr, swrAssetFreshLedger)
	}
	// Nothing further may reach the upstream — see upstreamSettleWindow
	// for why an absence gets a window and why that window fails
	// nothing correct.
	if waitFor(upstreamSettleWindow, func() bool { return up.calls.Load() != 2 }) {
		t.Fatalf("single-flight violated: %d upstream calls once the refresh had settled, want 2 (1 cold + 1 refresh)", up.calls.Load())
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

// recordingHistoryUpstream answers every asset with one point and records the id sets it was asked for.
type recordingHistoryUpstream struct {
	mu    sync.Mutex
	asked [][]string
	empty map[string]bool
}

func (u *recordingHistoryUpstream) batch(_ context.Context, ids []string) (map[string][]timescale.AssetPricePoint, error) {
	u.mu.Lock()
	u.asked = append(u.asked, append([]string(nil), ids...))
	u.mu.Unlock()
	out := map[string][]timescale.AssetPricePoint{}
	for _, id := range ids {
		if !u.empty[id] {
			out[id] = []timescale.AssetPricePoint{{T: id}}
		}
	}
	return out, nil
}

// A volume-ranked page whose set shifts by one asset must read only that asset, not the whole page again.
func TestCachedAssetsReader_HistoryBatchReadsOnlyNewAssets(t *testing.T) {
	c := NewCachedAssetsReader(nil, time.Minute)
	up := &recordingHistoryUpstream{}
	if _, err := c.historyBatch(context.Background(), "h", []string{"a", "b", "c"}, up.batch); err != nil {
		t.Fatal(err)
	}
	got, err := c.historyBatch(context.Background(), "h", []string{"b", "c", "d"}, up.batch)
	if err != nil {
		t.Fatal(err)
	}
	if len(up.asked) != 2 || strings.Join(up.asked[1], ",") != "d" {
		t.Fatalf("upstream asked %v, want the second read to ask only [d]", up.asked)
	}
	for _, id := range []string{"b", "c", "d"} {
		if len(got[id]) != 1 || got[id][0].T != id {
			t.Errorf("%s = %v, want its own point", id, got[id])
		}
	}
	if _, ok := got["a"]; ok {
		t.Error("an asset not asked for must not be served")
	}
}

// An asset with no series is cached as absent: it stays out of the map and is not re-read on every page.
func TestCachedAssetsReader_HistoryBatchCachesAbsentAssets(t *testing.T) {
	c := NewCachedAssetsReader(nil, time.Minute)
	up := &recordingHistoryUpstream{empty: map[string]bool{"x": true}}
	for range 2 {
		got, err := c.historyBatch(context.Background(), "h", []string{"x", "y"}, up.batch)
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := got["x"]; ok {
			t.Fatal("an absent asset must stay absent from the map")
		}
	}
	if len(up.asked) != 1 {
		t.Fatalf("upstream calls = %d, want 1", len(up.asked))
	}
}
