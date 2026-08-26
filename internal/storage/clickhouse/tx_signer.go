package clickhouse

import (
	"context"
	"fmt"
)

// TxSigner is one (ledger, tx_hash) → transaction source account, read from
// the lake's stellar.transactions for the signer back-tagger. The projector
// replays trades from lake EVENTS, which carry no source account, so this is
// the authoritative place the tx signer lives — see migration 0150 +
// timescale.TagTradesSigner.
type TxSigner struct {
	Ledger uint32
	TxHash string
	Signer string
}

// TxSignersForLedgerRange returns (ledger, tx_hash, source_account) for every
// transaction in the INCLUSIVE ledger range [minLedger, maxLedger] with a
// non-empty source account. The range is keyed on the stellar.transactions
// primary key (ledger_seq), so the read is bounded + index-efficient — NOT a
// full-table scan. The caller (the signer sweeper) derives the range from the
// small set of AMM trades that still need a signer, so at steady state the
// span is only a few minutes of recent ledgers; the timescale-side
// TagTradesSigner then filters to the AMM rows that actually need tagging.
func (r *ExplorerReader) TxSignersForLedgerRange(ctx context.Context, minLedger, maxLedger uint32) ([]TxSigner, error) {
	if maxLedger < minLedger {
		return nil, nil
	}
	const q = `
        SELECT ledger_seq, tx_hash, source_account
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
		if err := rows.Scan(&t.Ledger, &t.TxHash, &t.Signer); err != nil {
			return nil, fmt.Errorf("clickhouse: TxSignersForLedgerRange scan: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: TxSignersForLedgerRange rows: %w", err)
	}
	return out, nil
}
