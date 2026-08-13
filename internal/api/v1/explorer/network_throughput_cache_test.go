package explorer

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// These tests pin the §2.6b (2026-08-13) contract for
// GET /v1/network/throughput: the year-window FINAL scan never runs on a
// request deadline again — a warm entry is sliced, a stale one is SERVED
// (flags.stale + its real as_of) while ONE detached refresh runs, a
// saturated gate degrades to stale rather than erroring, and prewarm
// warms the exact entry the handler reads.

// throughputReader is a capReader whose NetworkThroughput is countable,
// controllable, and records the window argument each caller passed (the
// prewarm/handler parity assertion).
type throughputReader struct {
	*capReader

	mu      sync.Mutex
	windows []int

	calls atomic.Int32
	fail  atomic.Bool
	days  atomic.Int32  // buckets to return (default networkThroughputMaxWindowDays)
	block chan struct{} // when non-nil, the compute waits for it
}

func (r *throughputReader) NetworkThroughput(_ context.Context, windowDays int) ([]clickhouse.ThroughputBucket, error) {
	r.calls.Add(1)
	r.mu.Lock()
	r.windows = append(r.windows, windowDays)
	r.mu.Unlock()
	if r.block != nil {
		<-r.block
	}
	if r.fail.Load() {
		return nil, errors.New("boom")
	}
	n := int(r.days.Load())
	if n == 0 {
		n = networkThroughputMaxWindowDays
	}
	return throughputSeries(n), nil
}

func (r *throughputReader) recordedWindows() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.windows...)
}

// throughputSeries builds `n` ascending daily buckets ending today, each
// carrying its day index so a slice can be identified positionally.
func throughputSeries(n int) []clickhouse.ThroughputBucket {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	out := make([]clickhouse.ThroughputBucket, n)
	for i := range out {
		day := today.AddDate(0, 0, -(n - 1 - i))
		out[i] = clickhouse.ThroughputBucket{
			Day: day, Ledgers: int64(i + 1), Txs: int64(i), Ops: int64(i), Events: int64(i),
			Partial: day.Equal(today),
		}
	}
	return out
}

func newThroughputHandler() (*Handler, *throughputReader) {
	reader := &throughputReader{capReader: &capReader{probe: &deadlineProbe{}}}
	h := &Handler{
		Reader: reader,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return h, reader
}

func TestNetworkThroughputCached_ColdComputesOnceThenServesWarm(t *testing.T) {
	h, reader := newThroughputHandler()

	buckets, asOf, degraded, err := h.networkThroughputCached(context.Background(), 30)
	if err != nil || degraded || len(buckets) != 30 {
		t.Fatalf("cold fill: buckets=%d degraded=%v err=%v, want 30 fresh buckets", len(buckets), degraded, err)
	}
	if asOf.IsZero() {
		t.Error("cold fill returned a zero as_of, want the entry's real computation time")
	}
	if got := reader.calls.Load(); got != 1 {
		t.Fatalf("cold fill ran %d scans, want 1", got)
	}

	// Warm: served from the same entry, no recompute — including for a
	// DIFFERENT window, which slices the same max-window entry.
	if _, _, _, err := h.networkThroughputCached(context.Background(), 90); err != nil {
		t.Fatalf("warm read: %v", err)
	}
	if got := reader.calls.Load(); got != 1 {
		t.Errorf("warm read recomputed (%d scans) — the cache must be window-independent", got)
	}
}

func TestNetworkThroughputCached_StaleServedDegradedAndRefreshed(t *testing.T) {
	h, reader := newThroughputHandler()
	staleAt := time.Now().Add(-2 * networkThroughputTTL)
	h.throughput.mu.Lock()
	h.throughput.entry = networkThroughputEntry{buckets: throughputSeries(40), cachedAt: staleAt}
	h.throughput.has = true
	h.throughput.mu.Unlock()

	buckets, asOf, degraded, err := h.networkThroughputCached(context.Background(), 30)
	if err != nil || len(buckets) != 30 {
		t.Fatalf("stale serve: buckets=%d err=%v, want the stale entry sliced to 30", len(buckets), err)
	}
	if !degraded || !asOf.Equal(staleAt) {
		t.Errorf("stale serve: degraded=%v asOf=%v, want degraded=true carrying %v", degraded, asOf, staleAt)
	}

	waitFlightIdle(t, &h.throughput.flight, networkThroughputFlightKey)
	if got := reader.calls.Load(); got != 1 {
		t.Errorf("detached refresh ran %d scans, want exactly 1", got)
	}
	if _, ok, fresh := h.throughput.get(); !ok || !fresh {
		t.Errorf("after detached refresh: ok=%v fresh=%v, want a fresh entry", ok, fresh)
	}
}

func TestNetworkThroughputCached_FailedRefreshKeepsStaleEntry(t *testing.T) {
	h, reader := newThroughputHandler()
	reader.fail.Store(true)
	staleAt := time.Now().Add(-2 * networkThroughputTTL)
	h.throughput.mu.Lock()
	h.throughput.entry = networkThroughputEntry{buckets: throughputSeries(40), cachedAt: staleAt}
	h.throughput.has = true
	h.throughput.mu.Unlock()

	buckets, _, degraded, err := h.networkThroughputCached(context.Background(), 30)
	if err != nil || !degraded || len(buckets) != 30 {
		t.Fatalf("stale serve under failing refresh: buckets=%d degraded=%v err=%v", len(buckets), degraded, err)
	}
	waitFlightIdle(t, &h.throughput.flight, networkThroughputFlightKey)
	e, ok, _ := h.throughput.get()
	if !ok || len(e.buckets) != 40 || !e.cachedAt.Equal(staleAt) {
		t.Error("a failed refresh blanked the previous entry — old-but-real must survive")
	}
}

// TestNetworkThroughputCached_SingleFlightCollapsesColdCallers pins the
// property that makes the cold path safe under a burst: N concurrent
// callers on an empty cache launch ONE scan, not N.
func TestNetworkThroughputCached_SingleFlightCollapsesColdCallers(t *testing.T) {
	h, reader := newThroughputHandler()
	reader.block = make(chan struct{})

	const callers = 8
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, _, errs[i] = h.networkThroughputCached(context.Background(), 30)
		}()
	}
	// Let every caller reach the flight before the compute returns.
	time.Sleep(50 * time.Millisecond)
	close(reader.block)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
	}
	if got := reader.calls.Load(); got != 1 {
		t.Fatalf("%d concurrent cold callers ran %d scans, want exactly 1", callers, got)
	}
}

