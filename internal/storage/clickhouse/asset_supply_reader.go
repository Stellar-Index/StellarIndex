package clickhouse

import (
	"context"
	"fmt"
)

// classicCirculatingSupplyQuery is the trustline GROUP BY behind
// [ExplorerReader.ClassicCirculatingSupply].
//
// It carries explorerScanSettings because it is SCAN-SHAPED over
// ledger_entries_current, and it was the only such read in this package
// that did not. Its byte-identical sibling in asset_holders_rollup.go has
// always had the pin. Measured on r1 (system.query_log, 14 days): ~130
// runs/day, 127.8M rows, avg 24-27s, max 75.5s, peak 1.85 GiB — 86% of
// the api_serving profile's 2 GiB ceiling, and 2.564 GiB on the one run
// that went through the unbounded default user. Capping max_threads at 4
// is what collapses that footprint; the memory number is a backstop, not
// a target.
//
// max_execution_time is set explicitly for the same reason
// accountsByWealthQuery sets it: the Go client pins the connection
// default to 30s, which is BELOW this query's observed runtime, so it
// only survives because r1's api_serving profile happens to override the
// value to 184. A deployment without that override would kill this query
// every time, silently, and the classic-supply fallback would simply
// stop having answers. 150s sits above the 75.5s observed maximum and
// inside the caller's own 3-minute detached-refresh budget
// (classicSupplyRefreshBudget) — it must never exceed that, or the
// server would keep working on a query nobody is waiting for.
const classicCirculatingSupplyQuery = `SELECT asset, toString(sum(toInt128(balance))) AS circ
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'trustline' AND change_type != 'removed' AND balance > 0
		GROUP BY asset` + explorerScanSettings + `, max_execution_time = 150`

// ClassicCirculatingSupply returns per-asset circulating supply derived
// from the current trustline set: sum(balance) over every non-removed,
// positive trustline for each classic asset. The map key is the canonical
// CODE-ISSUER form (matching canonical asset_id); the value is the raw
// integer total at 7 decimals (classic assets are always 7dp), as a
// string holding an i128 sum (ADR-0003 — the running total can exceed
// int64, so it is summed as Int128 and stringified, never truncated).
//
// For a classic asset, circulating supply IS the amount held by every
// non-issuer account, which is exactly the trustline-balance sum — the
// issuer holds no trustline to its own asset (its issuance shows as the
// negative side, not a held balance). This slightly undercounts vs the
// precise three-domain supply pipeline (it omits claimable-balance and
// liquidity-pool-locked holdings), so callers that have a precise
// supply_1d figure for an asset should prefer that and use this only as
// the broad-coverage fallback for the long tail.
//
// One GROUP BY over the ~2M-row trustline slice (~0.5s on r1). Callers
// MUST cache the result (it changes slowly) rather than run it per
// request — it is far too heavy for an API hot path uncached.
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
