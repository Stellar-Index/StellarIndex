package clickhouse

import (
	"context"
	"fmt"
	"time"
)

// TxSigner is one (ledger, tx_hash) -> transaction source account, read from
// stellar.transactions for the signer back-tagger (lake events carry no source
// account; see timescale.TagTradesSigner). CloseTime lets the tagger bound its
// UPDATE by the trades time partition so it chunk-prunes.
type TxSigner struct {
	Ledger    uint32
	TxHash    string
	Signer    string
	CloseTime time.Time
}

// TxSignersForLedgerRange returns (ledger, tx_hash, source_account) for every
// transaction in the INCLUSIVE range [minLedger, maxLedger] with a non-empty
// source account. Keyed on the primary key (ledger_seq), so not a full scan;
// the caller derives the range from the AMM trades still needing a signer.
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
