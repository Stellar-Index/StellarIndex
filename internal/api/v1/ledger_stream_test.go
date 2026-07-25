package v1_test

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// advancingCursorsReader returns a ledgerstream cursor whose ledger
// increments by 1 on every ListCursors call — simulating the live
// indexer committing a new ledger between producer polls.
type advancingCursorsReader struct {
	mu     sync.Mutex
	ledger uint32
}

func (a *advancingCursorsReader) ListCursors(context.Context) ([]timescale.Cursor, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c := timescale.Cursor{
		Source:     "ledgerstream",
		LastLedger: a.ledger,
		UpdatedAt:  time.Now().UTC(),
	}
	a.ledger++
	return []timescale.Cursor{c}, nil
}

// No CursorsReader wired → 503 BEFORE the response switches into SSE
// mode (an SSE body has no way to carry a non-200 status).
func TestLedgerStream_NoReader_Returns503(t *testing.T) {
	srv := v1.New(v1.Options{})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/ledger/stream")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// Cursors table exists but carries no live ledgerstream row → 503
// pre-flight.
func TestLedgerStream_NoLedgerstreamCursor_Returns503(t *testing.T) {
	srv := v1.New(v1.Options{
		Cursors: &stubCursorsReader{
			rows: []timescale.Cursor{
				mkCursor("backfill", "0-1000:soroswap", 999, time.Hour),
			},
		},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/ledger/stream")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

// The initial ledger_update event lands synchronously on connect —
// the client must not have to wait a full poll interval for first
// data.
func TestLedgerStream_InitialEventSynchronous(t *testing.T) {
	srv := v1.New(v1.Options{
		Cursors: &stubCursorsReader{
			rows: []timescale.Cursor{
				mkCursor("ledgerstream", "", 62685000, 3*time.Second),
			},
		},
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/v1/ledger/stream")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	br := bufio.NewReader(resp.Body)
	data := readTipStreamEvent(t, br, 2*time.Second)
	if data == "" {
		t.Fatal("no event received within 2s — initial emit failed")
	}
	view := decodeLedgerEvent(t, data)
	if view.LatestLedger != 62685000 {
		t.Errorf("latest_ledger = %d, want 62685000", view.LatestLedger)
	}
}

// As the indexer commits new ledgers, the stream emits a fresh
// ledger_update per advance — the second event must carry a higher
// ledger than the first.
func TestLedgerStream_EmitsOnAdvance(t *testing.T) {
	srv := v1.New(v1.Options{Cursors: &advancingCursorsReader{ledger: 1000}})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/ledger/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	first := decodeLedgerEvent(t, readTipStreamEvent(t, br, 2*time.Second))
	second := decodeLedgerEvent(t, readTipStreamEvent(t, br, 4*time.Second))
	if second.LatestLedger <= first.LatestLedger {
		t.Errorf("ledger did not advance: first=%d second=%d",
			first.LatestLedger, second.LatestLedger)
	}
}

// deadlineCapturingCursorsReader advances the ledger like
// advancingCursorsReader, but records whether each ListCursors call
// after the first (the stream's synchronous prelude read, which is
// intentionally unbounded by RequestTimeout on `/stream` paths) was
// given a ctx with a Deadline.
type deadlineCapturingCursorsReader struct {
	mu    sync.Mutex
	calls int
	// tickHadDeadline receives one bool per call after the first —
	// true iff that call's ctx carried a Deadline.
	tickHadDeadline chan bool
}

func (d *deadlineCapturingCursorsReader) ListCursors(ctx context.Context) ([]timescale.Cursor, error) {
	d.mu.Lock()
	d.calls++
	call := d.calls
	d.mu.Unlock()

	if call > 1 {
		_, hasDeadline := ctx.Deadline()
		select {
		case d.tickHadDeadline <- hasDeadline:
		default:
		}
	}

	c := timescale.Cursor{
		Source:     "ledgerstream",
		LastLedger: uint32(1000 + call),
		UpdatedAt:  time.Now().UTC(),
	}
	return []timescale.Cursor{c}, nil
}

// TestLedgerStream_TickIsBoundedByATimeout is the REL-01 regression:
// the per-tick cursors read in the ledger-stream producer must run
// under its OWN bounded deadline, not the raw per-connection context
// (which RequestTimeout deliberately leaves undeadlined on `/stream`
// paths because the connection itself is long-lived by design).
// Without a per-tick bound, a slow ListCursors call could hold the
// producer goroutine — and its DB connection — open indefinitely, once
// per open connection, for as long as the client stays connected.
func TestLedgerStream_TickIsBoundedByATimeout(t *testing.T) {
	reader := &deadlineCapturingCursorsReader{tickHadDeadline: make(chan bool, 1)}
	srv := v1.New(v1.Options{Cursors: reader})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/ledger/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	br := bufio.NewReader(resp.Body)
	// Drain the synchronous initial event (prelude read, call #1 —
	// deliberately not asserted on).
	_ = readTipStreamEvent(t, br, 2*time.Second)
	// Drain the first TICK event (ledgerStreamPollInterval = 2s) so the
	// producer goroutine has actually made its second ListCursors call.
	_ = readTipStreamEvent(t, br, 4*time.Second)

	select {
	case hadDeadline := <-reader.tickHadDeadline:
		if !hadDeadline {
			t.Error("per-tick ListCursors call ran with an undeadlined context — " +
				"a slow/hung read can block the producer indefinitely (REL-01)")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for a per-tick ListCursors call")
	}
}

// capOrderingCursorsReader counts ListCursors calls so a test can
// assert whether the handler's pre-flight compute ran at all.
type capOrderingCursorsReader struct{ calls int32 }

func (c *capOrderingCursorsReader) ListCursors(context.Context) ([]timescale.Cursor, error) {
	atomic.AddInt32(&c.calls, 1)
	return []timescale.Cursor{{Source: "ledgerstream", LastLedger: 1000, UpdatedAt: time.Now().UTC()}}, nil
}

// TestLedgerStream_CapRejectsBeforePreflightCompute is the REL-05
// regression (pre-flight-compute ordering): a client rejected by the
// global concurrency cap must never reach handleLedgerStream's
// synchronous pre-flight ledgerTip read at all. Before the fix, the
// cap was only checked inside StreamFromChannel, AFTER that read had
// already run — so a caller already at the cap still paid for a full
// cursors read on every rejected request, turning the cap into a
// counter rather than an actual admission gate.
func TestLedgerStream_CapRejectsBeforePreflightCompute(t *testing.T) {
	streaming.SetMaxConcurrentStreams(1)
	t.Cleanup(func() { streaming.SetMaxConcurrentStreams(8192) })

	reader := &capOrderingCursorsReader{}
	srv := v1.New(v1.Options{Cursors: reader})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Connection #1 occupies the one available global slot. Do()
	// returns only once headers are flushed, by which point admission,
	// the prelude read, and the switch into SSE mode have all already
	// happened — so the slot is provably held before we fire #2.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel1()
	req1, _ := http.NewRequestWithContext(ctx1, http.MethodGet, ts.URL+"/v1/ledger/stream", nil)
	resp1, err := http.DefaultClient.Do(req1)
	if err != nil {
		t.Fatalf("connection 1: %v", err)
	}
	defer resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("connection 1 status = %d, want 200 (should hold the one available slot)", resp1.StatusCode)
	}

	// Reset the call counter now that connection #1's own prelude read
	// has already landed — only calls from here on are attributable to
	// connection #2.
	atomic.StoreInt32(&reader.calls, 0)

	resp2, err := http.Get(ts.URL + "/v1/ledger/stream")
	if err != nil {
		t.Fatalf("connection 2: %v", err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("connection 2 status = %d, want 503 (cap already at 1/1)", resp2.StatusCode)
	}
	if got := atomic.LoadInt32(&reader.calls); got != 0 {
		t.Errorf("ledgerTip's ListCursors was called %d time(s) for a request the cap already rejected — "+
			"the pre-flight compute ran BEFORE admission (REL-05 ordering)", got)
	}
}

// decodeLedgerEvent parses one ledger_update SSE data payload and
// returns the embedded LedgerTipView, failing the test on a
// malformed payload.
func decodeLedgerEvent(t *testing.T, data string) v1.LedgerTipView {
	t.Helper()
	if data == "" {
		t.Fatal("empty SSE data payload")
	}
	var payload struct {
		Data v1.LedgerTipView `json:"data"`
		AsOf time.Time        `json:"as_of"`
	}
	if err := json.Unmarshal([]byte(data), &payload); err != nil {
		t.Fatalf("payload not valid JSON: %v\nraw: %s", err, data)
	}
	if payload.AsOf.IsZero() {
		t.Errorf("payload missing as_of: %s", data)
	}
	return payload.Data
}
