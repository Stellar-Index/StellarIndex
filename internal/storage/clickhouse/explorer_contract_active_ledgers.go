package clickhouse

import (
	"context"
	"fmt"
)

// contractActiveLedgers returns the contract's newest active ledgers (descending), at most n, optionally <= before (inclusive: the
// boundary ledger can hold events older than the cursor tuple). DISTINCT would defeat the primary-key reverse walk (61.7M rows vs
// 336k for the XLM SAC), so pages read plain rows and collapse the adjacent un-merged RMT duplicates here.
func (r *ExplorerReader) contractActiveLedgers(ctx context.Context, contractID string, before uint32, n int) ([]uint32, error) {
	out := make([]uint32, 0, n)
	for len(out) < n {
		limit := 4 * (n - len(out))
		page, err := r.contractActiveLedgersPage(ctx, contractID, before, limit)
		if err != nil {
			return nil, err
		}
		added := false
		for _, l := range page {
			if len(out) > 0 && l >= out[len(out)-1] {
				continue
			}
			out = append(out, l)
			added = true
			if len(out) == n {
				return out, nil
			}
		}
		if len(page) < limit || !added || page[len(page)-1] <= 1 {
			return out, nil
		}
		before = page[len(page)-1] - 1
	}
	return out, nil
}

func (r *ExplorerReader) contractActiveLedgersPage(ctx context.Context, contractID string, before uint32, limit int) ([]uint32, error) {
	q := `SELECT ledger_seq FROM stellar.contract_active_ledgers WHERE contract_id = ?`
	args := []any{contractID}
	if before > 0 {
		q += ` AND ledger_seq <= ?`
		args = append(args, before)
	}
	q += ` ORDER BY ledger_seq DESC LIMIT ?`
	args = append(args, limit)
	rows, err := r.conn.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: contract %s active ledgers: %w", contractID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []uint32
	for rows.Next() {
		var l uint32
		if err := rows.Scan(&l); err != nil {
			return nil, fmt.Errorf("clickhouse: scan active ledger: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
