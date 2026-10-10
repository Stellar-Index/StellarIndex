package explorer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ─── stubs ───────────────────────────────────────────────────────────

type stubTradesReader struct {
	rows  []timescale.AccountTradeRow
	err   error
	calls atomic.Int32
	// gotCtxDeadline records whether the handler bounded the read.
	gotCtxDeadline atomic.Bool
	gotLimit       atomic.Int32
}

func (s *stubTradesReader) ListAccountTrades(ctx context.Context, _ string, limit int, _ timescale.AccountTradesCursor) ([]timescale.AccountTradeRow, time.Time, error) {
	s.calls.Add(1)
	s.gotLimit.Store(int32(limit)) //nolint:gosec // test limit fits int32
	if _, ok := ctx.Deadline(); ok {
		s.gotCtxDeadline.Store(true)
	}
	return s.rows, time.Time{}, s.err
}

type stubActivityReader struct {
	trades    int64
	tradesErr error
	defi      []timescale.DefiActionCount
	defiErr   error
	bridge    timescale.BridgeActivity
	bridgeErr error
	calls     atomic.Int32
}

func (s *stubActivityReader) CountAccountTrades(context.Context, string) (int64, time.Time, error) {
	s.calls.Add(1)
	return s.trades, time.Time{}, s.tradesErr
}

func (s *stubActivityReader) DefiActionCountsByUser(context.Context, string) ([]timescale.DefiActionCount, error) {
	return s.defi, s.defiErr
}

func (s *stubActivityReader) BridgeActivityByAddress(context.Context, string) (timescale.BridgeActivity, error) {
	return s.bridge, s.bridgeErr
}

// opCountReader overrides the activity endpoint's one lake read.
type opCountReader struct {
	*capReader
	counts []clickhouse.OpTypeCount
	err    error
}

func (r *opCountReader) AccountOperationTypeCounts(context.Context, string) ([]clickhouse.OpTypeCount, error) {
	return r.counts, r.err
}

// newActivityHandler wires a JSON-capturing handler around the stubs.
func newActivityHandler(reader ExplorerReader, activity ActivityReader, trades TradesReader) (*Handler, *any) {
	h := newProbeHandler(reader, nil)
	h.Activity = activity
	h.Trades = trades
	var captured any
	h.WriteJSON = func(w http.ResponseWriter, data any, _ bool) {
		captured = data
		w.WriteHeader(http.StatusOK)
	}
	return h, &captured
}

func getAccount(h *Handler, call func(*Handler, http.ResponseWriter, *http.Request), path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.SetPathValue("g_strkey", validTestAccount)
	w := httptest.NewRecorder()
	call(h, w, req)
	return w
}

// rendezvous releases its callers only once n of them are in flight, so a
// compute that issues the reads one at a time fails instead.
type rendezvous struct {
	n       int32
	arrived atomic.Int32
	all     chan struct{}
}

func (r *rendezvous) wait(ctx context.Context) error {
	if r.arrived.Add(1) == r.n {
		close(r.all)
	}
	select {
	case <-r.all:
		return nil
	case <-time.After(2 * time.Second):
		return errors.New("segment reads ran one at a time")
	case <-ctx.Done():
		return ctx.Err()
	}
}

type rendezvousOpCountReader struct {
	*capReader
	rv *rendezvous
}

func (r *rendezvousOpCountReader) AccountOperationTypeCounts(ctx context.Context, _ string) ([]clickhouse.OpTypeCount, error) {
	if err := r.rv.wait(ctx); err != nil {
		return nil, err
	}
	return []clickhouse.OpTypeCount{{OpType: "payment", Count: 1}}, nil
}

type rendezvousActivityReader struct{ rv *rendezvous }

func (s *rendezvousActivityReader) CountAccountTrades(ctx context.Context, _ string) (int64, time.Time, error) {
	return 3, time.Time{}, s.rv.wait(ctx)
}

func (s *rendezvousActivityReader) DefiActionCountsByUser(ctx context.Context, _ string) ([]timescale.DefiActionCount, error) {
	return nil, s.rv.wait(ctx)
}

func (s *rendezvousActivityReader) BridgeActivityByAddress(ctx context.Context, _ string) (timescale.BridgeActivity, error) {
	return timescale.BridgeActivity{}, s.rv.wait(ctx)
}

