package clickhouse

import (
	"context"
	"errors"
	"fmt"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// SponsorshipTotalsBucketLedgers is the width of one stellar.sponsorship_totals_buckets bucket.
const SponsorshipTotalsBucketLedgers = 10_000

const sponsorshipTotalsWatermarkName = "sponsorship_totals"

// ErrSponsorshipTotalsUnfilled reports a refused first advance of the totals watermark over a window that starts above
// the first derived event: the buckets below it were never written, so the sums would undercount.
var ErrSponsorshipTotalsUnfilled = errors.New("clickhouse: sponsorship totals would skip derived events below the window")

// ErrSponsorshipTotalsAheadOfEvents reports a refused advance past the sponsorship_events watermark.
var ErrSponsorshipTotalsAheadOfEvents = errors.New("clickhouse: sponsorship totals window runs ahead of the sponsorship_events watermark")

// sponsorshipBucketDeltas is one row per arrangement and bucket: the net in-force change across the bucket (the first
// event shows whether it entered in force) and its distinct events; $1/$2 bound the ledgers.
const sponsorshipBucketDeltas = `SELECT intDiv(ledger, 10000) AS bucket, sponsor, sponsored,
		toInt64(argMax(event, (ledger, intra_ledger_seq, change_index)) = 'started')
			- toInt64(argMin(event, (ledger, intra_ledger_seq, change_index)) != 'started') AS d,
		max(reserves) AS res,
		uniqExactIf((ledger, tx_hash, op_index, change_index), event = 'started') AS ns,
		uniqExactIf((ledger, tx_hash, op_index, change_index), event = 'revoked') AS nr,
		uniqExactIf((ledger, tx_hash, op_index, change_index), event = 'ended') AS ne
	FROM stellar.sponsorship_events
	WHERE %s ledger BETWEEN ? AND ?
	GROUP BY bucket, sponsor, sponsored, kind, key_id`

// RefreshSponsorshipTotals recomputes every bucket [from, thru] touches from the events, whole buckets at a time so a
// refresh is idempotent; an account a bucket no longer holds is rewritten as zero rather than left stale.
func RefreshSponsorshipTotals(ctx context.Context, addr string, from, thru uint32) error {
	if thru < from {
		return fmt.Errorf("clickhouse: sponsorship totals window [%d,%d] is not a ledger range", from, thru)
	}
	bLo, bHi := from/SponsorshipTotalsBucketLedgers, thru/SponsorshipTotalsBucketLedgers
	conn, err := openAccountMovementsWrite(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	q := `INSERT INTO stellar.sponsorship_totals_buckets (account, side, bucket, in_force, in_force_reserves, started, revoked, ended)
		SELECT account, side, bucket, sum(d), sum(dr), sum(ns), sum(nr), sum(ne) FROM (
			SELECT sa.2 AS account, sa.1 AS side, bucket, d, d * toInt64(res) AS dr, ns, nr, ne
			FROM (` + fmt.Sprintf(sponsorshipBucketDeltas, "") + `)
			ARRAY JOIN [('sponsor', sponsor), ('sponsored', sponsored)] AS sa
			WHERE sa.2 != ''
			UNION ALL
			SELECT account, side, bucket, toInt64(0), toInt64(0), toUInt64(0), toUInt64(0), toUInt64(0)
			FROM stellar.sponsorship_totals_buckets FINAL WHERE bucket BETWEEN ? AND ?)
		GROUP BY account, side, bucket
		SETTINGS max_execution_time = 3600, max_threads = 8, max_memory_usage = 12000000000,
			max_bytes_before_external_group_by = 4000000000`
	lo, hi := bLo*SponsorshipTotalsBucketLedgers, bHi*SponsorshipTotalsBucketLedgers+SponsorshipTotalsBucketLedgers-1
	if err := conn.Exec(ctx, q, lo, hi, bLo, bHi); err != nil {
		return fmt.Errorf("clickhouse: refresh sponsorship totals buckets [%d,%d]: %w", bLo, bHi, err)
	}
	return nil
}

// SponsorshipTotalsWatermark returns the highest ledger whose buckets are complete from the first derived event (0 = never filled).
func SponsorshipTotalsWatermark(ctx context.Context, addr string) (uint32, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	return entryWatermarkOn(ctx, conn, sponsorshipTotalsWatermarkName)
}

// SetSponsorshipTotalsWatermark records the buckets complete through `thru`: never past the events, and a first advance
// only from a window that starts at or below the first derived event.
func SetSponsorshipTotalsWatermark(ctx context.Context, addr string, from, thru uint32) error {
	return advanceEntryWatermark(ctx, addr, sponsorshipTotalsWatermarkName, from, thru, func(ctx context.Context, conn driver.Conn) error {
		ev, err := entryWatermarkOn(ctx, conn, sponsorshipWatermarkName)
		if err != nil {
			return err
		}
		if ev < thru {
			return fmt.Errorf("%w: [%d,%d] but the events stop at %d", ErrSponsorshipTotalsAheadOfEvents, from, thru, ev)
		}
		wm, err := entryWatermarkOn(ctx, conn, sponsorshipTotalsWatermarkName)
		if err != nil || wm > 0 {
			return err
		}
		var first uint32
		var n uint64
		if err := conn.QueryRow(ctx, `SELECT min(ledger), count() FROM stellar.sponsorship_events`).Scan(&first, &n); err != nil {
			return fmt.Errorf("clickhouse: first sponsorship event: %w", err)
		}
		if n > 0 && first < from-from%SponsorshipTotalsBucketLedgers {
			return fmt.Errorf("%w: [%d,%d] starts above the first event at %d", ErrSponsorshipTotalsUnfilled, from, thru, first)
		}
		return nil
	})
}
