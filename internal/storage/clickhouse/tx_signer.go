package clickhouse

import (
	"context"
	"fmt"
	"time"
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

// RecentTxSigners returns (ledger, tx_hash, source_account) for every
// transaction whose ledger closed at or after `since`, with a non-empty
// source account. The caller (the signer sweeper) bounds `since` to a short
// trailing window and feeds the result to timescale.TagTradesSigner, which
// filters to the AMM trades that actually need a signer — so reading all txs
// in the window is intentional (cheap, and avoids a per-source join here).
func (r *ExplorerReader) RecentTxSigners(ctx context.Context, since time.Time) ([]TxSigner, error) {
	const q = `
        SELECT ledger_seq, tx_hash, source_account
          FROM stellar.transactions FINAL
         WHERE close_time >= ?
           AND source_account != ''
    `
	rows, err := r.conn.Query(ctx, q, since.UTC())
	if err != nil {
		return nil, fmt.Errorf("clickhouse: RecentTxSigners since %s: %w", since.UTC(), err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]TxSigner, 0, 1024)
	for rows.Next() {
		var t TxSigner
		if err := rows.Scan(&t.Ledger, &t.TxHash, &t.Signer); err != nil {
			return nil, fmt.Errorf("clickhouse: RecentTxSigners scan: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: RecentTxSigners rows: %w", err)
	}
	return out, nil
}
