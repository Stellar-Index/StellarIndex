package clickhouse

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// LedgerCloseTimes returns the close time of every ledger in seqs that stellar.ledgers holds; a ledger the lake lacks
// is absent from the result. It reads the span the ledgers cover and keeps the wanted ones, so the set can be millions
// wide without an IN list.
func LedgerCloseTimes(ctx context.Context, addr string, seqs []uint32) (map[uint32]time.Time, error) {
	out := make(map[uint32]time.Time, len(seqs))
	if len(seqs) == 0 {
		return out, nil
	}
	want := make(map[uint32]struct{}, len(seqs))
	lo, hi := seqs[0], seqs[0]
	for _, s := range seqs {
		want[s] = struct{}{}
		lo, hi = min(lo, s), max(hi, s)
	}
	// Identity from the environment; see ops_auth.go.
	auth, err := chAuth()
	if err != nil {
		return nil, err
	}
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:        []string{addr},
		Auth:        auth,
		Settings:    clickhouse.Settings{"max_execution_time": 600},
		DialTimeout: 10 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("clickhouse: open %s: %w", addr, err)
	}
	defer func() { _ = conn.Close() }()

	rows, err := conn.Query(ctx, `SELECT ledger_seq, close_time FROM stellar.ledgers WHERE ledger_seq BETWEEN ? AND ?`, lo, hi)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: ledger close times [%d,%d]: %w", lo, hi, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var seq uint32
		var at time.Time
		if err := rows.Scan(&seq, &at); err != nil {
			return nil, fmt.Errorf("clickhouse: scan ledger close time: %w", err)
		}
		// A re-ingested ledger is held twice until it merges; both rows carry the same close time.
		if _, ok := want[seq]; ok {
			out[seq] = at.UTC()
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: ledger close times [%d,%d]: %w", lo, hi, err)
	}
	return out, nil
}
