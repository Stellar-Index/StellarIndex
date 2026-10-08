package clickhouse

import (
	"context"
	"fmt"
	"math"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// ReconcileEventStreamer adapts the contract_events read path to completeness.EventStreamer, so
// reconciliation reads the certified lake, not the serving DB.
type ReconcileEventStreamer struct {
	Addr string
	// NeedOpArgs includes the wide op_args_xdr column; leave false unless the decoder reads OpArgs
	// (reading it across the CAP-67 firehose OOMs).
	NeedOpArgs bool
	// NeedStateWriteKeys resolves StateWriteKeys from ledger_entry_changes by batched point lookups
	// (state_write_keys.go); same opt-in rationale as NeedOpArgs.
	NeedStateWriteKeys bool
}

// reconcileStreamWindow bounds each query to at most one partition of parts (250k divides the 1M
// partition): one full-history query held a wide in-order read open across every partition. History
// growth adds windows, not per-query memory.
const reconcileStreamWindow = 250_000

// StreamContractEvents streams events for [from,to] narrowed by the source's prefilter, one window
// at a time. NO FINAL (a full-range merge-on-read is too heavy). Unmerged ReplacingMergeTree
// duplicates share a ledger, hence a window, and each window is ORDER BY (ledger, tx_hash,
// op_index, event_index), so they stay ADJACENT and the reconcile dedups by identity in O(1) memory
// (ReDeriveOutputCountsByKindFromEvents).
func (s ReconcileEventStreamer) StreamContractEvents(ctx context.Context, from, to uint32, contractIDs, topic0Syms []string, fn func(events.Event) error) error {
	return forEachLedgerWindow(from, to, reconcileStreamWindow, func(lo, hi uint32) error {
		return StreamContractEventsFiltered(ctx, s.Addr, lo, hi, contractIDs, topic0Syms, nil, false, s.NeedOpArgs, s.NeedStateWriteKeys, fn)
	})
}

// ContiguousWatermark returns the highest ledger L such that stellar.ledgers holds every ledger in
// [from, L] with no hole: the projector's safe upper read bound (ADR-0041).
// Needed because the live dual-sink drops whole ledgers under buffer pressure and a flush can
// partly fail, while the projector advances its cursor unconditionally; reading past a hole would
// silently lose that ledger's events. Clamping to this watermark stalls AT the hole until catch-up
// heals it.
// Keyed off ledgers because Sink.Flush writes it LAST, so presence there implies the ledger's other
// tables are durable.
// Returns from-1 when CH has not reached `from` or `from` itself is a hole (the boundary case the
// interior-gap scan cannot see); callers treat tip <= from as idle.
func ContiguousWatermark(ctx context.Context, addr string, from uint32) (uint32, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	return contiguousWatermarkOn(ctx, conn, from)
}

// contiguousWatermarkOn runs the query on a caller-owned pooled connection
// (ExplorerReader.LakeWatermark, ADR-0041 Decision 4).
func contiguousWatermarkOn(ctx context.Context, conn driver.Conn, from uint32) (uint32, error) {
	return contiguousWatermarkUpTo(ctx, conn, from, math.MaxUint32)
}

// contiguousWatermarkUpTo bounds the gap scan to [from, to] and clamps the result to `to`, so a
// lagging reader pays one batch window per call.
func contiguousWatermarkUpTo(ctx context.Context, conn driver.Conn, from, to uint32) (uint32, error) {
	// first_gap_start sees only INTERIOR gaps (leadInFrame over DISTINCT ledgers finds nxt >
	// ledger+1), so it is blind to a hole at `from` itself: with `from` absent, {from+1, ...} is
	// contiguous and it reads 0. min_present exposes that: when it exceeds `from`, `from` is
	// missing and the watermark stalls at from-1. The healer scripts/ops/ch-live-catchup.sh carries
	// both arms; change them together.
	// leadInFrame returns the row's own value for the last row, so there is no spurious trailing
	// gap; min() over an empty set is 0, read as "no hole".
	// Every column is toUInt64(ifNull(..., 0)): scalar subqueries are Nullable and
	// min(ledger_seq+1) widens to UInt64, and the driver rejects type mismatches.
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
					FROM (SELECT DISTINCT ledger_seq FROM stellar.ledgers WHERE ledger_seq >= ? AND ledger_seq <= ?)
				)
				WHERE nxt > ledger_seq + 1
			)), 0)) AS first_gap_start,
			toUInt64(ifNull((SELECT min(ledger_seq) FROM stellar.ledgers WHERE ledger_seq >= ?), 0)) AS min_present,
			toUInt64(ifNull((SELECT max(ledger_seq) FROM stellar.ledgers WHERE ledger_seq >= ? AND ledger_seq <= ?), 0)) AS max_in_window`

	var chMax, firstGap, minPresent, maxInWindow uint64
	if err := conn.QueryRow(ctx, q, from, to, from, from, to).Scan(&chMax, &firstGap, &minPresent, &maxInWindow); err != nil {
		return 0, fmt.Errorf("clickhouse: contiguous watermark from %d: %w", from, err)
	}
	// Ledger sequences are always well within uint32.
	return boundedWatermark(from, to, uint32(chMax), uint32(firstGap), uint32(minPresent), uint32(maxInWindow)), nil
}

// boundedWatermark is watermark for a scan limited to [from, to]: the scan cannot see a hole
// running past `to`, so when it reports no gap but the window's highest ledger is below the clamp,
// contiguity ends there.
func boundedWatermark(from, to, chMax, firstGap, minPresent, maxInWindow uint32) uint32 {
	w := min(watermark(from, chMax, firstGap, minPresent), to)
	if firstGap == 0 && maxInWindow != 0 && maxInWindow < w {
		return maxInWindow
	}
	return w
}

// WatermarkReader holds one connection for repeated reads by a polling caller (the projector).
type WatermarkReader struct {
	conn driver.Conn
}

// NewWatermarkReader opens the reader's connection.
func NewWatermarkReader(ctx context.Context, addr string) (*WatermarkReader, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	return &WatermarkReader{conn: conn}, nil
}

// ContiguousWatermark is [ContiguousWatermark] on the reader's connection,
// scanning no further than ledger to and never returning above it.
func (w *WatermarkReader) ContiguousWatermark(ctx context.Context, from, to uint32) (uint32, error) {
	return contiguousWatermarkUpTo(ctx, w.conn, from, to)
}

// LakeMinLedger is [LakeMinLedger] on the reader's connection.
func (w *WatermarkReader) LakeMinLedger(ctx context.Context) (uint32, error) {
	return lakeMinLedgerOn(ctx, w.conn)
}

// Close releases the reader's connection.
func (w *WatermarkReader) Close() error { return w.conn.Close() }

// LakeMinLedger returns the lowest ledger_seq in stellar.ledgers (0 when empty): the edge
// ContiguousWatermark cannot see past. Every lake begins at ledger 2, so a derive resuming from
// genesis must clamp up to this or the watermark reports a boundary hole forever.
func LakeMinLedger(ctx context.Context, addr string) (uint32, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()
	return lakeMinLedgerOn(ctx, conn)
}

func lakeMinLedgerOn(ctx context.Context, conn driver.Conn) (uint32, error) {
	// min() over an empty table is 0, read as "no ledger present" (as lakeTipLedger).
	var lo uint64
	if err := conn.QueryRow(ctx, `SELECT toUInt64(min(ledger_seq)) FROM stellar.ledgers`).Scan(&lo); err != nil {
		return 0, fmt.Errorf("clickhouse: lake min ledger: %w", err)
	}
	return uint32(lo), nil // ledger sequences fit uint32
}

// substrateWindow bounds each substrate query: both checks need a full sort of their range (window
// functions), and a whole-lake span exceeds the query memory cap. Both properties are LOCAL
// (neighbour contiguity, neighbour hash-link), so windows with a 1-ledger overlap prove the same
// claim at bounded memory.
const substrateWindow = 5_000_000

// substrateChainGenesis is ledger 1, which has no predecessor, so the earliest checkable link is at
// ledger 2. substrateQueryLo never goes below it.
const substrateChainGenesis = uint64(1)

// substrateQueryLo is a window query's lower bound, one ledger BELOW the span it certifies, so the
// seam pair (wlo-1, wlo) is hash-checked, including the first window's (from-1, from) junction.
// Guarded at substrateChainGenesis against underflow.
func substrateQueryLo(wlo, from uint64) uint64 {
	switch {
	case wlo > from:
		return wlo - 1
	case from > substrateChainGenesis:
		return from - 1
	default:
		return from
	}
}

// SubstrateProblem returns the earliest ledger in [from,to] where the lake substrate fails
// (ADR-0033 Claim 1): a missing ledger or a hash-chain break (prev_hash != prior ledger_hash). (0,
// false) means continuous and hash-linked over the whole range.
// Both checks run over a per-ledger dedup (GROUP BY ledger_seq, argMax by ingested_at) so duplicate
// parts do not create false breaks. Windows overlap by one ledger and the first problem found is
// returned.
func SubstrateProblem(ctx context.Context, addr string, from, to uint32) (problem uint32, hasProblem bool, detail string, err error) {
	conn, oerr := openRead(ctx, addr)
	if oerr != nil {
		return 0, false, "", oerr
	}
	defer func() { _ = conn.Close() }()
	return substrateProblemOn(ctx, conn, addr, from, to)
}

// substrateProblemOn is SubstrateProblem on a given connection (unit-testable with a fake
// driver.Conn). addr is used only by the seam-hole fallback, which opens its own connection.
func substrateProblemOn(ctx context.Context, conn driver.Conn, addr string, from, to uint32) (problem uint32, hasProblem bool, detail string, err error) {
	const gapQ = `
		SELECT toUInt64(ifNull((SELECT min(gap_start) FROM (
			SELECT ledger_seq + 1 AS gap_start
			FROM (
				SELECT ledger_seq, leadInFrame(ledger_seq) OVER (
					ORDER BY ledger_seq ROWS BETWEEN CURRENT ROW AND 1 FOLLOWING
				) AS nxt
				FROM (SELECT DISTINCT ledger_seq FROM stellar.ledgers WHERE ledger_seq BETWEEN ? AND ?)
			)
			WHERE nxt > ledger_seq + 1
		)), 0))`
	// First hash-chain break. One tuple argMax, so on an ingested_at tie both hashes come from the
	// same row.
	const chainQ = `
		SELECT toUInt64(ifNull((SELECT min(ledger_seq) FROM (
			SELECT ledger_seq, prev_hash,
			       lagInFrame(ledger_hash) OVER (ORDER BY ledger_seq) AS prior_hash
			FROM (
				SELECT ledger_seq, hp.1 AS ledger_hash, hp.2 AS prev_hash
				FROM (
					SELECT ledger_seq, argMax((ledger_hash, prev_hash), ingested_at) AS hp
					FROM stellar.ledgers WHERE ledger_seq BETWEEN ? AND ?
					GROUP BY ledger_seq
				)
			)
		) WHERE ledger_seq > ? AND prior_hash != '' AND prev_hash != prior_hash), 0))`

	// Endpoint-presence guard, fail-closed: the windowed scan finds only holes BETWEEN present
	// ledgers, so an empty range or one missing its head/tail would read as intact and falsely
	// certify lake_complete during a partial restore. Assert endpoints and count up front; the tail
	// is re-checked after the scan.
	var haveMin, haveMax, present uint64
	const endpointsQ = `SELECT toUInt64(ifNull(min(ledger_seq),0)), toUInt64(ifNull(max(ledger_seq),0)), toUInt64(uniqExact(ledger_seq)) FROM stellar.ledgers WHERE ledger_seq BETWEEN ? AND ?`
	if qerr := conn.QueryRow(ctx, endpointsQ, from, to).Scan(&haveMin, &haveMax, &present); qerr != nil {
		return 0, false, "", fmt.Errorf("clickhouse: substrate endpoint presence [%d,%d]: %w", from, to, qerr)
	}
	headProblem, headHasProblem, headDetail := substrateHeadProblem(from, to, present > 0, uint32(haveMin))
	if present == 0 {
		// An empty range has no interior to certify.
		return headProblem, true, headDetail, nil
	}

	// A truncated head does NOT excuse the walks: [haveMin, to] is real data, and a source with
	// genesis >= haveMin is only certified clean by walking it. [from, haveMin) is already known
	// missing, so the walk starts at haveMin.
	walkFrom := uint64(from)
	if headHasProblem {
		walkFrom = haveMin
	}

	for wlo := walkFrom; wlo <= uint64(to); wlo += substrateWindow {
		whi := wlo + substrateWindow
		if whi > uint64(to) {
			whi = uint64(to)
		}
		// The window starts one ledger BEFORE the span it certifies (including the first, whose
		// seam is the -from junction) so the seam pair is hash-checked and a seam gap is caught.
		qlo := substrateQueryLo(wlo, walkFrom)

		var firstGap uint64
		if qerr := conn.QueryRow(ctx, gapQ, qlo, whi).Scan(&firstGap); qerr != nil {
			return 0, false, "", fmt.Errorf("clickhouse: substrate contiguity [%d,%d]: %w", qlo, whi, qerr)
		}
		var firstBreak uint64
		if qerr := conn.QueryRow(ctx, chainQ, qlo, whi, qlo).Scan(&firstBreak); qerr != nil {
			return 0, false, "", fmt.Errorf("clickhouse: substrate hash-chain [%d,%d]: %w", qlo, whi, qerr)
		}

		switch {
		case firstGap == 0 && firstBreak == 0:
			continue
		case firstGap != 0 && (firstBreak == 0 || firstGap <= firstBreak):
			return uint32(firstGap), true, fmt.Sprintf("substrate: missing ledger at %d", firstGap), nil
		default:
			return uint32(firstBreak), true, fmt.Sprintf("substrate: hash-chain break at %d", firstBreak), nil
		}
	}
	if headHasProblem {
		// The interior is clean but [from, haveMin) was never walked: report the head problem (the
		// FIRST problem in [from, to]). sourceSubstrateOK's `problem < genesis` still passes
		// sources whose genesis is >= haveMin.
		return headProblem, true, headDetail, nil
	}
	// Tail-presence guard: if the last present ledger is below `to`, return `to` (not haveMax+1) so
	// `problem < genesis` fails EVERY source, not just those below haveMax+1.
	if haveMax < uint64(to) {
		return to, true, fmt.Sprintf("substrate: missing tail ledger(s) — last present is %d, expected %d", haveMax, to), nil
	}
	// Total-count guard: a hole spanning both overlap ledgers of a window seam has no present
	// ledger on either side in ANY window, so gapQ and chainQ are blind to it. Compare `present`
	// (uniqExact over the range) against the full range size.
	if !substrateCountIntact(from, to, present) {
		return substrateLocateHole(ctx, addr, from, to)
	}
	return 0, false, "", nil
}

// substrateCountIntact is the pure total-count decision (unit-testable without a lake).
func substrateCountIntact(from, to uint32, present uint64) bool {
	return present == uint64(to)-uint64(from)+1
}

// substrateLocateHole finds the first missing ledger once substrateCountIntact proves a
// seam-straddling hole. It tiles [from,to] into non-overlapping windows (existence is enough) and
// asks QueryMissingLedgerSeqs for the first tile with a deficit.
func substrateLocateHole(ctx context.Context, addr string, from, to uint32) (problem uint32, hasProblem bool, detail string, err error) {
	werr := forEachLedgerWindow(from, to, substrateWindow, func(lo, hi uint32) error {
		if hasProblem {
			return nil
		}
		missing, merr := QueryMissingLedgerSeqs(ctx, addr, lo, hi)
		if merr != nil {
			return merr
		}
		if len(missing) > 0 {
			problem, hasProblem = missing[0], true
		}
		return nil
	})
	if werr != nil {
		return 0, false, "", werr
	}
	if !hasProblem {
		return 0, false, "", fmt.Errorf("clickhouse: substrate presence count mismatch in [%d,%d] but no missing ledger located", from, to)
	}
	return problem, true, fmt.Sprintf("substrate: missing ledger at %d (window-seam hole)", problem), nil
}

// substrateHeadProblem is the pure low-ledger coverage decision: an empty range or a missing head,
// which the gap scan cannot see. The problem ledger keeps the consumer's `problem < genesis =>
// source-OK` test correct for coverage failures: empty returns `to` so EVERY source fails; a
// missing head returns haveMin-1 so only sources whose data begins in the absent head fail.
func substrateHeadProblem(from, to uint32, present bool, haveMin uint32) (problem uint32, hasProblem bool, detail string) {
	if !present {
		return to, true, fmt.Sprintf("substrate: no ledgers present in [%d,%d] (empty range — not intact)", from, to)
	}
	if haveMin > from {
		return haveMin - 1, true, fmt.Sprintf("substrate: missing head ledger(s) — first present is %d, expected %d (fails every source with genesis ≤ %d)", haveMin, from, haveMin-1)
	}
	return 0, false, ""
}

