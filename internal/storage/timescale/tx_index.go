package timescale

import (
	"context"
	"fmt"
	"time"
)

// TxIndexKey is one on-chain transaction in `trades` whose tx_index is still NULL, with the ts span of its rows so the tag UPDATE can be ts-bounded.
type TxIndexKey struct {
	Ledger uint32
	TxHash string
	MinTs  time.Time
	MaxTs  time.Time
}

// TxIndexTag is one (ledger, tx_hash) → apply-order mapping the tx-index tagger writes into trades.tx_index (migration 0196).
type TxIndexTag struct {
	Ledger  uint32
	TxHash  string
	TxIndex uint32
}

// UntaggedTxIndexTrades pages the distinct on-chain (ledger, tx_hash) pairs in [from, to) whose trades lack a tx_index, keyset-ordered strictly after (afterLedger, afterHash); off-chain rows (ledger = 0) are excluded.
func (s *Store) UntaggedTxIndexTrades(ctx context.Context, from, to time.Time, afterLedger uint32, afterHash string, limit int) ([]TxIndexKey, error) {
	const q = `
        SELECT ledger, tx_hash, min(ts), max(ts)
          FROM trades
         WHERE ts >= $1 AND ts < $2
           AND ledger > 0
           AND tx_index IS NULL
           AND (ledger, tx_hash) > ($3, $4)
         GROUP BY ledger, tx_hash
         ORDER BY ledger, tx_hash
         LIMIT $5
    `
	rows, err := s.db.QueryContext(ctx, q, from.UTC(), to.UTC(), int64(afterLedger), afterHash, limit)
	if err != nil {
		return nil, fmt.Errorf("timescale: UntaggedTxIndexTrades: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []TxIndexKey
	for rows.Next() {
		var (
			k      TxIndexKey
			ledger int64
		)
		if err := rows.Scan(&ledger, &k.TxHash, &k.MinTs, &k.MaxTs); err != nil {
			return nil, fmt.Errorf("timescale: UntaggedTxIndexTrades scan: %w", err)
		}
		k.Ledger = uint32(ledger)
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("timescale: UntaggedTxIndexTrades rows: %w", err)
	}
	return out, nil
}

// UntaggedTxIndexTradeExists reports whether [from, to) holds an on-chain trade without a tx_index. EXISTS stops at the first match,
// so a chunk with work is not read whole to say so.
func (s *Store) UntaggedTxIndexTradeExists(ctx context.Context, from, to time.Time) (bool, error) {
	const q = `
        SELECT EXISTS (
            SELECT 1 FROM trades
             WHERE ts >= $1 AND ts < $2
               AND ledger > 0
               AND tx_index IS NULL
        )
    `
	var exists bool
	if err := s.db.QueryRowContext(ctx, q, from.UTC(), to.UTC()).Scan(&exists); err != nil {
		return false, fmt.Errorf("timescale: UntaggedTxIndexTradeExists: %w", err)
	}
	return exists, nil
}

// TagTradesTxIndex back-fills trades.tx_index for the supplied tags across every on-chain source, FIRST-WINS (tx_index IS NULL); tx_index is absent from the trades upsert so a re-derive preserves it.
func (s *Store) TagTradesTxIndex(ctx context.Context, from, to time.Time, tags []TxIndexTag) (int64, error) {
	if len(tags) == 0 {
		return 0, nil
	}
	ledgers := make([]int64, len(tags))
	txHashes := make([]string, len(tags))
	idx := make([]int64, len(tags))
	for i, t := range tags {
		ledgers[i] = int64(t.Ledger)
		txHashes[i] = t.TxHash
		idx[i] = int64(t.TxIndex)
	}
	const q = `
        UPDATE trades t
           SET tx_index = s.tx_index::integer
          FROM (
              SELECT unnest($1::bigint[]) AS ledger,
                     unnest($2::text[])   AS tx_hash,
                     unnest($3::bigint[]) AS tx_index
          ) s
         WHERE t.ts     >= $4
           AND t.ts     <  $5
           AND t.ledger  = s.ledger
           AND t.tx_hash = s.tx_hash
           AND t.ledger  > 0
           AND t.tx_index IS NULL
    `
	res, err := s.db.ExecContext(ctx, q, ledgers, txHashes, idx, from.UTC(), to.UTC())
	if err != nil {
		return 0, fmt.Errorf("timescale: TagTradesTxIndex (%d tags): %w", len(tags), err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("timescale: TagTradesTxIndex rows-affected: %w", err)
	}
	return n, nil
}
