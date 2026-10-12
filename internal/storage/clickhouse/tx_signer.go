package clickhouse

import (
	"context"
	"fmt"
	"time"
)

// TxSigner is one (ledger, tx_hash) to transaction source account row for the signer back-tagger; CloseTime bounds its UPDATE by the trades time partition.
type TxSigner struct {
	Ledger    uint32
	TxHash    string
	Signer    string
	CloseTime time.Time
}

// TxSignersForLedgerRange returns (ledger, tx_hash, source_account) for every transaction in the INCLUSIVE range with a non-empty source account, keyed on the primary key (not a full scan).
func (r *ExplorerReader) TxSignersForLedgerRange(ctx context.Context, minLedger, maxLedger uint32) ([]TxSigner, error) {
	if maxLedger < minLedger {
		return nil, nil
	}
	const q = `
        SELECT ledger_seq, tx_hash, source_account, close_time
          FROM stellar.transactions FINAL
         WHERE ledger_seq >= ?
           AND ledger_seq <= ?
           AND source_account != ''
    `
	rows, err := r.conn.Query(ctx, q, minLedger, maxLedger)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: TxSignersForLedgerRange [%d,%d]: %w", minLedger, maxLedger, err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]TxSigner, 0, 1024)
	for rows.Next() {
		var t TxSigner
		if err := rows.Scan(&t.Ledger, &t.TxHash, &t.Signer, &t.CloseTime); err != nil {
			return nil, fmt.Errorf("clickhouse: TxSignersForLedgerRange scan: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: TxSignersForLedgerRange rows: %w", err)
	}
	return out, nil
}