// watermark is the pure interpretation of a ContiguousWatermark result:
//   - chMax < from: from-1 (CH has not reached `from`)
//   - minPresent > from: from-1 (hole AT `from`)
//   - firstGap == 0: chMax (complete to the tip)
//   - otherwise: firstGap-1
//
// The minPresent guard closes a data-loss blind spot: with `from` absent, {from+1, ...} is
// contiguous, so returning chMax would let the projector skip `from` and permanently drop its
// sole-writer rows. Order matters: after the chMax<from guard, minPresent >= from, so `minPresent >
// from` means `from` is missing; from-1 is the tightest bound and wins over any interior gap above
// it.
func watermark(from, chMax, firstGap, minPresent uint32) uint32 {
	if from == 0 {
		// Ledger 0 does not exist, and from-1 would wrap to MaxUint32, read as "complete forever".
		from = 1
	}
	if chMax < from {
		return from - 1
	}
	if minPresent > from {
		return from - 1
	}
	if firstGap == 0 {
		return chMax
	}
	return firstGap - 1
}

// eventCensusPartitionWidth mirrors PARTITION BY intDiv(ledger_seq, 1000000) on stellar.ledgers and
// contract_events (tier1_schema.sql).
const eventCensusPartitionWidth = 1_000_000

// EventCensusShortfall is one contract_events partition holding fewer rows than stellar.ledgers
// says its ledgers emitted.
type EventCensusShortfall struct {
	// Partition is intDiv(ledger_seq, 1_000_000).
	Partition uint32
	// FirstEventLedger is the lowest ledger in the partition whose
	// soroban_event_count is non-zero.
	FirstEventLedger uint32
	// Expected is Σ soroban_event_count (deduplicated per ledger); Present is the active-part row
	// count.
	Expected uint64
	Present  uint64
}

