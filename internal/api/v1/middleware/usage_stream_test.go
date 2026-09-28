package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

func streamUsageTotal(t *testing.T, counter *usage.Counter, subject auth.Subject) int64 {
	t.Helper()
	// The stream-open counter write runs on the shared after-response
	// pool, not inline (GH-627), so a read right after the headers commit
	// must wait for it to land first.
	if !middleware.AfterResponseDrainForTest(afterResponseTestTimeout) {
		t.Fatal("after-response pool did not drain in time")
	}
	days, err := counter.Read(context.Background(), middleware.UsageKeyForSubject(subject), 3)
	if err != nil {
		t.Fatalf("counter.Read: %v", err)
	}
	var total int64
	for _, d := range days {
		total += d.Requests
	}
	return total
}

// runOpenStream serves one SSE request whose handler commits status, then
// blocks until the returned release func is called. It returns once the
// headers are committed; wait blocks until the handler has returned.
func runOpenStream(t *testing.T, counter *usage.Counter, subject auth.Subject, status int) (release, wait func()) {
	t.Helper()
	opened := make(chan struct{})
	unblock := make(chan struct{})
	done := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/price/tip/stream", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(status)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(opened)
		<-unblock
	})
	stamp := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), subject)))
		})
	}
	h := middleware.Chain(mux, stamp, middleware.UsageTracker(counter, nil))
	go func() {
		defer close(done)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/price/tip/stream", nil))
	}()
	select {
	case <-opened:
	case <-time.After(5 * time.Second):
		t.Fatal("stream handler never committed its headers")
	}
	var released bool
	release = func() {
		if !released {
			released = true
			close(unblock)
		}
	}
	t.Cleanup(release)
	wait = func() {
		release()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("stream handler never returned")
		}
	}
	return release, wait
}

// TestUsageTracker_StreamCountedAtOpen pins GH-1279: an SSE stream is
// exempt from RequestTimeout, so it can stay open across a day boundary or
// be killed by a deploy without ever returning. Counting it only at close
// billed it on the wrong day or not at all. It must be counted once, when
// the stream opens, and not again when it closes.
func TestUsageTracker_StreamCountedAtOpen(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	var now atomic.Int64
	now.Store(time.Date(2026, 9, 27, 23, 59, 0, 0, time.UTC).Unix())
	counter := usage.New(rdb, usage.WithClock(func() time.Time { return time.Unix(now.Load(), 0) }))
	subject := auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_stream", Identifier: "acct:stream"}

	_, wait := runOpenStream(t, counter, subject, http.StatusOK)

	if got := streamUsageTotal(t, counter, subject); got != 1 {
		t.Fatalf("usage total while stream is open = %d, want 1 — an open SSE stream must be "+
			"metered when it opens, not only when (and if) it closes", got)
	}
	now.Store(time.Date(2026, 9, 28, 0, 30, 0, 0, time.UTC).Unix())
	wait()

	days, err := counter.Read(context.Background(), middleware.UsageKeyForSubject(subject), 3)
	if err != nil {
		t.Fatalf("counter.Read: %v", err)
	}
	perDay := map[string]int64{}
	for _, d := range days {
		perDay[d.Date] = d.Requests
	}
	if perDay["2026-09-27"] != 1 || perDay["2026-09-28"] != 0 {
		t.Errorf("per-day usage = %v, want exactly 1 on the open day 2026-09-27 and 0 on the "+
			"close day — a stream is one request, counted once when it opens", perDay)
	}
}

// TestUsageTracker_StreamRefusedAtOpenNotBilled guards that metering at open
// still classifies by the committed status: a stream refused with 429 must
// not eat monthly quota (COR-05), while the detail family still sees it.
func TestUsageTracker_StreamRefusedAtOpenNotBilled(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	counter := usage.New(rdb)
	subject := auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_stream429", Identifier: "acct:stream429"}

	_, wait := runOpenStream(t, counter, subject, http.StatusTooManyRequests)
	wait()

	if got := streamUsageTotal(t, counter, subject); got != 0 {
		t.Errorf("billable usage total for a 429-refused stream = %d, want 0", got)
	}
	rows, err := counter.ScanDetail(context.Background(), []string{time.Now().UTC().Format("2006-01-02")})
	if err != nil {
		t.Fatalf("ScanDetail: %v", err)
	}
	var throttled int64
	for _, r := range rows {
		if r.Class == usage.ClassThrottled {
			throttled += r.Count
		}
	}
	if throttled != 1 {
		t.Errorf("throttled detail count = %d, want 1 (rows %+v)", throttled, rows)
	}
}

// TestUsageTracker_StreamTickCountBounded pins the rest of GH-1279:
// counting an SSE stream once at open (TestUsageTracker_StreamCountedAtOpen)
// still lets it buy unbounded duration for that one unit. A stream held
// open for a known duration must accrue additional billable units for
// each meter interval it survives (never starved) but not more than
// that plus scheduling slack (never double-billed).
func TestUsageTracker_StreamTickCountBounded(t *testing.T) {
	const interval = 20 * time.Millisecond
	t.Cleanup(middleware.SetStreamMeterIntervalForTest(interval))

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	counter := usage.New(rdb)
	subject := auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_stream_tick", Identifier: "acct:stream_tick"}
	id := middleware.UsageKeyForSubject(subject)

	_, wait := runOpenStream(t, counter, subject, http.StatusOK)

	const wantTicks = 5
	time.Sleep(wantTicks * interval)
	wait()

	// Ticks write synchronously on their own goroutine (see
	// meterOpenStream), so MonthToDate is read directly once the
	// stream has closed rather than through streamUsageTotal's
	// after-response drain.
	got, err := counter.MonthToDate(context.Background(), id)
	if err != nil {
		t.Fatalf("MonthToDate: %v", err)
	}
	const (
		wantMin = int64(1) // the open bill alone, if ticks never fired
		slack   = int64(2) // scheduling jitter, either direction
	)
	wantMax := int64(1+wantTicks) + slack
	if got < wantMin || got > wantMax {
		t.Fatalf("usage total for a stream held open ~%d meter intervals = %d, want in [%d, %d] "+
			"(below min: periodic re-metering never advanced it; above max: a tick double-billed)",
			wantTicks, got, wantMin, wantMax)
	}
}

