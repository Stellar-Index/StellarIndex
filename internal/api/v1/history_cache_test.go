package v1

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// histStaleLedger / histFreshLedger tell the trade a cold fill cached from the trade a refresh
// replaces it with: the fake moves the latest trade forward a ledger on every call after the
// first. A fixed-row stub would leave only a stopwatch to tell a cached read from a refreshed one.
const (
	histStaleLedger = 100
	histFreshLedger = 200
)

// fakeHist is a HistoryReader implementing only LatestTradePerSource (the embedded interface is nil, other methods panic). Counts
// are atomic and failures keyed on call number for -race; an optional hold parks a call so tests order against an in-flight fill.
type fakeHist struct {
	HistoryReader
	calls   atomic.Int64
	hold    *upstreamHold
	failGE2 atomic.Bool
}

func (f *fakeHist) LatestTradePerSource(
	ctx context.Context, _ canonical.Pair, _ string,
) ([]canonical.Trade, error) {
	n := f.calls.Add(1)
	if err := f.hold.park(ctx, n); err != nil {
		return nil, err
	}
	if n >= 2 && f.failGE2.Load() {
		return nil, errors.New("swr history boom")
	}
	ledger := uint32(histStaleLedger)
	if n >= 2 {
		ledger = histFreshLedger
	}
	return []canonical.Trade{{Source: "sdex", Ledger: ledger}}, nil
}

func histTestPair(t *testing.T) canonical.Pair {
	t.Helper()
	base, err := canonical.ParseAsset("native")
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	quote, err := canonical.ParseAsset("fiat:USD")
	if err != nil {
		t.Fatalf("parse quote: %v", err)
	}
	p, err := canonical.NewPair(base, quote)
	if err != nil {
		t.Fatalf("new pair: %v", err)
	}
	return p
}

// expireHistoryEntries walks every cached entry back past any ttl so the next read takes the
// stale-while-revalidate branch. Sleeping past a tiny ttl cannot keep the entry a refresh WRITES
// fresh while the test asserts on it.
func expireHistoryEntries(c *CachedHistoryReader) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, e := range c.entries {
		if !e.at.IsZero() {
			e.at = e.at.Add(-time.Hour)
		}
	}
}

// TestCachedHistoryReader_ColdThenFreshHit: a cold call returns the
// upstream result (one call); an immediate repeat is a fresh hit
// (still one call). ttl=0 disables caching entirely (pass-through).
func TestCachedHistoryReader_ColdThenFreshHit(t *testing.T) {
	up := &fakeHist{}
	c := NewCachedHistoryReader(up, time.Minute)
	p := histTestPair(t)

	rows, err := c.LatestTradePerSource(context.Background(), p, "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("cold: rows=%d err=%v", len(rows), err)
	}
	rows, err = c.LatestTradePerSource(context.Background(), p, "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("fresh hit: rows=%d err=%v", len(rows), err)
	}
	if up.calls.Load() != 1 {
		t.Fatalf("want 1 upstream call (cold + fresh hit); got %d", up.calls.Load())
	}

	pt := NewCachedHistoryReader(&fakeHist{}, 0) // ttl=0 → pass-through
	if _, err := pt.LatestTradePerSource(context.Background(), p, ""); err != nil {
		t.Fatalf("pass-through err: %v", err)
	}
}

// coldCallerDeadline is the cold caller's request budget. It is a stimulus, not a bound: the fill is held open past the caller's
// return, so any finite deadline expires first.
const coldCallerDeadline = 30 * time.Millisecond

