package clickhouse

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"
)

// ContractDeployMonth is one month of first-seen contract deployments, split by executable type.
type ContractDeployMonth struct {
	Month time.Time
	SAC   int64
	Wasm  int64
}

// ContractStats is the /contracts page header: deployments over time plus recent activity.
type ContractStats struct {
	Deployments []ContractDeployMonth
	// Active90d is nil when the census is unavailable; unknown must not read as zero.
	Active90d *int64
	// HistoryComplete is true only with a genesis-complete watermark on the instance table;
	// without it Deployments undercount and callers must render a lower bound.
	HistoryComplete bool
	ThruLedger      uint32
}

// Aliases must not shadow column names (ILLEGAL_AGGREGATION). A full-table GROUP BY, so it
// carries its own execution cap on top of the scan pin.
const contractDeploymentsQuery = `SELECT toStartOfMonth(f) AS m,
		       toInt64(countIf(s = 1)) AS n_sac, toInt64(countIf(s = 0)) AS n_wasm
		FROM (
		    SELECT contract_hash, min(close_time) AS f, argMin(is_sac, ledger_seq) AS s
		    FROM stellar.contract_instance_changes
		    GROUP BY contract_hash
		)
		GROUP BY m
		ORDER BY m` + explorerScanSettings + `, max_execution_time = 120`

const contractActive90dQuery = `SELECT toInt64(uniqExact(contract_id)) FROM stellar.contracts_census_daily WHERE day >= today() - 90`

// ContractStats reads the deployment histogram, the 90-day active count and the
// instance table's genesis watermark.
func (r *ExplorerReader) ContractStats(ctx context.Context) (ContractStats, error) {
	var out ContractStats
	rows, err := r.conn.Query(ctx, contractDeploymentsQuery)
	if err != nil {
		return out, fmt.Errorf("clickhouse: contract deployments: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var m ContractDeployMonth
		if err := rows.Scan(&m.Month, &m.SAC, &m.Wasm); err != nil {
			return out, fmt.Errorf("clickhouse: scan contract deployments: %w", err)
		}
		out.Deployments = append(out.Deployments, m)
	}
	if err := rows.Err(); err != nil {
		return out, fmt.Errorf("clickhouse: contract deployments: %w", err)
	}

	if r.censusAvailable(ctx) {
		var n int64
		if err := r.conn.QueryRow(ctx, contractActive90dQuery).Scan(&n); err != nil {
			return out, fmt.Errorf("clickhouse: contracts active 90d: %w", err)
		}
		out.Active90d = &n
	}

	// An unreadable watermark degrades to a lower bound rather than failing the stats.
	if wm, err := r.instanceGenesisWatermark(ctx); err == nil && wm > 0 {
		out.HistoryComplete, out.ThruLedger = true, wm
	}
	return out, nil
}

const contractTypesQuery = `SELECT contract_hash, argMax(is_sac, ledger_seq) AS sac
		FROM stellar.contract_instance_changes
		WHERE contract_hash IN (?)
		GROUP BY contract_hash`

const contractTypesMax = 500

// ContractTypes maps C... contract ids to whether the contract is a SAC. Ids with no
// instance row, or not valid contract strkeys, are absent from the result.
func (r *ExplorerReader) ContractTypes(ctx context.Context, contractIDs []string) (map[string]bool, error) {
	if len(contractIDs) == 0 {
		return map[string]bool{}, nil
	}
	if len(contractIDs) > contractTypesMax {
		return nil, fmt.Errorf("clickhouse: contract types: %d ids exceeds %d", len(contractIDs), contractTypesMax)
	}
	byHex := make(map[string]string, len(contractIDs))
	hexIDs := make([]string, 0, len(contractIDs))
	for _, id := range contractIDs {
		raw, err := strkey.Decode(strkey.VersionByteContract, id)
		if err != nil {
			continue
		}
		h := hex.EncodeToString(raw)
		byHex[h] = id
		hexIDs = append(hexIDs, h)
	}
	out := make(map[string]bool, len(hexIDs))
	if len(hexIDs) == 0 {
		return out, nil
	}
	rows, err := r.conn.Query(ctx, contractTypesQuery, hexIDs)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: contract types: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var h string
		var sac uint8
		if err := rows.Scan(&h, &sac); err != nil {
			return nil, fmt.Errorf("clickhouse: scan contract types: %w", err)
		}
		if id, ok := byHex[h]; ok {
			out[id] = sac == 1
		}
	}
	return out, rows.Err()
}