// EventCensusShortfalls cross-checks contract_events against ledgers per partition touched by
// [from, to].
// SubstrateProblem proves only stellar.ledgers, and "ledgers is written LAST" holds for ingest
// alone: a DROP/REPLACE PARTITION or a restore that brings ledgers back first
// (docs/operations/clickhouse-destructive-ddl.md) leaves ledgers intact over an empty event table.
// soroban_event_count increments exactly when a contract_events row is appended, so fewer rows
// means lost events.
// Present counts active parts (system.parts); unmerged duplicates only raise it, so no false
// shortfall (a partial loss masked by as many duplicates goes undetected). Expected is read BEFORE
// present, since the sink flushes contract_events before ledgers.
func EventCensusShortfalls(ctx context.Context, addr string, from, to uint32) ([]EventCensusShortfall, error) {
	if from > to {
		return nil, nil
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()
	expected, err := eventCensusExpected(ctx, conn, from-from%eventCensusPartitionWidth, to)
	if err != nil {
		return nil, err
	}
	present, err := eventCensusPresent(ctx, conn)
	if err != nil {
		return nil, err
	}
	return censusShortfalls(expected, present), nil
}

func eventCensusExpected(ctx context.Context, conn driver.Conn, lo, hi uint32) ([]EventCensusShortfall, error) {
	const q = `
		SELECT toUInt32(intDiv(ledger_seq, 1000000)) AS p,
		       toUInt64(sum(cnt)),
		       toUInt32(minIf(ledger_seq, cnt > 0))
		FROM (
			SELECT ledger_seq, argMax(soroban_event_count, ingested_at) AS cnt
			FROM stellar.ledgers WHERE ledger_seq BETWEEN ? AND ?
			GROUP BY ledger_seq
		)
		GROUP BY p HAVING sum(cnt) > 0
		ORDER BY p`
	rows, err := conn.Query(ctx, q, lo, hi)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: event census expected [%d,%d]: %w", lo, hi, err)
	}
	defer func() { _ = rows.Close() }()
	var out []EventCensusShortfall
	for rows.Next() {
		var s EventCensusShortfall
		if err := rows.Scan(&s.Partition, &s.Expected, &s.FirstEventLedger); err != nil {
			return nil, fmt.Errorf("clickhouse: scan event census expected: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func eventCensusPresent(ctx context.Context, conn driver.Conn) (map[uint32]uint64, error) {
	m, err := rawCensusPresent(ctx, conn, "contract_events")
	if err != nil {
		return nil, err
	}
	if m["contract_events"] == nil {
		return map[uint32]uint64{}, nil
	}
	return m["contract_events"], nil
}

// censusShortfalls keeps expected partitions whose present count falls short; an absent partition
// reads as 0. Pure.
func censusShortfalls(expected []EventCensusShortfall, present map[uint32]uint64) []EventCensusShortfall {
	var out []EventCensusShortfall
	for _, e := range expected {
		e.Present = present[e.Partition]
		if e.Present < e.Expected {
			out = append(out, e)
		}
	}
	return out
}