// TestNetworkThroughputCached_SaturatedGateServesStale pins the
// backpressure contract: with the shared detached-refresh gate full the
// refresh is SKIPPED (not queued), a cached entry keeps serving, and only
// a stone-cold process surfaces the retryable saturation sentinel.
func TestNetworkThroughputCached_SaturatedGateServesStale(t *testing.T) {
	h, reader := newThroughputHandler()
	gate := h.detachedGate()
	held := 0
	for gate.TryAcquire() {
		held++
		if held > 1000 {
			t.Fatal("gate never saturates")
		}
	}

	staleAt := time.Now().Add(-2 * networkThroughputTTL)
	h.throughput.mu.Lock()
	h.throughput.entry = networkThroughputEntry{buckets: throughputSeries(40), cachedAt: staleAt}
	h.throughput.has = true
	h.throughput.mu.Unlock()

	buckets, asOf, degraded, err := h.networkThroughputCached(context.Background(), 30)
	if err != nil {
		t.Fatalf("saturated gate with a cached entry returned %v, want the stale entry", err)
	}
	if len(buckets) != 30 || !degraded || !asOf.Equal(staleAt) {
		t.Errorf("saturated stale serve: buckets=%d degraded=%v asOf=%v", len(buckets), degraded, asOf)
	}
	if got := reader.calls.Load(); got != 0 {
		t.Fatalf("saturated gate still launched %d scans", got)
	}

	// Stone-cold under saturation: honest retryable miss, still no scan.
	h.throughput.mu.Lock()
	h.throughput.has = false
	h.throughput.mu.Unlock()
	if _, _, _, err := h.networkThroughputCached(context.Background(), 30); !errors.Is(err, errRefreshSaturated) {
		t.Fatalf("cold miss under saturation err = %v, want errRefreshSaturated (retryable 503)", err)
	}
	if got := reader.calls.Load(); got != 0 {
		t.Errorf("saturated gate launched %d scans on the cold path", got)
	}
}