// ─── /trades ─────────────────────────────────────────────────────────

func TestAccountTrades_HappyPathAndCursor(t *testing.T) {
	ts := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	rows := make([]timescale.AccountTradeRow, 0, 2)
	for i, src := range []string{"sdex", "aquarius"} {
		rows = append(rows, timescale.AccountTradeRow{
			Source: src, Ledger: uint32(100 - i), TxHash: strings.Repeat("a", 64), OpIndex: uint32(i),
			Ts: ts, BaseAsset: "native", QuoteAsset: "USDC-GISSUER",
			BaseAmount: "10000000", QuoteAmount: "1234567", USDVolume: "0.12345600",
			Role: "taker",
		})
	}
	stub := &stubTradesReader{rows: rows}
	h, captured := newActivityHandler(&capReader{probe: &deadlineProbe{}}, nil, stub)
	// ParseLimit stub returns the default; make the page size equal the
	// row count so a next_cursor must be emitted.
	h.ParseLimit = func(_ http.ResponseWriter, _ *http.Request, _, _ int) (int, bool) { return 2, true }

	w := getAccount(h, (*Handler).AccountTrades, "/v1/accounts/"+validTestAccount+"/trades")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	view, ok := (*captured).(AccountTradesView)
	if !ok {
		t.Fatalf("captured payload is %T, want AccountTradesView", *captured)
	}
	if view.Account != validTestAccount || len(view.Trades) != 2 {
		t.Fatalf("view = %+v", view)
	}
	if view.Trades[0].BaseAmount != "10000000" || view.Trades[0].USDVolume != "0.12345600" {
		t.Errorf("amounts must pass through as decimal strings: %+v", view.Trades[0])
	}
	if view.Note == "" {
		t.Error("the scope note must always be present")
	}
	if !stub.gotCtxDeadline.Load() {
		t.Error("ListAccountTrades must be called with a bounded context (C3-1)")
	}

	// Full page → next_cursor points at the last served row and
	// round-trips through the parser.
	if view.NextCursor == "" {
		t.Fatal("full page must emit next_cursor")
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/accounts/x/trades?cursor="+view.NextCursor, nil)
	w2 := httptest.NewRecorder()
	cur, ok := h.parseAccountTradesCursor(w2, req)
	if !ok {
		t.Fatalf("next_cursor %q did not round-trip", view.NextCursor)
	}
	last := rows[len(rows)-1]
	if !cur.Ts.Equal(last.Ts) || cur.Ledger != last.Ledger || cur.TxHash != last.TxHash || cur.OpIndex != last.OpIndex {
		t.Errorf("cursor = %+v, want the last served row's keyset position %+v", cur, last)
	}
}

func TestAccountTrades_InvalidCursor400(t *testing.T) {
	var rec problemRecord
	h, _ := newActivityHandler(&capReader{probe: &deadlineProbe{}}, nil, &stubTradesReader{})
	h.WriteProblem = func(w http.ResponseWriter, _ *http.Request, typeURL, title string, status int, detail string) {
		rec = problemRecord{typeURL: typeURL, title: title, status: status, detail: detail, written: true}
		w.WriteHeader(status)
	}
	for _, bad := range []string{"garbage", "1.2.3", "x.2.hash.3", "-5.2.hash.3", "1.2..3", "1.2.hash.x"} {
		rec = problemRecord{}
		req := httptest.NewRequest(http.MethodGet, "/v1/accounts/"+validTestAccount+"/trades?cursor="+bad, nil)
		req.SetPathValue("g_strkey", validTestAccount)
		w := httptest.NewRecorder()
		h.AccountTrades(w, req)
		if !rec.written || rec.status != http.StatusBadRequest {
			t.Errorf("cursor %q: status = %d (written=%v), want 400", bad, rec.status, rec.written)
		}
	}
}

func TestAccountTrades_NilReader503_And_Timeout503(t *testing.T) {
	var rec problemRecord
	h, _ := newActivityHandler(&capReader{probe: &deadlineProbe{}}, nil, nil)
	h.WriteProblem = func(w http.ResponseWriter, _ *http.Request, typeURL, title string, status int, detail string) {
		rec = problemRecord{typeURL: typeURL, title: title, status: status, detail: detail, written: true}
		w.WriteHeader(status)
	}
	w := getAccount(h, (*Handler).AccountTrades, "/v1/accounts/"+validTestAccount+"/trades")
	if w.Code != http.StatusServiceUnavailable || rec.status != http.StatusServiceUnavailable {
		t.Fatalf("nil reader: status = %d, want 503", w.Code)
	}

	// Deadline → 503 + the endpoint's own `…-timeout` type.
	rec = problemRecord{}
	h, _ = newActivityHandler(&capReader{probe: &deadlineProbe{}}, nil, &stubTradesReader{err: context.DeadlineExceeded})
	h.WriteProblem = func(w http.ResponseWriter, _ *http.Request, typeURL, title string, status int, detail string) {
		rec = problemRecord{typeURL: typeURL, title: title, status: status, detail: detail, written: true}
		w.WriteHeader(status)
	}
	w = getAccount(h, (*Handler).AccountTrades, "/v1/accounts/"+validTestAccount+"/trades")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("timeout: status = %d, want 503", w.Code)
	}
	if rec.typeURL != "https://api.stellarindex.io/errors/account-trades-timeout" {
		t.Errorf("timeout problem type = %q", rec.typeURL)
	}
	if !strings.Contains(rec.detail, explorerReadTimeout.String()) {
		t.Errorf("timeout detail must name the read budget: %q", rec.detail)
	}
}

