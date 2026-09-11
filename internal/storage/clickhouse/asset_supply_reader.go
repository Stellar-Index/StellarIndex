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
// THIS IS A LOWER BOUND, NOT THE SUPPLY. A classic asset's supply sits in
// FOUR places — trustlines, claimable balances, liquidity-pool reserves, and
// the balances its Stellar Asset Contract holds for CONTRACT (C-address)
// holders in contract_data — and ledger_entries_current populates its `asset`
// column for trustlines ONLY (extract_entry_changes.go, ownerAndAsset). A
// query keyed on `asset` therefore cannot see the other three at all; it is
// blind to them by construction, not merely approximate. Measured against
// Horizon on 2026-09-11 the hidden remainder was CETES +36.605%, TESOURO
// +10.594%, USTRY +10.272%, USDY +1.274% — 99.9% of it SAC-held.
//
// Every trustline balance was minted, so this sum is a PROVABLE LOWER BOUND on
// issued supply, which is exactly what makes it useful as a floor against a
// fuller but possibly under-seeded reading. Callers should prefer, in order:
// the precise supply_1d figure (ADR-0011 Algorithm 2, all four components plus
// the operator's locked-set policy, operator-curated watch-list only); then
// the lake-flows total over the asset's SAC contract (Σmint−Σburn−Σclawback
// over stellar.supply_flows, holding-domain-agnostic and available for every
// asset — see internal/api/v1/classic_lake_supply.go); and only then this.
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