// TestUsageTracker_StreamNoTicksAfterClose guards meterOpenStream's
// shutdown: once a stream's handler has returned, no further tick may
// land, however many meter intervals later something looks again.
//
// Verified adversarially (1µs interval, 3000 closes) before the
// defer-reorder + non-blocking done re-check in meterOpenStream: the
// ticker's own shutdown could still race a simultaneously-ready tick
// and leak ~1 extra billable unit per 3000 closes. That residual
// select-fairness tie is not fully eliminable short of a mutex
// serializing every tick against close (disproportionate given it
// requires a tick to land within nanoseconds of close even at a 1µs
// interval, and is irrelevant at the real 1-minute production
// interval); this test uses a realistic interval, at which it does
// not flake.
func TestUsageTracker_StreamNoTicksAfterClose(t *testing.T) {
	const interval = 5 * time.Millisecond
	t.Cleanup(middleware.SetStreamMeterIntervalForTest(interval))

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	counter := usage.New(rdb)
	subject := auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_stream_close", Identifier: "acct:stream_close"}
	id := middleware.UsageKeyForSubject(subject)

	_, wait := runOpenStream(t, counter, subject, http.StatusOK)

	// Let at least one periodic tick land before closing, so this
	// test exercises meterOpenStream's loop, not just the open bill.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got, err := counter.MonthToDate(context.Background(), id)
		if err != nil {
			t.Fatalf("MonthToDate: %v", err)
		}
		if got >= 2 {
			break
		}
		time.Sleep(interval)
	}
	wait()

	closedTotal, err := counter.MonthToDate(context.Background(), id)
	if err != nil {
		t.Fatalf("MonthToDate: %v", err)
	}

	// Several more meter intervals, well after the stream closed and
	// with no request in flight.
	time.Sleep(10 * interval)

	after, err := counter.MonthToDate(context.Background(), id)
	if err != nil {
		t.Fatalf("MonthToDate: %v", err)
	}
	if after != closedTotal {
		t.Fatalf("usage total grew from %d to %d after the stream's handler returned — "+
			"a tick fired after close", closedTotal, after)
	}
}

// TestUsageTracker_StreamPanicBeforeOpenBillsNothing pins the
// defer-ordering hazard fixed alongside the periodic re-meter: a
// handler that panics before ever writing anything never opens the
// stream (once.fired() stays false the whole time it runs), so
// UsageTracker's recover defer is the FIRST call to once.fire —
// classing the request as a platform 5xx that COR-05 forbids billing.
//
// Before the fix, that recover defer ran BEFORE the streaming block's
// close(done) (defers are LIFO and close(done) was registered first,
// hence ran second), so meterOpenStream's ticker kept running for a
// window after once.fire had already flipped fired()==true. A tick
// landing in that window read rec's still-default 200 status (a panic
// never sets it) and billed it as OK — eating quota on a request this
// same defer had just classed as non-billable. A 1µs meter interval
// plus many trials makes the (otherwise sub-microsecond) window
// observable: pre-fix this leaked on 113 of 3000 trials in
// development, post-fix 0.
func TestUsageTracker_StreamPanicBeforeOpenBillsNothing(t *testing.T) {
	t.Cleanup(middleware.SetStreamMeterIntervalForTest(time.Microsecond))

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	counter := usage.New(rdb)
	subject := auth.Subject{Tier: auth.TierAPIKey, KeyID: "kid_stream_panic", Identifier: "acct:stream_panic"}
	id := middleware.UsageKeyForSubject(subject)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/price/tip/stream", func(http.ResponseWriter, *http.Request) {
		panic("boom before open")
	})
	stamp := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), subject)))
		})
	}
	// Recoverer OUTSIDE UsageTracker, matching server.go's stack order.
	h := middleware.Chain(mux, stamp, middleware.Recoverer(nil), middleware.UsageTracker(counter, nil))

	const trials = 500
	for i := 0; i < trials; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/price/tip/stream", nil))
	}
	// Give any still-running per-request ticker goroutine a moment to
	// (mis)fire before reading — at a 1µs interval they exit almost
	// immediately once done closes, but "almost" is exactly the gap
	// under test.
	time.Sleep(50 * time.Millisecond)
	if !middleware.AfterResponseDrainForTest(afterResponseTestTimeout) {
		t.Fatal("after-response pool did not drain in time")
	}

	got, err := counter.MonthToDate(context.Background(), id)
	if err != nil {
		t.Fatalf("MonthToDate: %v", err)
	}
	if got != 0 {
		t.Fatalf("billable usage total after %d panic-before-open streams = %d, want 0 "+
			"(a tick landed between once.fire and close(done) and billed rec's still-default "+
			"200 status — COR-05 violation)", trials, got)
	}
}