// TestNetworkThroughputCached_SlicesTailOfMaxWindow proves the
// one-entry-serves-every-window design: an N-day request is the LAST N
// buckets of the cached max-window series (identical to what a direct
// N-day query returns), and an over-wide request gets everything cached.
func TestNetworkThroughputCached_SlicesTailOfMaxWindow(t *testing.T) {
	h, _ := newThroughputHandler()
	full := throughputSeries(networkThroughputMaxWindowDays)
	h.throughput.put(full)

	got, _, _, err := h.networkThroughputCached(context.Background(), 30)
	if err != nil {
		t.Fatalf("slice read: %v", err)
	}
	want := full[len(full)-30:]
	if len(got) != 30 || !got[0].Day.Equal(want[0].Day) || !got[29].Day.Equal(want[29].Day) {
		t.Fatalf("30d slice = %d buckets [%v … %v], want the tail [%v … %v]",
			len(got), got[0].Day, got[len(got)-1].Day, want[0].Day, want[29].Day)
	}
	all, _, _, _ := h.networkThroughputCached(context.Background(), networkThroughputMaxWindowDays)
	if len(all) != networkThroughputMaxWindowDays {
		t.Errorf("max-window read = %d buckets, want %d", len(all), networkThroughputMaxWindowDays)
	}
}

// TestPrewarmNetworkThroughput_WarmsTheEntryTheHandlerReads is the
// arg-parity guard (feedback_prewarm_handler_drift): the prewarm pass and
// the request path must warm/read the SAME entry with the SAME window
// argument, or the prewarm is a phantom that warms a slot nobody hits.
func TestPrewarmNetworkThroughput_WarmsTheEntryTheHandlerReads(t *testing.T) {
	h, reader := newThroughputHandler()

	h.PrewarmNetworkThroughput(context.Background())
	waitFlightIdle(t, &h.throughput.flight, networkThroughputFlightKey)
	if got := reader.calls.Load(); got != 1 {
		t.Fatalf("prewarm ran %d scans, want 1", got)
	}

	// The request path finds it warm — no second scan, no cold wait.
	buckets, _, degraded, err := h.networkThroughputCached(context.Background(), 30)
	if err != nil || degraded || len(buckets) != 30 {
		t.Fatalf("post-prewarm read: buckets=%d degraded=%v err=%v, want 30 fresh", len(buckets), degraded, err)
	}
	if got := reader.calls.Load(); got != 1 {
		t.Errorf("post-prewarm read recomputed (%d scans) — prewarm warmed a different key", got)
	}

	// Every compute, whoever kicked it, used the max window.
	for i, w := range reader.recordedWindows() {
		if w != networkThroughputMaxWindowDays {
			t.Errorf("compute %d used window_days=%d, want %d", i, w, networkThroughputMaxWindowDays)
		}
	}

	// Warm entry → prewarm is a no-op.
	h.PrewarmNetworkThroughput(context.Background())
	waitFlightIdle(t, &h.throughput.flight, networkThroughputFlightKey)
	if got := reader.calls.Load(); got != 1 {
		t.Errorf("prewarm on a fresh entry re-ran the scan (%d)", got)
	}

	// Nil reader / cancelled context: safe no-ops.
	(&Handler{}).PrewarmNetworkThroughput(context.Background())
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	h.PrewarmNetworkThroughput(cancelled)
}

// TestNetworkThroughput_PartialDecidedAtServeTime pins the midnight-cross
// correctness of the cached series: `partial` describes the bucket at
// SERVE time, so an entry computed yesterday does not keep advertising a
// now-complete day as still accumulating.
func TestNetworkThroughput_PartialDecidedAtServeTime(t *testing.T) {
	h, _ := newThroughputHandler()
	h.ParseWindowDays = func(_ *http.Request, def int) int { return def }
	h.ClientAborted = func(*http.Request, error) bool { return false }
	h.WriteProblem = func(w http.ResponseWriter, _ *http.Request, _, _ string, status int, _ string) {
		w.WriteHeader(status)
	}
	var got NetworkThroughputView
	h.WriteJSONAt = func(w http.ResponseWriter, data any, _ bool, _ time.Time) {
		got, _ = data.(NetworkThroughputView)
		w.WriteHeader(http.StatusOK)
	}

	// An entry computed BEFORE midnight: its newest bucket is yesterday,
	// and it was flagged partial at compute time.
	yesterday := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	h.throughput.put([]clickhouse.ThroughputBucket{
		{Day: yesterday.AddDate(0, 0, -1), Ledgers: 1},
		{Day: yesterday, Ledgers: 2, Partial: true},
	})

	h.NetworkThroughput(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/network/throughput", nil))

	if len(got.Buckets) != 2 {
		t.Fatalf("served %d buckets, want 2", len(got.Buckets))
	}
	if got.Buckets[1].Partial {
		t.Error("yesterday still flagged partial — `partial` must be decided at serve time, not read from the cached bucket")
	}
}