// TestCachedHistoryReader_DetachedColdFillWarms pins that a cold caller whose ctx deadline is
// shorter than the upstream query gets ctx.Err() (the handler 503s) while the fill it started
// keeps running on its OWN budget, so the next poll is served from the entry that fill wrote, and
// the timed-out caller never spawns a second upstream call.
//
// Both halves are claims about the ORDER of two events, so the fill is HELD inside the store until
// the test releases it. The order is then constructed, not raced: the fill has provably not
// completed and the only edge the caller can wake on is its own expired deadline. Sleeping against
// a short ctx lets a starved caller see both select arms ready, and wall-time ceilings only
// measure runner scheduling.
func TestCachedHistoryReader_DetachedColdFillWarms(t *testing.T) {
	hold := newUpstreamHold(1) // the cold fill is parked from its first call
	t.Cleanup(hold.release)
	up := &fakeHist{hold: hold}
	c := NewCachedHistoryReader(up, time.Minute)
	p := histTestPair(t)

	ctx, cancel := context.WithTimeout(context.Background(), coldCallerDeadline)
	defer cancel()
	type coldReturn struct {
		rows []canonical.Trade
		err  error
	}
	// Buffered, and the goroutine touches nothing but the channel: the
	// caller is the thing under test, so the test has to be able to
	// fail while it is still blocked.
	returned := make(chan coldReturn, 1)
	go func() {
		rows, err := c.LatestTradePerSource(ctx, p, "")
		returned <- coldReturn{rows: rows, err: err}
	}()

	hold.awaitCall(t, "the detached cold fill")
	var cold coldReturn
	select {
	case cold = <-returned:
	case <-time.After(blockedCallerBailout):
		t.Fatal("the cold caller never returned while its fill sat in the store — it waited on the fill instead of on its own context")
	}
	if n := hold.completed.Load(); n != 0 {
		t.Fatalf("%d held fills had already finished when the cold caller returned; there was no fill in flight to order it against", n)
	}
	if !errors.Is(cold.err, context.DeadlineExceeded) {
		t.Fatalf("cold caller must get DeadlineExceeded (→503); got %v with %d rows", cold.err, len(cold.rows))
	}

	// The fill outlives the request that started it: released, it
	// settles the entry, and the next poll is served from it off that
	// one upstream call.
	hold.release()
	rows, err := c.LatestTradePerSource(context.Background(), p, "")
	if n := hold.cancelled.Load(); n != 0 {
		t.Fatalf("%d fills died with the request ctx; the detached fill must run on its own budget, not the caller's", n)
	}
	if err != nil || len(rows) != 1 {
		t.Fatalf("detached fill never warmed the cache: rows=%d err=%v", len(rows), err)
	}
	if rows[0].Ledger != histStaleLedger {
		t.Fatalf("the warmed entry holds ledger %d, want the %d the held fill returned", rows[0].Ledger, histStaleLedger)
	}
	if got := up.calls.Load(); got != 1 {
		t.Fatalf("timed-out caller must not spawn a 2nd fill; want 1 upstream call, got %d", got)
	}
}

