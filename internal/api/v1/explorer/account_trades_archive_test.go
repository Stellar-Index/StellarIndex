package explorer

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type fixedTradesReader struct {
	rows    []timescale.AccountTradeRow
	horizon time.Time
}

func (r fixedTradesReader) ListAccountTrades(context.Context, string, int, timescale.AccountTradesCursor) ([]timescale.AccountTradeRow, time.Time, error) {
	return r.rows, r.horizon, nil
}

type fakeTradesArchive struct {
	runs               []clickhouse.TradesByAccountRun
	covErr, listErr    error
	rows               []clickhouse.TradeByAccountRow
	gotFloor, gotBelow time.Time
	calls              int
}

func (f *fakeTradesArchive) TradesByAccountCoverage(context.Context) ([]clickhouse.TradesByAccountRun, error) {
	return f.runs, f.covErr
}

func (f *fakeTradesArchive) ListArchivedAccountTrades(_ context.Context, _ string, _ int, floor, below time.Time, _ clickhouse.TradesByAccountCursor) ([]clickhouse.TradeByAccountRow, error) {
	f.calls++
	f.gotFloor, f.gotBelow = floor, below
	return f.rows, f.listErr
}

type accountTradesBody struct {
	Trades     []AccountTradeEntry `json:"trades"`
	Note       string              `json:"note"`
	LowerBound bool                `json:"lower_bound"`
}

func TestAccountTrades_ArchiveFillAndLowerBound(t *testing.T) {
	horizon := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	pgRow := timescale.AccountTradeRow{Source: "sdex", Ledger: 900, TxHash: "pg", Ts: horizon.Add(time.Hour), BaseAmount: "1", QuoteAmount: "2"}
	chRow := clickhouse.TradeByAccountRow{
		Source: "sdex", Ledger: 500, TxHash: "ch", Ts: horizon.Add(-time.Hour),
		BaseAmount: big.NewInt(3), QuoteAmount: big.NewInt(4),
	}
	run := func(from uint32) []clickhouse.TradesByAccountRun {
		return []clickhouse.TradesByAccountRun{{FromLedger: from, ToLedger: 950, FromTs: time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), ToTs: horizon.Add(time.Hour)}}
	}
	for _, tc := range []struct {
		name      string
		horizon   time.Time
		archive   *fakeTradesArchive
		wantRows  int
		wantLower bool
		wantSince string
		wantDeg   bool
	}{
		{"nothing compressed", time.Unix(0, 0).UTC(), nil, 1, false, "", false},
		{"no archive wired", horizon, nil, 1, true, "2026-09-01", false},
		{"archive from genesis", horizon, &fakeTradesArchive{runs: run(2), rows: []clickhouse.TradeByAccountRow{chRow}}, 2, false, "", false},
		{"archive partial", horizon, &fakeTradesArchive{runs: run(1000), rows: []clickhouse.TradeByAccountRow{chRow}}, 2, true, "2020-01-01", false},
		{"coverage stops short of horizon", horizon, &fakeTradesArchive{runs: []clickhouse.TradesByAccountRun{{FromLedger: 2, ToLedger: 10, ToTs: horizon.Add(-48 * time.Hour)}}}, 1, true, "2026-09-01", false},
		{"coverage error", horizon, &fakeTradesArchive{covErr: errors.New("down")}, 1, true, "2026-09-01", true},
		{"archive read error", horizon, &fakeTradesArchive{runs: run(2), listErr: errors.New("down")}, 1, true, "2026-09-01", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newProbeHandler(nil, nil)
			h.Trades = fixedTradesReader{rows: []timescale.AccountTradeRow{pgRow}, horizon: tc.horizon}
			if tc.archive != nil {
				h.TradesArchive = tc.archive
			}
			req := httptest.NewRequest(http.MethodGet, "/v1/accounts/"+validTestAccount+"/trades", nil)
			req.SetPathValue("g_strkey", validTestAccount)
			var sent any
			var degraded bool
			h.WriteJSON = func(_ http.ResponseWriter, data any, _ bool) { sent = data }
			h.WriteJSONAt = func(_ http.ResponseWriter, data any, _, deg bool, _ time.Time) { sent, degraded = data, deg }
			h.AccountTrades(httptest.NewRecorder(), req)
			raw, err := json.Marshal(sent)
			if err != nil {
				t.Fatal(err)
			}
			if degraded != tc.wantDeg {
				t.Errorf("degraded=%v, want %v", degraded, tc.wantDeg)
			}
			var body accountTradesBody
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Fatal(err)
			}
			if len(body.Trades) != tc.wantRows || body.LowerBound != tc.wantLower {
				t.Fatalf("rows=%d lower_bound=%v, want %d %v", len(body.Trades), body.LowerBound, tc.wantRows, tc.wantLower)
			}
			if tc.wantSince != "" && !strings.Contains(body.Note, "since "+tc.wantSince) {
				t.Errorf("note %q does not name floor %s", body.Note, tc.wantSince)
			}
			if tc.wantSince == "" && strings.Contains(body.Note, "since ") {
				t.Errorf("complete answer carries a floor note: %q", body.Note)
			}
			if a := tc.archive; a != nil && a.calls > 0 && !a.gotBelow.Equal(horizon) {
				t.Errorf("archive read below %v, want the horizon %v (no overlap with Postgres)", a.gotBelow, horizon)
			}
		})
	}
}
