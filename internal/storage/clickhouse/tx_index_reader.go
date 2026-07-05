package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// TxIndexReader resolves tx hashes to their intra-ledger application
// order (tx_index) from the lake's stellar.tx_hash_index — the
// hash-ordered lookup table kept current by a materialized view over
// stellar.transactions (ADR-0034 / perf-todo §4). This is the
// ordering signal the Postgres served tier does not carry; the MEV
// worker's sandwich detectors consume it (mev.TxOrderResolver).
//
// Construct once at startup, reuse, Close at shutdown.
type TxIndexReader struct {
	conn driver.Conn
}

// NewTxIndexReader dials ClickHouse with a small pool and pings it.
func NewTxIndexReader(ctx context.Context, addr string) (*TxIndexReader, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:            []string{addr},
		Auth:            clickhouse.Auth{Database: "stellar"},
		Settings:        clickhouse.Settings{"max_execution_time": 30},
		DialTimeout:     10 * time.Second,
		ReadTimeout:     30 * time.Second,
		MaxOpenConns:    4,
		MaxIdleConns:    2,
		ConnMaxLifetime: time.Hour,
	})
	if err != nil {
		return nil, fmt.Errorf("clickhouse: open tx-index reader %s: %w", addr, err)
	}
	if err := conn.Ping(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clickhouse: ping tx-index reader %s: %w", addr, err)
	}
	return &TxIndexReader{conn: conn}, nil
}

// txIndexChunk bounds each IN-list lookup; tx_hash is the table's
// primary key so every chunk is a set of PK point lookups.
const txIndexChunk = 2000

// TxIndexes returns tx_hash → tx_index for every hash the lake knows.
// Missing hashes (not yet indexed — the tx_hash_index historical
// backfill is windowed) are simply absent from the map; callers
// degrade rather than guess. max() collapses ReplacingMergeTree
// duplicates that haven't merged yet (tx_index is identical across
// duplicates of one hash).
func (r *TxIndexReader) TxIndexes(ctx context.Context, hashes []string) (map[string]uint32, error) {
	out := make(map[string]uint32, len(hashes))
	for start := 0; start < len(hashes); start += txIndexChunk {
		end := start + txIndexChunk
		if end > len(hashes) {
			end = len(hashes)
		}
		if err := r.txIndexesChunk(ctx, hashes[start:end], out); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (r *TxIndexReader) txIndexesChunk(ctx context.Context, hashes []string, out map[string]uint32) error {
	const q = `
        SELECT tx_hash, max(tx_index)
          FROM stellar.tx_hash_index
         WHERE tx_hash IN (?)
         GROUP BY tx_hash
    `
	rows, err := r.conn.Query(ctx, q, hashes)
	if err != nil {
		return fmt.Errorf("clickhouse: TxIndexes query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			hash string
			idx  uint32
		)
		if err := rows.Scan(&hash, &idx); err != nil {
			return fmt.Errorf("clickhouse: TxIndexes scan: %w", err)
		}
		out[hash] = idx
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("clickhouse: TxIndexes rows: %w", err)
	}
	return nil
}

// Close releases the connection pool.
func (r *TxIndexReader) Close() error {
	return r.conn.Close()
}
