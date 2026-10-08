package clickhouse

import (
	"context"
	"fmt"
)

// classicCirculatingSupplyQuery is the trustline GROUP BY behind
// [ExplorerReader.ClassicCirculatingSupply].
//
// It carries explorerScanSettings because it is SCAN-SHAPED over
// ledger_entries_current (as its sibling in asset_holders_rollup.go does).
// Capping max_threads at 4 is what bounds the memory footprint.
//
// max_execution_time is set explicitly, as accountsByWealthQuery does: the
// Go client pins the connection default to 30s, BELOW this query's runtime,
// and it only survives where the api_serving profile overrides it. 150s
// sits above the observed maximum and must stay inside the caller's
// 3-minute detached-refresh budget (classicSupplyRefreshBudget), or the
// server would keep working on a query nobody is waiting for.
const classicCirculatingSupplyQuery = `SELECT asset, toString(sum(toInt128(balance))) AS circ
		FROM stellar.ledger_entries_current FINAL
		WHERE entry_type = 'trustline' AND change_type != 'removed' AND balance > 0
		GROUP BY asset` + explorerScanSettings + `, max_execution_time = 150`

// ClassicCirculatingSupply returns per-asset circulating supply derived
// from the current trustline set: sum(balance) over every non-removed,
// positive trustline for each classic asset. The map key is the canonical
// CODE-ISSUER form; the value is the raw integer total at 7 decimals, as a
// string holding an i128 sum (ADR-0003 — summed as Int128, never truncated).
//
// THIS IS A LOWER BOUND, NOT THE SUPPLY. A classic asset's supply sits in
// FOUR places — trustlines, claimable balances, liquidity-pool reserves, and
// the balances its SAC holds for CONTRACT (C-address) holders — and
// ledger_entries_current populates `asset` for trustlines ONLY
// (extract_entry_changes.go, ownerAndAsset), so this query is blind to the
// other three by construction (99.9% of the hidden remainder was SAC-held).
//
// Every trustline balance was minted, so the sum is a PROVABLE LOWER BOUND
// on issued supply, useful as a floor. Callers should prefer, in order: the
// precise supply_1d figure (ADR-0011 Algorithm 2); the lake-flows total over
// the asset's SAC contract (internal/api/v1/classic_lake_supply.go); and
// only then this.
//
// Callers MUST cache the result (it changes slowly): the GROUP BY over the
// trustline slice is far too heavy for an API hot path.
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
