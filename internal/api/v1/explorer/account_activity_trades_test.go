package explorer

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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
	// gotCtxDeadline records whether the handler bounded the read (C3-1).
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

	// Deadline → 503 + the endpoint's own `…-timeout` type (C-F1).
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

// TestAccountActivity_SegmentFailureIsDisclosed — the C3-045 posture: a
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
