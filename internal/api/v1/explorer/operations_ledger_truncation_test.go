package explorer

import (
	"context"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// ledgerOpsTotalReader feeds GET /v1/ledgers/{seq}/operations a fixed page of
// rows and a controllable ledger header, to exercise the Total/Truncated
// signal independently of the response-byte-budget test's row content.
type ledgerOpsTotalReader struct {
	*capReader
	rows []clickhouse.OpRow
	hdr  clickhouse.LedgerHeader
}

func (r *ledgerOpsTotalReader) OperationsByLedger(ctx context.Context, _ uint32, _ int) ([]clickhouse.OpRow, error) {
	r.probe.record(ctx)
	return r.rows, nil
}

func (r *ledgerOpsTotalReader) LedgerBySeq(ctx context.Context, _ uint32) (clickhouse.LedgerHeader, bool, error) {
	r.probe.record(ctx)
	return r.hdr, true, nil
}
