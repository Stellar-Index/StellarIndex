package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// TxIndexReader resolves tx hashes to their intra-ledger application order (tx_index) from stellar.tx_hash_index for the MEV sandwich detectors; construct once, reuse, Close at shutdown.
type TxIndexReader struct {
	conn driver.Conn
}

// NewTxIndexReader dials ClickHouse with a small pool and pings it, authenticating as the environment's identity (chAuth).
func NewTxIndexReader(ctx context.Context, addr string) (*TxIndexReader, error) {
	auth, err := chAuth()
	if err != nil {
		return nil, err
	}
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:            []string{addr},
		Auth:            auth,
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

// txIndexChunk bounds each IN-list: the table has no partition, so a wide
// list reads a granule per unmerged part per key and trips MEMORY_LIMIT.
const txIndexChunk = 500

// TxIndexes returns tx_hash to tx_index for every hash the lake knows; missing hashes are absent (callers degrade, never guess), max() collapses RMT duplicates, and a failed chunk aborts the call rather than return a partial map.
func (r *TxIndexReader) TxIndexes(ctx context.Context, hashes []string) (map[string]uint32, error) {
	out := make(map[string]uint32, len(hashes))
	err := forEachTxHashChunk(ctx, hashes, obs.MEVLakeOrderLookupSkippedTotal, func(chunk []string) error {
		return r.txIndexesChunk(ctx, chunk, out)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// TxLedgerIndex is one (ledger_seq, tx_index) the lake holds for a hash.
type TxLedgerIndex struct {
	Ledger  uint32
	TxIndex uint32
}

// TxLedgerIndexes returns tx_hash to every (ledger_seq, tx_index) the lake holds so a writer can confirm the ledger; same absent-not-zero and abort-on-failure contract as TxIndexes.
func (r *TxIndexReader) TxLedgerIndexes(ctx context.Context, hashes []string) (map[string][]TxLedgerIndex, error) {
	out := make(map[string][]TxLedgerIndex, len(hashes))
	err := forEachTxHashChunk(ctx, hashes, obs.TxIndexTagLookupSkippedTotal, func(chunk []string) error {
		return r.txLedgerIndexesChunk(ctx, chunk, out)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// forEachTxHashChunk runs read over txIndexChunk-sized slices and stops at the
// first failure, adding every hash in the call to skipped unless ctx ended.
func forEachTxHashChunk(ctx context.Context, hashes []string, skipped prometheus.Counter, read func([]string) error) error {
	for _, chunk := range chunkStrings(hashes, txIndexChunk) {
		if err := read(chunk); err != nil {
			if ctx.Err() == nil {
				skipped.Add(float64(len(hashes)))
			}
			return fmt.Errorf("chunk of %d hashes: %w", len(chunk), err)
		}
	}
	return nil
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

func (r *TxIndexReader) txLedgerIndexesChunk(ctx context.Context, hashes []string, out map[string][]TxLedgerIndex) error {
	const q = `
        SELECT tx_hash, ledger_seq, max(tx_index)
          FROM stellar.tx_hash_index
         WHERE tx_hash IN (?)
         GROUP BY tx_hash, ledger_seq
    `
	rows, err := r.conn.Query(ctx, q, hashes)
	if err != nil {
		return fmt.Errorf("clickhouse: TxLedgerIndexes query: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var (
			hash string
			row  TxLedgerIndex
		)
		if err := rows.Scan(&hash, &row.Ledger, &row.TxIndex); err != nil {
			return fmt.Errorf("clickhouse: TxLedgerIndexes scan: %w", err)
		}
		out[hash] = append(out[hash], row)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("clickhouse: TxLedgerIndexes rows: %w", err)
	}
	return nil
}

// Close releases the connection pool.
func (r *TxIndexReader) Close() error {
	return r.conn.Close()
}