// TestCachedHistoryReader_SWRServesStaleSingleFlight: 20 concurrent reads of an expired entry all get the stale trade while
// exactly one detached refresh runs, then the refreshed trade replaces it. Asserts on served value (ledger 100 stale vs 200
// fresh) with the refresh held until all reads return, so it is load-independent.
func TestCachedHistoryReader_SWRServesStaleSingleFlight(t *testing.T) {
	hold := newUpstreamHold(2) // the cold fill runs free; the refresh is parked
	t.Cleanup(hold.release)
	up := &fakeHist{hold: hold}
	c := NewCachedHistoryReader(up, time.Minute)
	p := histTestPair(t)

	cold, err := c.LatestTradePerSource(context.Background(), p, "")
	if err != nil || len(cold) != 1 || cold[0].Ledger != histStaleLedger {
		t.Fatalf("cold: rows=%d err=%v, want 1 row at ledger %d", len(cold), err, histStaleLedger)
	}
	if up.calls.Load() != 1 {
		t.Fatalf("cold calls=%d want 1", up.calls.Load())
	}
	expireHistoryEntries(c)

	// The reads report back on a buffered channel rather than failing
	// from their own goroutines: a caller blocked on the refresh is a
	// failure this test has to name, and naming it ends the test while
	// its siblings are still running.
	const reads = 20
	type staleRead struct {
		rows   int
		ledger uint32
		err    error
	}
	served := make(chan staleRead, reads)
	for i := 0; i < reads; i++ {
		go func() {
			rows, err := c.LatestTradePerSource(context.Background(), p, "")
			r := staleRead{rows: len(rows), err: err}
			if len(rows) > 0 {
				r.ledger = rows[0].Ledger
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
			if got.ledger != histStaleLedger {
				t.Fatalf("stale read %d was served ledger %d, want the stale %d — it waited for the refresh instead of being served the entry that was already there",
					i, got.ledger, histStaleLedger)
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
		t.Fatalf("single-flight violated: %d calls for %d concurrent stale reads, want 2 (1 cold + 1 refresh)", got, reads)
	}

	// Released, the one refresh settles the entry. Polling by READ adds no upstream call: the
	// in-flight marker is held until the refresh has written, so earlier polls get the stale
	// entry and the successful one the refreshed entry. The ttl outlives the test, so the call
	// count is then final.
	hold.release()
	var fresh []canonical.Trade
	var freshErr error
	if !waitFor(2*time.Second, func() bool {
		fresh, freshErr = c.LatestTradePerSource(context.Background(), p, "")
		return freshErr == nil && len(fresh) == 1 && fresh[0].Ledger == histFreshLedger
	}) {
		t.Fatalf("after the refresh: rows=%d err=%v, want 1 row at the refreshed ledger %d", len(fresh), freshErr, histFreshLedger)
	}
	// Nothing further may reach the upstream: the refresh re-stamped the entry, so the read above
	// was a hit, not a second stale serve. See upstreamSettleWindow for why an absence needs a
	// window.
	if waitFor(upstreamSettleWindow, func() bool { return up.calls.Load() != 2 }) {
		t.Fatalf("single-flight violated: %d upstream calls once the refresh had settled, want 2 (1 cold + 1 refresh)", up.calls.Load())
	}
}

// TestCachedHistoryReader_SWRKeepsStaleOnError: a failing background
// refresh keeps serving stale (never errors, never blocks) and is
// retried on the next expired request.
func TestCachedHistoryReader_SWRKeepsStaleOnError(t *testing.T) {
	up := &fakeHist{}
	up.failGE2.Store(true) // call 1 (cold) OK; call >=2 (refresh) errors
	c := NewCachedHistoryReader(up, 20*time.Millisecond)
	p := histTestPair(t)

	if _, err := c.LatestTradePerSource(context.Background(), p, ""); err != nil {
		t.Fatalf("cold: %v", err) // calls=1
	}
	time.Sleep(40 * time.Millisecond) // expire

	rows, err := c.LatestTradePerSource(context.Background(), p, "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("stale-with-failing-refresh must serve stale; rows=%d err=%v", len(rows), err)
	}
	if !waitFor(2*time.Second, func() bool { return up.calls.Load() == 2 }) {
		t.Fatalf("refresh not attempted; calls=%d want 2", up.calls.Load())
	}

	time.Sleep(40 * time.Millisecond) // re-expire → retry
	rows2, err2 := c.LatestTradePerSource(context.Background(), p, "")
	if err2 != nil || len(rows2) != 1 {
		t.Fatalf("must still serve stale after a failed refresh; rows=%d err=%v", len(rows2), err2)
	}
	if !waitFor(2*time.Second, func() bool { return up.calls.Load() == 3 }) {
		t.Fatalf("refresh not retried after re-expire; calls=%d want 3", up.calls.Load())
	}
}

// TestCachedHistoryReader_FreshContextBypassesCache: a warm entry must not
// answer a caller that marked its ctx fresh, and the bypass leaves the
// cached entry untouched.
func TestCachedHistoryReader_FreshContextBypassesCache(t *testing.T) {
	up := &fakeHist{}
	c := NewCachedHistoryReader(up, time.Minute)
	p := histTestPair(t)

	if _, err := c.LatestTradePerSource(context.Background(), p, ""); err != nil {
		t.Fatalf("warm: %v", err)
	}
	rows, err := c.LatestTradePerSource(withFreshHistory(context.Background()), p, "")
	if err != nil || len(rows) != 1 || rows[0].Ledger != histFreshLedger {
		t.Fatalf("fresh read: rows=%v err=%v, want the store's ledger %d", rows, err, histFreshLedger)
	}
	cached, err := c.LatestTradePerSource(context.Background(), p, "")
	if err != nil || len(cached) != 1 || cached[0].Ledger != histStaleLedger {
		t.Fatalf("plain read after bypass: %v err=%v, want the original cached entry", cached, err)
	}
	if up.calls.Load() != 2 {
		t.Fatalf("want 2 upstream calls (warm + bypass); got %d", up.calls.Load())
	}
}

// TestCachedHistoryReader_ColdFillPanic_SurfacesErrorAndStaysRetryable pins what happens AFTER
// the panic is recovered. The detached cold fill's waiter wakes on close(done); a bare
// recover-and-return leaves entry.err nil and entry.flight set, so the waiter gets an empty result
// with a NIL error (a 200 with no trades) and every later request re-joins the closed flight.
//
// So it asserts the corrected values:
//
//  1. the waiter gets errHistoryFillPanicked, not (nil, nil);
//  2. the next call re-runs the upstream (entry dropped, not poisoned) and returns the row;
//  3. stellarindex_worker_panics_total{worker="api-history-latest-per-source-fill"} moved, so the
//     dead refresher pages an operator.
func TestCachedHistoryReader_ColdFillPanic_SurfacesErrorAndStaysRetryable(t *testing.T) {
	const workerName = "api-history-latest-per-source-fill"
	before := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))

	up := &panickingHist{panicUpTo: 1}
	c := NewCachedHistoryReader(up, time.Minute)
	p := histTestPair(t)

	rows, err := c.LatestTradePerSource(context.Background(), p, "")
	if !errors.Is(err, errHistoryFillPanicked) {
		t.Fatalf("cold waiter got (%v rows, err=%v), want errHistoryFillPanicked — a "+
			"panicking fill that reports success serves an EMPTY result as a 200 OK",
			len(rows), err)
	}

	// The entry must have been dropped, not left latched in flight.
	rows, err = c.LatestTradePerSource(context.Background(), p, "")
	if err != nil {
		t.Fatalf("second call err = %v, want nil — the panicked entry poisoned the "+
			"cache instead of being dropped for retry", err)
	}
	if len(rows) != 1 || rows[0].Source != "sdex" {
		t.Fatalf("second call rows = %#v, want exactly the upstream row — the cache "+
			"served the panicked fill's empty result instead of retrying", rows)
	}
	if got := up.calls.Load(); got != 2 {
		t.Fatalf("upstream called %d time(s), want 2 — the second request must start a "+
			"FRESH fill; anything less means the panicked flight was reused", got)
	}

	after := testutil.ToFloat64(obs.WorkerPanicsTotal.WithLabelValues(workerName))
	if after-before != 1 {
		t.Errorf("stellarindex_worker_panics_total{worker=%q} moved by %v, want 1 — "+
			"without it the refresher dies with no page and no runbook", workerName, after-before)
	}
}

// TestCachedHistoryReader_StaleRefreshPanic_KeepsStaleAndRetries: when a refresh panics, keep serving the prior value and clear
// the in-flight marker; a leftover flight would pin the key to one stale value forever.
func TestCachedHistoryReader_StaleRefreshPanic_KeepsStaleAndRetries(t *testing.T) {
	up := &panickingHist{}
	c := NewCachedHistoryReader(up, 20*time.Millisecond)
	p := histTestPair(t)

	// Warm it with a real value.
	if _, err := c.LatestTradePerSource(context.Background(), p, ""); err != nil {
		t.Fatalf("warm: %v", err)
	}
	// Arm the panic for the NEXT (stale-revalidate) call only.
	up.panicUpTo = 2
	time.Sleep(40 * time.Millisecond)

	// (A'): serves stale immediately and kicks the detached refresh.
	rows, err := c.LatestTradePerSource(context.Background(), p, "")
	if err != nil || len(rows) != 1 {
		t.Fatalf("stale serve = (%d rows, %v), want the retained stale value", len(rows), err)
	}
	if !waitFor(2*time.Second, func() bool { return up.calls.Load() >= 2 }) {
		t.Fatal("the detached stale refresh never ran")
	}

	// The marker must be clear, so a later expiry can refresh again.
	time.Sleep(40 * time.Millisecond)
	if _, err := c.LatestTradePerSource(context.Background(), p, ""); err != nil {
		t.Fatalf("post-panic serve: %v", err)
	}
	if !waitFor(2*time.Second, func() bool { return up.calls.Load() >= 3 }) {
		t.Fatal("no THIRD upstream call — the panicked refresh left entry.flight set, " +
			"so this key would serve its stale value for the life of the process")
	}
}
