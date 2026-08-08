package v1_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Contract tests for GET /v1/accounts/{g}/trades and
// GET /v1/accounts/{g}/activity through the real Server mux + envelope
// (validation and wire shape; the handler-internal behaviors — timeout
// mapping, SWR cache, segment degrade — are pinned in
// internal/api/v1/explorer/account_activity_trades_test.go).

// stubAccountTradesReader is a canned explorerpkg.TradesReader
// (structural, same convention as stubPositionsReader).
type stubAccountTradesReader struct {
	rows []timescale.AccountTradeRow
}

func (s *stubAccountTradesReader) ListAccountTrades(_ context.Context, _ string, limit int, _ timescale.AccountTradesCursor) ([]timescale.AccountTradeRow, time.Time, error) {
	rows := s.rows
	if limit > 0 && len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, time.Time{}, nil
}

// stubAccountActivityReader is a canned explorerpkg.ActivityReader.
type stubAccountActivityReader struct {
	trades int64
	defi   []timescale.DefiActionCount
	bridge timescale.BridgeActivity
}

func (s *stubAccountActivityReader) CountAccountTrades(context.Context, string) (int64, time.Time, error) {
	return s.trades, time.Time{}, nil
}

func (s *stubAccountActivityReader) DefiActionCountsByUser(context.Context, string) ([]timescale.DefiActionCount, error) {
	return s.defi, nil
}

func (s *stubAccountActivityReader) BridgeActivityByAddress(context.Context, string) (timescale.BridgeActivity, error) {
	return s.bridge, nil
}

func TestExplorer_AccountTrades_WireShape(t *testing.T) {
	ts := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	reader := &stubAccountTradesReader{rows: []timescale.AccountTradeRow{
		{
			Source: "sdex", Ledger: 63_000_001,
			TxHash:  "be8ac09cf011950987ae7c17badec336ccf24782a03f5573b1f982cb44c98f36",
			OpIndex: 0, Ts: ts,
			BaseAsset: "native", QuoteAsset: "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
			BaseAmount: "10000000", QuoteAmount: "1234567", USDVolume: "0.12345600",
			Role: "taker", Counterparty: otherG,
		},
		{
			Source: "aquarius", Ledger: 62_999_990,
			TxHash:  "aa8ac09cf011950987ae7c17badec336ccf24782a03f5573b1f982cb44c98f36",
			OpIndex: 2, Ts: ts.Add(-time.Hour),
			BaseAsset: "native", QuoteAsset: "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC",
			BaseAmount: "5000000", QuoteAmount: "400000",
			Role: "taker",
		},
	}}

	srv := v1.New(v1.Options{AccountTrades: reader})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/accounts/"+testG+"/trades")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.AccountTradesView `json:"data"`
	}
	mustDecode(t, resp, &body)

	if body.Data.Account != testG {
		t.Errorf("account = %q, want %q", body.Data.Account, testG)
	}
	if body.Data.Note == "" {
		t.Error("the taker/maker scope note must always be present")
	}
	if len(body.Data.Trades) != 2 {
		t.Fatalf("trades = %d, want 2", len(body.Data.Trades))
	}
	first := body.Data.Trades[0]
	if first.Source != "sdex" || first.Ledger != 63_000_001 || first.Role != "taker" {
		t.Errorf("first trade = %+v", first)
	}
	// ADR-0003: amounts stay decimal strings end-to-end.
	if first.BaseAmount != "10000000" || first.QuoteAmount != "1234567" || first.USDVolume != "0.12345600" {
		t.Errorf("amounts must be decimal strings: %+v", first)
	}
	if first.Ts != "2026-07-29T12:00:00Z" {
		t.Errorf("ts = %q, want RFC3339 UTC", first.Ts)
	}
	// Second row's usd_volume was NULL (unknown) — must be ABSENT, not "0".
	if body.Data.Trades[1].USDVolume != "" {
		t.Errorf("unknown usd_volume must be empty/absent, got %q", body.Data.Trades[1].USDVolume)
	}
	// Short page (2 < default 50) → no cursor.
	if body.Data.NextCursor != "" {
		t.Errorf("short page must not emit next_cursor, got %q", body.Data.NextCursor)
	}
}

