package explorer

import (
	"context"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// ledgerOpsReader is a capReader whose OperationsByLedger returns a
// caller-supplied fixed page — used to feed GET /v1/ledgers/{seq}/operations
// rows with controlled BodyXDR sizes.
type ledgerOpsReader struct {
	*capReader
	rows []clickhouse.OpRow
}

func (r *ledgerOpsReader) OperationsByLedger(ctx context.Context, _ uint32, _ int) ([]clickhouse.OpRow, error) {
	r.probe.record(ctx)
	return r.rows, nil
}

// garbageXDR is deliberately undecodable so opView always takes the decode-
// failure path, which echoes the row's own BodyXDR back onto RawXDR — a
// clean, deterministic signal that a row was FULLY processed. opViewLight
// never sets RawXDR at all, so RawXDR == "" cleanly signals the row was
// served undecoded.
func garbageXDR(n int) string {
	return strings.Repeat("Z", n)
}
