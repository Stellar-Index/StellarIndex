package v1

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// histStaleLedger / histFreshLedger are what tell the trade a cold
// fill cached from the trade a refresh replaces it with: the fake
// moves the latest trade forward a ledger on every call after the
// first, which is what a "latest trade per source" read moving on
// looks like. The stub used to return one fixed row, identical before
// and after a refresh, so nothing but a stopwatch separated a caller
// served the cached entry from one handed the refresh's result.
const (
	histStaleLedger = 100
	histFreshLedger = 200
)

// fakeHist is a HistoryReader whose only real method is
// LatestTradePerSource. The embedded interface is nil — every other
// method would panic, but these tests only exercise the cached one.
// Call accounting is atomic and the failure toggle is keyed on call
// number (no mid-test field mutation) so `go test -race` stays
// clean under the concurrent detached fill. An optional hold parks a
// call inside the fake until the test releases it, which is how the
// tests below order a caller against a fill still in flight instead
// of racing one against the other on a clock.
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

// expireHistoryEntries walks every cached entry back past any ttl, so
// the next read takes the stale-while-revalidate branch. It replaces
// sleeping past a deliberately tiny ttl: the tests below need the
// entry a refresh WRITES to stay fresh while they assert on it, which
// a 25 ms ttl cannot promise, and the positions cache tests seed their
// entry directly for the same reason.
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

// coldCallerDeadline is the request budget the cold caller below runs
// out of. It is a stimulus rather than a bound: the fill it races is
// held open until after the caller has returned, so ANY finite
// deadline expires first, and nothing asserts on this number.
const coldCallerDeadline = 30 * time.Millisecond

// TestCachedHistoryReader_DetachedColdFillWarms is the #29 core: a
// cold caller whose ctx deadline is shorter than the upstream query
// gets ctx.Err() (the handler 503s) while the fill it started keeps
// running on its OWN budget, so the next poll is served from the entry
// that fill wrote — and the timed-out caller never spawns a second
// upstream call.
//
// Both halves are claims about the ORDER of two events, and both used
// to be raced rather than ordered, so the test failed for reasons that
// had nothing to do with the cache — red 2 of 40 runs under CPU load
// against an unchanged cache, once on each half, and the two failures
// are different diseases:
//
//   - The upstream slept 200 ms against a 30 ms request ctx, so under
//     starvation the caller was rescheduled with BOTH arms of its
//     select ready and Go picked between them uniformly: it returned
//     the fill's rows and a nil error, and the test failed asserting a
//     deadline it never got. That is not a threshold that can be
//     widened, it is a coin toss.
//   - "Returned at its own ctx" was a 120 ms ceiling on wall time,
//     which measures how quickly the runner rescheduled the caller
//     after its deadline had already fired. It read 329.57 ms in that
//     window and 606 ms in an earlier one.
//
// The fill is now HELD inside the store until the test lets it go, so
// the order is constructed instead of raced: the fill has provably not
// completed, its flight channel has provably not closed, and the only
// edge the caller can wake on is its own expired deadline. A cache
// that waited for the fill does not return late, it does not return at
// all, and it fails on the sentence that says so.
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

// TestCachedHistoryReader_SWRServesStaleSingleFlight: every one of 20
// concurrent reads of an expired entry is served THE STALE TRADE while
// exactly one single-flighted detached refresh runs, and the trade
// that refresh returns is what replaces it.
//
// What a stale read is served is a value, so it is asserted as one.
// The fake used to return one fixed row, so the trade before the
// refresh and the trade after it were the same trade, and no assertion
// here could tell a read served the entry from a read handed the
// refresh's result — the 120 ms ceiling on each reader was the whole
// evidence, and what it measured was how quickly the runner
// rescheduled 20 goroutines. The fake now moves the latest trade
// forward a ledger on the refresh, and the refresh is HELD until every
// read has returned, so a read that waited for it could only be
// holding ledger 200 and a read served the entry can only be holding
// ledger 100 — on any runner at any load.
//
// The tail releases the hold and pins the refreshed trade, which is
// what keeps the assertion above a discrimination rather than a
// restatement of the fixture.
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

	// Released, the one refresh settles the entry. Polling by READ adds
	// no upstream call of its own: a stale read looks at the in-flight
	// marker, which the refresh holds until it has written what it
	// found, so every poll before that point is served the stale entry
	// and the poll that succeeds is served the refreshed one. Because
	// the ttl outlives the test, the entry then stays fresh and the
	// call count is final.
	hold.release()
	var fresh []canonical.Trade
	var freshErr error
	if !waitFor(2*time.Second, func() bool {
		fresh, freshErr = c.LatestTradePerSource(context.Background(), p, "")
		return freshErr == nil && len(fresh) == 1 && fresh[0].Ledger == histFreshLedger
	}) {
		t.Fatalf("after the refresh: rows=%d err=%v, want 1 row at the refreshed ledger %d", len(fresh), freshErr, histFreshLedger)
	}
	// Nothing further may reach the upstream: the refresh re-stamped
	// the entry and the ttl outlives this test, so the read above was a
	// hit rather than a second stale serve. See upstreamSettleWindow
	// for why an absence gets a window and why that window fails
	// nothing correct.
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