func TestExplorer_AccountTrades_Validation(t *testing.T) {
	srv := v1.New(v1.Options{AccountTrades: &stubAccountTradesReader{}})
	base := httpTestServer(t, srv).URL

	for path, want := range map[string]int{
		"/v1/accounts/not-a-strkey/trades":                 http.StatusBadRequest,
		"/v1/accounts/" + testG + "/trades?limit=0":        http.StatusBadRequest,
		"/v1/accounts/" + testG + "/trades?limit=201":      http.StatusBadRequest,
		"/v1/accounts/" + testG + "/trades?cursor=garbage": http.StatusBadRequest,
		"/v1/accounts/" + testG + "/trades?cursor=1.2.3":   http.StatusBadRequest,
		"/v1/accounts/" + testG + "/trades?cursor=0.1.h.2": http.StatusBadRequest,
	} {
		resp := mustGet(t, base+path)
		if resp.StatusCode != want {
			t.Errorf("%s: status = %d, want %d", path, resp.StatusCode, want)
		}
		_ = resp.Body.Close()
	}
}

func TestExplorer_AccountTrades_Unwired503(t *testing.T) {
	srv := v1.New(v1.Options{})
	base := httpTestServer(t, srv).URL
	resp := mustGet(t, base+"/v1/accounts/"+testG+"/trades")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestExplorer_AccountActivity_WireShape(t *testing.T) {
	chReader := &stubExplorerReader{opTypeStats: []clickhouse.OpTypeCount{
		{OpType: "payment", Count: 120},
		{OpType: "path_payment_strict_send", Count: 4},
	}}
	activity := &stubAccountActivityReader{
		trades: 77,
		defi: []timescale.DefiActionCount{
			{Protocol: "blend", Action: "supply", Count: 3},
			{Protocol: "aquarius", Action: "position_update", Count: 9},
		},
		bridge: timescale.BridgeActivity{RozoOutbound: 1, CCTPOutboundBurns: 2},
	}
	srv := v1.New(v1.Options{Explorer: chReader, AccountActivity: activity})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/accounts/"+testG+"/activity")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data v1.AccountActivityView `json:"data"`
	}
	mustDecode(t, resp, &body)

	d := body.Data
	if d.Account != testG {
		t.Errorf("account = %q, want %q", d.Account, testG)
	}
	if d.CoverageNote != "" {
		t.Errorf("fully-successful read must not carry a coverage note: %q", d.CoverageNote)
	}
	if len(d.OpsByType) != 2 || d.OpsByType[0].OpType != "payment" || d.OpsByType[0].Count != 120 {
		t.Errorf("ops_by_type = %+v", d.OpsByType)
	}
	if d.TradesTotal == nil || *d.TradesTotal != 77 {
		t.Errorf("trades_total = %v, want 77", d.TradesTotal)
	}
	if len(d.DefiActions) != 2 || d.DefiActions[0].Protocol != "blend" {
		t.Errorf("defi_actions = %+v", d.DefiActions)
	}
	if d.BridgeTransfers == nil || d.BridgeTransfers.RozoOutboundPayments != 1 ||
		d.BridgeTransfers.CCTPOutboundBurns != 2 || d.BridgeTransfers.Note == "" {
		t.Errorf("bridge_transfers = %+v", d.BridgeTransfers)
	}
}

func TestExplorer_AccountActivity_Validation(t *testing.T) {
	srv := v1.New(v1.Options{AccountActivity: &stubAccountActivityReader{}})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/accounts/not-a-strkey/activity")
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("invalid strkey: status = %d, want 400", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// Neither seam wired → 503.
	srv2 := v1.New(v1.Options{})
	base2 := httpTestServer(t, srv2).URL
	resp2 := mustGet(t, base2+"/v1/accounts/"+testG+"/activity")
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("unwired: status = %d, want 503", resp2.StatusCode)
	}
	_ = resp2.Body.Close()
}
