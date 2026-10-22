package clickhouse

import (
	"context"
	"fmt"
	"time"
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
// A fresh holders-rollup cycle answers it; otherwise it scans.
func (r *ExplorerReader) ClassicCirculatingSupply(ctx context.Context) (map[string]string, error) {
	if m, ok := r.classicCirculatingFromRollup(ctx); ok {
		return m, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: classic circulating supply: %w", err)
	}
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

// classicCirculatingRollupQuery reads the sums the holders rollup computes in its own scan of the same trustline population.
const classicCirculatingRollupQuery = `SELECT asset, toString(circulating), holders, computed_at
		FROM stellar.asset_holders_counts WHERE asset != 'native'`

// classicCirculatingFromRollup is ClassicCirculatingSupply from the holders rollup; ok=false sends the caller to the scan when the table
// or column is absent, the read fails, the cycle is older than holdersRollupMaxAge, or a held asset sums to 0 (a cycle written before
// the column existed).
func (r *ExplorerReader) classicCirculatingFromRollup(ctx context.Context) (map[string]string, bool) {
	rows, err := r.conn.Query(ctx, classicCirculatingRollupQuery)
	if err != nil {
		return nil, false
	}
	defer func() { _ = rows.Close() }()
	out := make(map[string]string)
	var at time.Time
	for rows.Next() {
		var asset, circ string
		var holders int64
		var rowAt time.Time
		if err := rows.Scan(&asset, &circ, &holders, &rowAt); err != nil {
			return nil, false
		}
		if (holders > 0 && circ == "0") || (!at.IsZero() && !rowAt.Equal(at)) {
			return nil, false
		}
		at = rowAt
		if asset != "" && circ != "" && circ != "0" {
			out[asset] = circ
		}
	}
	if rows.Err() != nil || len(out) == 0 || !holdersRollupStamped(at) || time.Since(at) > holdersRollupMaxAge {
		return nil, false
	}
	return out, true
}