// ─── /activity ───────────────────────────────────────────────────────

func TestAccountActivity_ComposesSegments(t *testing.T) {
	reader := &opCountReader{
		capReader: &capReader{probe: &deadlineProbe{}},
		counts:    []clickhouse.OpTypeCount{{OpType: "payment", Count: 7}, {OpType: "manage_buy_offer", Count: 3}},
	}
	activity := &stubActivityReader{
		trades: 42,
		defi: []timescale.DefiActionCount{
			{Protocol: "blend", Action: "supply", Count: 5},
			{Protocol: "sorocredit", Action: "position_opened", Count: 1},
		},
		bridge: timescale.BridgeActivity{RozoOutbound: 2, CCTPInboundMints: 1},
	}
	h, captured := newActivityHandler(reader, activity, nil)

	w := getAccount(h, (*Handler).AccountActivity, "/v1/accounts/"+validTestAccount+"/activity")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	view, ok := (*captured).(AccountActivityView)
	if !ok {
		t.Fatalf("captured payload is %T, want AccountActivityView", *captured)
	}
	if view.CoverageNote != "" {
		t.Errorf("fully-successful read must not carry a coverage note: %q", view.CoverageNote)
	}
	if len(view.OpsByType) != 2 || view.OpsByType[0].OpType != "payment" {
		t.Errorf("ops_by_type = %+v", view.OpsByType)
	}
	if view.TradesTotal == nil || *view.TradesTotal != 42 {
		t.Errorf("trades_total = %v, want 42", view.TradesTotal)
	}
	if len(view.DefiActions) != 2 {
		t.Errorf("defi_actions = %+v", view.DefiActions)
	}
	if view.BridgeTransfers == nil || view.BridgeTransfers.RozoOutboundPayments != 2 ||
		view.BridgeTransfers.CCTPInboundMints != 1 || view.BridgeTransfers.Note == "" {
		t.Errorf("bridge_transfers = %+v", view.BridgeTransfers)
	}

	// Second request must serve the cached payload — the whole-history
	// aggregates never run per-request (the endpoint's entire point).
	before := activity.calls.Load()
	if w2 := getAccount(h, (*Handler).AccountActivity, "/v1/accounts/"+validTestAccount+"/activity"); w2.Code != http.StatusOK {
		t.Fatalf("second request: status = %d, want 200", w2.Code)
	}
	if activity.calls.Load() != before {
		t.Error("second request within the TTL recomputed instead of serving the cache")
	}
}

// TestAccountActivity_SegmentsReadConcurrently — the four segment reads hit
// independent stores; run serially, a cold compute cost their sum.
func TestAccountActivity_SegmentsReadConcurrently(t *testing.T) {
	rv := &rendezvous{n: 4, all: make(chan struct{})}
	reader := &rendezvousOpCountReader{capReader: &capReader{probe: &deadlineProbe{}}, rv: rv}
	h, captured := newActivityHandler(reader, &rendezvousActivityReader{rv: rv}, nil)

	w := getAccount(h, (*Handler).AccountActivity, "/v1/accounts/"+validTestAccount+"/activity")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	view := (*captured).(AccountActivityView)
	if view.CoverageNote != "" {
		t.Fatalf("every segment must have been read concurrently: %q", view.CoverageNote)
	}
	if view.TradesTotal == nil || *view.TradesTotal != 3 || len(view.OpsByType) != 1 || view.BridgeTransfers == nil {
		t.Errorf("segments not composed: %+v", view)
	}
}

