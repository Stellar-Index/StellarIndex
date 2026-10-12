package clickhouse

import (
	"context"
	"fmt"
)

// classicCirculatingSupplyQuery is the trustline GROUP BY behind [ExplorerReader.ClassicCirculatingSupply]. It is SCAN-SHAPED over
// ledger_entries_current, hence explorerScanSettings (max_threads 4 bounds memory). max_execution_time is explicit because the Go client
// pins the default to 30s, below this query's runtime; 150s must stay inside classicSupplyRefreshBudget (3 min) or the server works for
// nobody.
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
