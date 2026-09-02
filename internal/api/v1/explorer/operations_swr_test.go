package explorer

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// These pin the #444 / #332 F2 serving contract for the GET /v1/operations
// DIRECTORY first page. Measured cause: `opsDirTTL` was 3s and the cache was
// fill-on-miss, so at r1's real arrival rate nearly every hit missed and paid
// the (then unbounded) 1.8s lake read on the request deadline — 6h p95
// 2.238s. The fix bounds the read (storage/clickhouse) AND makes this cache
// serve stale while ONE detached rebuild runs, so a visitor never waits on a
// rebuild once any entry exists.

// opsDirReader is a capReader whose RecentOperations is countable and
// controllable — it can block, fail, and return a chosen page.
type opsDirReader struct {
	*capReader

	calls atomic.Int32
	fail  atomic.Bool
	seq   atomic.Uint32 // ledger stamped on the returned row (identifies the fill)
	block chan struct{} // when non-nil, the read waits for it
}

func (r *opsDirReader) RecentOperations(_ context.Context, _ int, _ clickhouse.ExplorerCursor) ([]clickhouse.OpRow, error) {
	r.calls.Add(1)
	if r.block != nil {
		<-r.block
	}
	if r.fail.Load() {
		return nil, errors.New("boom")
	}
	return []clickhouse.OpRow{{
		Seq: r.seq.Load(), CloseTime: time.Unix(1700000000, 0).UTC(),
		TxHash: "h", TxIndex: 0, OpIndex: 0, OpType: "OperationTypePayment",
	}}, nil
}

// writeCapture records what the handler asked the envelope writer for.
type writeCapture struct {
	calls int
	stale bool
	asOf  time.Time
	view  OperationsView
}

func newOpsDirHandler() (*Handler, *opsDirReader, *writeCapture) {
	reader := &opsDirReader{capReader: &capReader{probe: &deadlineProbe{}}}
	reader.seq.Store(63_000_000)
	captured := &writeCapture{}
	h := newProbeHandler(reader, nil)
	h.WriteJSON = func(w http.ResponseWriter, data any, stale bool) {
		captured.calls++
		captured.stale = stale
		captured.asOf = time.Time{}
		if v, ok := data.(OperationsView); ok {
			captured.view = v
		}
		w.WriteHeader(http.StatusOK)
	}
	h.WriteJSONAt = func(w http.ResponseWriter, data any, stale bool, asOf time.Time) {
		captured.calls++
		captured.stale = stale
		captured.asOf = asOf
		if v, ok := data.(OperationsView); ok {
			captured.view = v
		}
		w.WriteHeader(http.StatusOK)
	}
	return h, reader, captured
}

func getOperations(t *testing.T, h *Handler) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.Operations(rec, httptest.NewRequest(http.MethodGet, "/v1/operations", nil))
	return rec
}

// waitForCalls polls until the reader has been entered n times (the detached
// refresh runs on its own goroutine).
func waitForCalls(t *testing.T, r *opsDirReader, n int32) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if r.calls.Load() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("reader entered %d times, want %d", r.calls.Load(), n)
}

// A cold first page fills inline; a second hit inside the TTL is served from
// the entry with no further lake read (unchanged behaviour, pinned so the
// stale-serve work cannot regress the warm path).
func TestOperationsDirectory_WarmHitDoesNotReadTheLake(t *testing.T) {
	h, reader, captured := newOpsDirHandler()

	if got := getOperations(t, h).Code; got != http.StatusOK {
		t.Fatalf("cold status = %d, want 200", got)
	}
	if got := reader.calls.Load(); got != 1 {
		t.Fatalf("cold fill made %d reads, want 1", got)
	}
	if captured.stale {
		t.Error("a freshly-filled page was served with flags.stale")
	}

	if got := getOperations(t, h).Code; got != http.StatusOK {
		t.Fatalf("warm status = %d, want 200", got)
	}
	if got := reader.calls.Load(); got != 1 {
		t.Errorf("warm hit made %d reads, want 1 (served from cache)", got)
	}
}

