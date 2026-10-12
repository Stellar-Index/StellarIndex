package clickhouse

import (
	"context"
	"fmt"
)

// classicCirculatingSupplyQuery is the trustline GROUP BY behind
// [ExplorerReader.ClassicCirculatingSupply].
//
// It carries explorerScanSettings because it is SCAN-SHAPED over ledger_entries_current; capping
// max_threads at 4 bounds the memory footprint.
//
// max_execution_time is set explicitly: the Go client pins the connection default to 30s, BELOW
// this query's runtime. 150s sits above the observed maximum and must stay inside the caller's
// 3-minute detached-refresh budget (classicSupplyRefreshBudget), or the server would keep working
// on a query nobody is waiting for.
const classicCirculatingSupplyQuery = `SELECT asset, toString(sum(toInt128(balance))) AS circ
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'trustline' AND change_type != 'removed' AND balance > 0
		GROUP BY asset` + explorerScanSettings + `, max_execution_time = 150`

// ClassicCirculatingSupply returns per-asset sum of positive trustline balances (CODE-ISSUER to i128 string, 7 decimals); a LOWER BOUND, not the supply (blind to claimable balances, LP reserves and SAC holders), and callers MUST cache it.
func (r *ExplorerReader) ClassicCirculatingSupply(ctx context.Context) (map[string]string, error) {
	rows, err := r.conn.Query(ctx, classicCirculatingSupplyQuery)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: classic circulating supply: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]string)
	for rows.Next() {
		var asset, circ string
		if err := rows.Scan(&asset, &circ); err != nil {
			return nil, fmt.Errorf("clickhouse: scan classic supply: %w", err)
		}
		if asset != "" && circ != "" && circ != "0" {
			out[asset] = circ
		}
	}
	return out, rows.Err()
}
