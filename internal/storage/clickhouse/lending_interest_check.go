package clickhouse

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
)

// LendingInterestFloor returns the first ledger the interest rows cover (0 = never advanced).
func LendingInterestFloor(ctx context.Context, addr string) (uint32, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	return entryWatermarkRow(ctx, conn, lendingInterestFloorName)
}

// LedgerHour is the UTC hour a ledger closed in and the stored ledgers that bound it.
type LedgerHour struct {
	Hour        time.Time
	First, Last uint32 // lowest and highest ledger stellar.ledgers holds in the hour
	// Closed: ledger Last+1 is stored and closes in a later hour, so no ledger of the hour is still to come.
	Closed bool
}

// maxLedgersPerHour bounds an hour's ledgers: close times strictly increase at one-second resolution.
const maxLedgersPerHour = 3600

const lendingInterestHourQuery = `SELECT min(ledger_seq), max(ledger_seq) FROM stellar.ledgers
	WHERE ledger_seq BETWEEN ? AND ? AND close_time >= ? AND close_time < ?`

// LendingInterestHour returns the hour ledger seq closed in, with its first and last stored ledgers.
func LendingInterestHour(ctx context.Context, addr string, seq uint32) (LedgerHour, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return LedgerHour{}, err
	}
	defer func() { _ = conn.Close() }()
	h, ok, err := ledgerHour(ctx, conn, seq)
	if err != nil {
		return LedgerHour{}, err
	}
	if !ok {
		return LedgerHour{}, fmt.Errorf("clickhouse: lending interest: ledger %d is not in stellar.ledgers", seq)
	}
	out := LedgerHour{Hour: h}
	if err := conn.QueryRow(ctx, lendingInterestHourQuery, seq-min(seq-1, maxLedgersPerHour), seq+maxLedgersPerHour,
		h, h.Add(time.Hour)).Scan(&out.First, &out.Last); err != nil {
		return LedgerHour{}, fmt.Errorf("clickhouse: lending interest: ledgers of hour %s: %w", h.Format(time.RFC3339), err)
	}
	next, ok, err := ledgerHour(ctx, conn, out.Last+1)
	if err != nil {
		return LedgerHour{}, err
	}
	out.Closed = ok && next.After(h)
	return out, nil
}

// LendingInterestStateRow is one lending_state_hourly row of a ResData or ProtocolFees key, decoded into Value's
// Reserve or Fees. Before marks the key's last row before the range; a ResData removal sets Value.Removed, a
// ProtocolFees removal reads as the empty map.
type LendingInterestStateRow struct {
	Key    LendingInterestKey
	Hour   time.Time
	Ledger uint32
	Before bool
	Value  lendinginterest.Version
}

const lendingInterestStateRowsQuery = `SELECT key_xdr, hour, ledger_seq, entry_xdr FROM stellar.lending_state_hourly FINAL
	WHERE contract_id IN ? AND key_xdr IN ? AND hour >= ? AND hour < ?
	ORDER BY key_xdr, hour`

const lendingInterestStateBeforeQuery = `SELECT key_xdr, last.1, last.2, last.3 FROM (
		SELECT key_xdr, argMax((hour, ledger_seq, entry_xdr), hour) AS last
		FROM stellar.lending_state_hourly FINAL
		WHERE contract_id IN ? AND key_xdr IN ? AND hour < ?
		GROUP BY key_xdr
	)`

// LendingInterestStateRows reads what the in-line identities compare the fold against: every lending_state_hourly
// row of keys' ResData and ProtocolFees keys in hours [fromHour, toHour), plus each key's last row before fromHour.
// It refuses unless lending_state holds every ledger closing before toHour.
func LendingInterestStateRows(ctx context.Context, addr string, pools []LendingContract, keys []LendingInterestKey,
	fromHour, toHour time.Time,
) ([]LendingInterestStateRow, error) {
	ids := make([]any, 0, len(pools))
	for _, c := range pools {
		ids = append(ids, c.ID)
	}
	byKey := map[string]LendingInterestKey{}
	var list []any
	for _, k := range keys {
		if k.Kind == lendinginterest.KindReserve || k.Kind == lendinginterest.KindFees {
			byKey[k.Key] = k
			list = append(list, k.Key)
		}
	}
	if len(ids) == 0 || len(list) == 0 || !toHour.After(fromHour) {
		return nil, errors.New("clickhouse: lending interest state rows: no pools, no keys or an empty hour range")
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	if err := lendingStateBefore(ctx, conn, toHour); err != nil {
		return nil, err
	}
	var out []LendingInterestStateRow
	for _, q := range []struct {
		sql    string
		args   []any
		before bool
	}{
		{lendingInterestStateBeforeQuery, []any{clickhouse.GroupSet{Value: ids}, clickhouse.GroupSet{Value: list}, fromHour.UTC()}, true},
		{lendingInterestStateRowsQuery, []any{clickhouse.GroupSet{Value: ids}, clickhouse.GroupSet{Value: list}, fromHour.UTC(), toHour.UTC()}, false},
	} {
		rows, err := conn.Query(ctx, q.sql, q.args...)
		if err != nil {
			return nil, fmt.Errorf("clickhouse: lending interest state rows [%s,%s): %w", fromHour, toHour, err)
		}
		got, err := scanLendingInterestState(rows, byKey, q.before)
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	return out, nil
}

type stateRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanLendingInterestState(rows stateRows, byKey map[string]LendingInterestKey, before bool) ([]LendingInterestStateRow, error) {
	var out []LendingInterestStateRow
	for rows.Next() {
		var key, entry string
		r := LendingInterestStateRow{Before: before}
		if err := rows.Scan(&key, &r.Hour, &r.Ledger, &entry); err != nil {
			return nil, fmt.Errorf("clickhouse: lending interest state rows: %w", err)
		}
		k, ok := byKey[key]
		if !ok {
			return nil, fmt.Errorf("clickhouse: lending interest state row for unrequested key %s", key)
		}
		r.Key, r.Hour = k, r.Hour.UTC()
		switch {
		case entry == "" && k.Kind == lendinginterest.KindFees:
			r.Value.Fees = noBLNTFees()
		case entry == "":
			r.Value.Removed = true
		default:
			if err := decodeLendingInterestEntry(k, entry, &r.Value); err != nil {
				return nil, fmt.Errorf("clickhouse: lending interest state row: ledger %d key %s: %w", r.Ledger, key, err)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
