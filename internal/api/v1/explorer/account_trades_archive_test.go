package explorer

import (
	"context"
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