// THE regression: an entry past opsDirTTL must be SERVED — immediately, with
// flags.stale and its real as_of — while exactly one DETACHED rebuild runs.
// Pre-fix the expired entry read as a miss and the visitor waited on the lake
// read on the request deadline.
func TestOperationsDirectory_StaleEntryIsServedWhileOneDetachedRefreshRuns(t *testing.T) {
	h, reader, captured := newOpsDirHandler()

	// Seed a STALE entry with a payload identifiable on the wire.
	staleAt := time.Now().Add(-3 * opsDirTTL)
	seeded := OperationsView{
		NextCursor: "stale-cursor",
		Operations: []OpView{{Ledger: 61_000_000, TxHash: "seeded"}},
	}
	h.opsDir.mu.Lock()
	h.opsDir.entries = map[int]opsDirEntry{50: {view: seeded, cachedAt: staleAt}}
	h.opsDir.mu.Unlock()

	// Hold the rebuild open so the request cannot possibly be answered by it.
	reader.block = make(chan struct{})
	reader.seq.Store(63_500_000)

	done := make(chan struct{})
	go func() {
		defer close(done)
		if got := getOperations(t, h).Code; got != http.StatusOK {
			t.Errorf("stale-serve status = %d, want 200", got)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("request BLOCKED on the rebuild — the stale entry was not served (this is the #332 F2 defect)")
	}

	if !captured.stale {
		t.Error("stale entry served without flags.stale — the response claims to be current")
	}
	if !captured.asOf.Equal(staleAt) {
		t.Errorf("served as_of = %v, want the entry's real fill time %v", captured.asOf, staleAt)
	}
	if captured.view.NextCursor != "stale-cursor" || len(captured.view.Operations) != 1 ||
		captured.view.Operations[0].TxHash != "seeded" {
		t.Errorf("served payload is not the cached entry: %+v", captured.view)
	}

	// Exactly one detached rebuild was kicked; further stale hits join it.
	getOperations(t, h)
	getOperations(t, h)
	close(reader.block)
	waitForCalls(t, reader, 1)
	time.Sleep(50 * time.Millisecond)
	if got := reader.calls.Load(); got != 1 {
		t.Errorf("stale hits kicked %d rebuilds, want 1 (single-flight)", got)
	}

	// …and the rebuild replaced the entry, so the next hit is fresh and
	// carries the NEW page, not the seeded one.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, _, fresh := h.opsDir.get(50); fresh {
			break
		}
		time.Sleep(time.Millisecond)
	}
	getOperations(t, h)
	if captured.stale {
		t.Error("page served stale after a successful rebuild")
	}
	if len(captured.view.Operations) != 1 || captured.view.Operations[0].Ledger != 63_500_000 {
		t.Errorf("rebuilt page did not replace the seeded entry: %+v", captured.view)
	}
}

// A failing rebuild must keep the previous entry — old-but-real, disclosed via
// flags.stale — rather than blanking the directory or 500ing.
func TestOperationsDirectory_FailedRefreshKeepsServingTheLastGoodPage(t *testing.T) {
	h, reader, captured := newOpsDirHandler()

	staleAt := time.Now().Add(-3 * opsDirTTL)
	seeded := OperationsView{NextCursor: "last-good", Operations: []OpView{{Ledger: 61_000_000}}}
	h.opsDir.mu.Lock()
	h.opsDir.entries = map[int]opsDirEntry{50: {view: seeded, cachedAt: staleAt}}
	h.opsDir.mu.Unlock()
	reader.fail.Store(true)

	if got := getOperations(t, h).Code; got != http.StatusOK {
		t.Fatalf("status = %d, want 200 (last-good served)", got)
	}
	waitForCalls(t, reader, 1)
	time.Sleep(50 * time.Millisecond)

	rec := getOperations(t, h)
	if rec.Code != http.StatusOK {
		t.Fatalf("status after a failed rebuild = %d, want 200", rec.Code)
	}
	if !captured.stale {
		t.Error("last-good page served without flags.stale")
	}
	if captured.view.NextCursor != "last-good" {
		t.Errorf("last-good page lost after a failed rebuild: %+v", captured.view)
	}
}

// A cursor page is never cached and never stale-served — it must still read
// through to the lake on every request.
func TestOperationsDirectory_CursorPageBypassesTheCache(t *testing.T) {
	h, reader, _ := newOpsDirHandler()

	for range 2 {
		rec := httptest.NewRecorder()
		h.Operations(rec, httptest.NewRequest(http.MethodGet, "/v1/operations?cursor=63000000.1.0", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("cursor page status = %d, want 200", rec.Code)
		}
	}
	if got := reader.calls.Load(); got != 2 {
		t.Errorf("cursor pages made %d reads, want 2 (never cached)", got)
	}
	if _, ok, _ := h.opsDir.get(50); ok {
		t.Error("a cursor page populated the first-page cache")
	}
}
