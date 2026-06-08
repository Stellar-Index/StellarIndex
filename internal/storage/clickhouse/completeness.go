package clickhouse

import (
	"context"
	"fmt"
)

// ContiguousWatermark returns the highest ledger L such that stellar.ledgers
// contains every ledger in [from, L] with NO hole — i.e. the lake is provably
// complete from `from` up to L. It is the real-time projector's safe upper read
// bound when reading forward events from CH (ADR-0034 #10 feed-switch).
//
// Why it's needed: the live dual-sink (LiveSink) is best-effort — it DROPS whole
// ledgers under buffer pressure and a flush can partially fail — so CH can have
// holes near the tip. The projector advances its per-source cursor to the upper
// bound unconditionally (to skip event-free stretches), so reading past a hole
// would silently lose that ledger's protocol events. Clamping the upper bound to
// this watermark makes the projector stall AT a hole until the catch-up timer
// heals it, rather than skipping over it.
//
// Completeness is keyed off the ledgers table, which is a per-ledger commit
// marker: Sink.Flush writes stellar.ledgers LAST, so a ledger_seq present there
// guarantees that ledger's contract_events (and all other tables) are already
// durable. A buffer-full drop drops the whole extract, so it leaves no ledgers
// row either — either way "present in ledgers" ⟹ "complete in CH".
//
// Returns from-1 when CH has not yet reached `from` (nothing complete to read);
// callers treat tip <= from as idle.
func ContiguousWatermark(ctx context.Context, addr string, from uint32) (uint32, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()

	// ch_max: highest ledger present in the lake.
	// first_gap_start: the lowest missing ledger >= from (0 when there is none).
	//
	// The leadInFrame frame (CURRENT ROW .. 1 FOLLOWING) returns the current
	// row's own value for the last row in the partition, so the final ledger
	// never registers a spurious trailing gap. min() over an empty gap set
	// returns 0 (UInt default), which we read as "no hole".
	// Both columns are wrapped toUInt64(ifNull(…, 0)) so they scan as plain
	// non-nullable uint64 regardless of CH's promotion rules: scalar subqueries
	// are Nullable, max(ledger_seq) is UInt32 but min(ledger_seq+1) widens to
	// UInt64, and an empty set yields NULL. ifNull(…,0) maps "no gap" / "empty
	// lake" to 0; toUInt64 unifies the width. The driver rejects type
	// mismatches, so this normalization is load-bearing.
	const q = `
		SELECT
			toUInt64(ifNull((SELECT max(ledger_seq) FROM stellar.ledgers), 0)) AS ch_max,
			toUInt64(ifNull((SELECT min(gap_start) FROM (
				SELECT ledger_seq + 1 AS gap_start
				FROM (
					SELECT ledger_seq,
					       leadInFrame(ledger_seq) OVER (
					           ORDER BY ledger_seq ROWS BETWEEN CURRENT ROW AND 1 FOLLOWING
					       ) AS nxt
					FROM (SELECT DISTINCT ledger_seq FROM stellar.ledgers WHERE ledger_seq >= ?)
				)
				WHERE nxt > ledger_seq + 1
			)), 0)) AS first_gap_start`

	var chMax, firstGap uint64
	if err := conn.QueryRow(ctx, q, from).Scan(&chMax, &firstGap); err != nil {
		return 0, fmt.Errorf("clickhouse: contiguous watermark from %d: %w", from, err)
	}
	// Ledger sequences are always well within uint32.
	return watermark(from, uint32(chMax), uint32(firstGap)), nil
}

// watermark is the pure interpretation of a ContiguousWatermark query result:
//   - chMax < from        → from-1 (CH has not reached `from`; nothing complete)
//   - firstGap == 0        → chMax (no hole at or above `from`; complete to the tip)
//   - otherwise            → firstGap-1 (complete up to just before the first hole)
func watermark(from, chMax, firstGap uint32) uint32 {
	if chMax < from {
		return from - 1
	}
	if firstGap == 0 {
		return chMax
	}
	return firstGap - 1
}