// TestAccountActivity_SegmentFailureIsDisclosed — the disclosure posture: a
// failed segment must be ABSENT and NAMED, never silently zero.
func TestAccountActivity_SegmentFailureIsDisclosed(t *testing.T) {
	reader := &opCountReader{
		capReader: &capReader{probe: &deadlineProbe{}},
		counts:    []clickhouse.OpTypeCount{{OpType: "payment", Count: 7}},
	}
	activity := &stubActivityReader{
		tradesErr: errors.New("pg down"),
		defi:      []timescale.DefiActionCount{{Protocol: "blend", Action: "supply", Count: 5}},
		bridge:    timescale.BridgeActivity{},
	}
	h, captured := newActivityHandler(reader, activity, nil)

	w := getAccount(h, (*Handler).AccountActivity, "/v1/accounts/"+validTestAccount+"/activity")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (segment degrade, not failure)", w.Code)
	}
	view := (*captured).(AccountActivityView)
	if view.TradesTotal != nil {
		t.Errorf("failed trades_total must be absent, got %v", *view.TradesTotal)
	}
	if !strings.Contains(view.CoverageNote, "trades_total") {
		t.Errorf("coverage note must name the missing segment: %q", view.CoverageNote)
	}
	// The JSON wire shape must genuinely omit the failed segment.
	b, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"trades_total":`) {
		t.Errorf("wire shape must omit the failed segment entirely: %s", b)
	}
}

func TestAccountActivity_AllSegmentsFail(t *testing.T) {
	var rec problemRecord
	boom := errors.New("everything down")
	reader := &opCountReader{capReader: &capReader{probe: &deadlineProbe{}}, err: boom}
	activity := &stubActivityReader{tradesErr: boom, defiErr: boom, bridgeErr: boom}
	h, _ := newActivityHandler(reader, activity, nil)
	h.WriteProblem = func(w http.ResponseWriter, _ *http.Request, typeURL, title string, status int, detail string) {
		rec = problemRecord{typeURL: typeURL, title: title, status: status, detail: detail, written: true}
		w.WriteHeader(status)
	}
	w := getAccount(h, (*Handler).AccountActivity, "/v1/accounts/"+validTestAccount+"/activity")
	if w.Code != http.StatusInternalServerError || !rec.written {
		t.Fatalf("all-fail cold read: status = %d (written=%v), want 500", w.Code, rec.written)
	}
}

func TestAccountActivity_NilSeams(t *testing.T) {
	// Both seams nil → 503.
	h := newProbeHandler(nil, nil)
	w := getAccount(h, (*Handler).AccountActivity, "/v1/accounts/"+validTestAccount+"/activity")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("both seams nil: status = %d, want 503", w.Code)
	}

	// Lake reader nil but Postgres wired → the PG segments serve, the
	// ops segment is disclosed as missing.
	activity := &stubActivityReader{trades: 1}
	h2, captured := newActivityHandler(nil, activity, nil)
	h2.Reader = nil
	w2 := getAccount(h2, (*Handler).AccountActivity, "/v1/accounts/"+validTestAccount+"/activity")
	if w2.Code != http.StatusOK {
		t.Fatalf("PG-only: status = %d, want 200", w2.Code)
	}
	view := (*captured).(AccountActivityView)
	if view.OpsByType != nil {
		t.Errorf("ops_by_type must be absent with no lake reader, got %+v", view.OpsByType)
	}
	if !strings.Contains(view.CoverageNote, "ops_by_type") {
		t.Errorf("coverage note must name ops_by_type: %q", view.CoverageNote)
	}
}

// TestAccountTrades_GateBoundsConcurrentScans pins that the handler must
// acquire accountTradesGate before the expensive per-account scan, so that no
// more than cap(accountTradesGate) scans ever run at once.
//
// It fires cap+1 concurrent requests against a reader that blocks until
// released. With the gate acquired, exactly `cap` reach the reader and the
// extra parks in the gate select; peak in-flight is cap. Against the un-fixed
// handler (gate declared but never acquired) all cap+1 reach the reader at
// once and peak in-flight is cap+1 — this test fails RED.
func TestAccountTrades_GateBoundsConcurrentScans(t *testing.T) {
	gateCap := cap(accountTradesGate)
	if gateCap == 0 {
		t.Fatalf("accountTradesGate is unbuffered")
	}
	total := gateCap + 1

	reader := &blockingTradesReader{
		entered: make(chan struct{}, total),
		release: make(chan struct{}),
	}
	h := newProbeHandler(nil, nil)
	h.Trades = reader

	var wg sync.WaitGroup
	codes := make([]int, total)
	for i := 0; i < total; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Distinct callers: the per-caller cap must not mask the global one.
			w := getAccountTradesFrom(h, fmt.Sprintf("198.51.100.%d:1234", i+1))
			codes[i] = w.Code
		}(i)
	}

	// Wait until the gate is saturated: gateCap scans have entered the reader.
	for i := 0; i < gateCap; i++ {
		select {
		case <-reader.entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d scans entered the reader — deadlock?", i, gateCap)
		}
	}

	// Give the extra request a beat to reach — and, when the gate is
	// acquired, PARK IN — the gate select rather than the reader. This sleep
	// stays well inside accountTradesGateWait (%v) so the extra does not shed;
	// it is served once a slot frees below.
	time.Sleep(150 * time.Millisecond)
	if got := reader.maxFlight.Load(); got > int32(gateCap) {
		t.Errorf("peak concurrent trades scans = %d, want <= gate cap %d — the gate is not acquired (unbounded scan)",
			got, gateCap)
	}

	// Release the in-flight scans; the parked request now takes a freed slot.
	close(reader.release)
	wg.Wait()

	if got := reader.maxFlight.Load(); got != int32(gateCap) {
		t.Errorf("peak concurrent trades scans = %d, want exactly gate cap %d", got, gateCap)
	}
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request %d served %d, want 200 (it should get a slot within the gate wait, not shed)", i, c)
		}
	}
}

// TestAccountTrades_OneScanPerCaller pins that one caller cannot hold more
// than one gate slot: its second concurrent scan sheds at once without
// taking a slot, while another caller is still admitted.
func TestAccountTrades_OneScanPerCaller(t *testing.T) {
	reader := &blockingTradesReader{
		entered: make(chan struct{}, 4),
		release: make(chan struct{}),
	}
	h := newProbeHandler(nil, nil)
	h.Trades = reader

	var wg sync.WaitGroup
	codes := make(map[string]int)
	var mu sync.Mutex
	serve := func(name, addr string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := getAccountTradesFrom(h, addr)
			mu.Lock()
			codes[name] = w.Code
			mu.Unlock()
		}()
	}
	awaitEntry := func(name string) {
		select {
		case <-reader.entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("%s never reached the reader", name)
		}
	}

	serve("a1", "198.51.100.1:1111")
	awaitEntry("a1")

	// A's second scan must shed at once; run it aside so a handler that
	// admits it (and parks in the reader) fails rather than hangs.
	second := make(chan int, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		second <- getAccountTradesFrom(h, "198.51.100.1:2222").Code
	}()
	select {
	case code := <-second:
		if code != http.StatusServiceUnavailable {
			t.Errorf("caller A's second concurrent scan served %d, want 503 shed", code)
		}
	case <-time.After(accountTradesGateWait):
		t.Errorf("caller A's second concurrent scan was admitted, want an immediate 503 shed")
	}
	if got := len(accountTradesGate); got != 1 {
		t.Errorf("gate slots held = %d after the per-caller shed, want 1 (only A's first scan)", got)
	}

	serve("b1", "203.0.113.7:1111")
	awaitEntry("b1")

	close(reader.release)
	wg.Wait()
	for _, name := range []string{"a1", "b1"} {
		if codes[name] != http.StatusOK {
			t.Errorf("%s served %d, want 200", name, codes[name])
		}
	}
	if w := getAccountTradesFrom(h, "198.51.100.1:3333"); w.Code != http.StatusOK {
		t.Errorf("caller A after its scan completed served %d, want 200 (slot not released)", w.Code)
	}
}
