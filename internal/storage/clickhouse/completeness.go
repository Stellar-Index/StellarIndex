package clickhouse

import (
	"context"
	"fmt"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// ReconcileEventStreamer adapts the CH contract_events read path to the
// completeness package's EventStreamer seam (projection reconciliation sourced
// from the certified lake, off the serving DB). Satisfies
// completeness.EventStreamer structurally.
type ReconcileEventStreamer struct {
	Addr string
	// NeedOpArgs includes the WIDE op_args_xdr column in the read. Only a
	// decoder that consumes events.Event.OpArgs needs it (redstone's
	// write_prices feed-id zip — PR 166); every other decoder works from
	// topics + data. Reading op_args_xdr across the CAP-67 firehose was one
	// leg of the 2026-07-08 sep41 completeness OOMs — leave false unless the
	// source's decoder reads OpArgs.
	NeedOpArgs bool
}

// reconcileStreamWindow is the per-query ledger span for the reconcile event
// stream. A full-history reconcile (sep41: soroban-era genesis → tip, 13M+
// ledgers of watched-contract CAP-67 traffic) as ONE query kept a wide
// in-order read open across every partition — buffers scaled with parts ×
// column width until the server cap killed it. Per-window queries bound the
// open read to ≤1 partition of parts (250k divides the 1M partition size);
// history growth adds windows (time), not per-query memory.
const reconcileStreamWindow = 250_000

// StreamContractEvents streams events.Event for [from,to] narrowed by the
// source's prefilter, in reconcileStreamWindow-sized queries. NO FINAL — that
// forces a full-range merge-on-read and is far too heavy on the shared host.
// Un-merged ReplacingMergeTree duplicate parts (the re-run partitions
// 25/45/62) would inflate counts, but each window streams ORDER BY (ledger,
// tx_hash, op_index, event_index) and windows are ascending ledger ranges, so
// duplicates (which share a ledger, hence a window) remain ADJACENT across the
// whole callback sequence and the reconcile dedups them by identity in O(1)
// memory (see ReDeriveOutputCountsByKindFromEvents). Correct + gentle, no
// OPTIMIZE needed.
func (s ReconcileEventStreamer) StreamContractEvents(ctx context.Context, from, to uint32, contractIDs, topic0Syms []string, fn func(events.Event) error) error {
	return forEachLedgerWindow(from, to, reconcileStreamWindow, func(lo, hi uint32) error {
		return StreamContractEventsFiltered(ctx, s.Addr, lo, hi, contractIDs, topic0Syms, nil, false, s.NeedOpArgs, fn)
	})
}

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

// substrateWindow is the per-query ledger span for the substrate audit.
// Both substrate checks need a full sort of the range they inspect (the
// window functions), and a whole-lake span (63M+ ledgers at 2026-06-12,
// growing forever) exceeds CH's 12 GiB query memory cap — first in the
// AggregatingTransform, then (with external spill enabled) in the
// MergingSortedTransform. Windowing is the durable fix: both properties
// are LOCAL (contiguity between neighbours; hash-link between
// neighbours), so checking windows with a 1-ledger overlap proves
// exactly the same claim at bounded memory, at any lake size. 5M rows
// sorts comfortably in-memory per query.
const substrateWindow = 5_000_000

// SubstrateProblem returns the earliest ledger in [from,to] where the CH lake's
// substrate fails (ADR-0033 Claim 1): a missing ledger (contiguity gap) or a
// hash-chain break (prev_hash != the prior ledger's ledger_hash). Returns
// (0, false) when the substrate is intact over the whole range — i.e. the lake
// is provably continuous + hash-linked, the strongest "we captured everything"
// claim. This is the cheap, re-runnable form of the one-shot #7 certification.
//
// Both checks run over a per-ledger dedup (GROUP BY ledger_seq, argMax by
// ingested_at) so ReplacingMergeTree duplicate parts don't create false breaks.
// The audit runs in substrateWindow-sized spans with a 1-ledger overlap (the
// seam link is checked by the next window's WHERE ledger_seq > seam-1 bound),
// returning the FIRST problem found so the windowing is observationally
// identical to the old single-query form.
func SubstrateProblem(ctx context.Context, addr string, from, to uint32) (problem uint32, hasProblem bool, detail string, err error) {
	conn, oerr := openRead(ctx, addr)
	if oerr != nil {
		return 0, false, "", oerr
	}
	defer func() { _ = conn.Close() }()

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
	// First hash-chain break: prev_hash != the immediately-prior ledger's hash.
	const chainQ = `
		SELECT toUInt64(ifNull((SELECT min(ledger_seq) FROM (
			SELECT ledger_seq, prev_hash,
			       lagInFrame(ledger_hash) OVER (ORDER BY ledger_seq) AS prior_hash
			FROM (
				SELECT ledger_seq, argMax(ledger_hash, ingested_at) AS ledger_hash, argMax(prev_hash, ingested_at) AS prev_hash
				FROM stellar.ledgers WHERE ledger_seq BETWEEN ? AND ?
				GROUP BY ledger_seq
			)
		) WHERE ledger_seq > ? AND prior_hash != '' AND prev_hash != prior_hash), 0))`

	// Endpoint-presence guard (F1 fail-open fix): the windowed gap/chain scan
	// below only finds holes BETWEEN present ledgers, so an EMPTY range, or one
	// missing its head/tail ledgers, would otherwise read as "intact" — which
	// falsely certifies lake_complete during a partial restore/backfill. Assert
	// the endpoints are present + count the range up front so an absent
	// substrate fails CLOSED. (Interior gaps still fall through to the windowed
	// scan, which pinpoints their location; the tail is re-checked after it.)
	var haveMin, haveMax, present uint64
	const endpointsQ = `SELECT toUInt64(ifNull(min(ledger_seq),0)), toUInt64(ifNull(max(ledger_seq),0)), toUInt64(uniqExact(ledger_seq)) FROM stellar.ledgers WHERE ledger_seq BETWEEN ? AND ?`
	if qerr := conn.QueryRow(ctx, endpointsQ, from, to).Scan(&haveMin, &haveMax, &present); qerr != nil {
		return 0, false, "", fmt.Errorf("clickhouse: substrate endpoint presence [%d,%d]: %w", from, to, qerr)
	}
	if p, has, d := substrateHeadProblem(from, to, present > 0, uint32(haveMin)); has {
		return p, true, d, nil
	}

	for wlo := uint64(from); wlo <= uint64(to); wlo += substrateWindow {
		whi := wlo + substrateWindow
		if whi > uint64(to) {
			whi = uint64(to)
		}
		// Window starts one ledger BEFORE the span it certifies (except the
		// very first), so the seam pair (wlo-1, wlo) is hash-checked and a
		// gap at the seam is caught by contiguity over [wlo-1, whi].
		qlo := wlo
		if qlo > uint64(from) {
			qlo--
		}

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
	// Tail-presence guard (F1): the interior scan is clean, but if the last
	// present ledger is below `to`, the tail of the range is missing — every
	// source's data extends to `to`, so return `to` (not haveMax+1) so the
	// per-source consumer's `problem < genesis` test fails EVERY source, not
	// just those below haveMax+1 (the F1 consumer fail-open a high-genesis
	// source would otherwise slip through — see substrateHeadProblem).
	if haveMax < uint64(to) {
		return to, true, fmt.Sprintf("substrate: missing tail ledger(s) — last present is %d, expected %d", haveMax, to), nil
	}
	return 0, false, "", nil
}

// substrateHeadProblem is the pure low-ledger coverage decision for
// SubstrateProblem: an EMPTY range, or one missing its HEAD ledger(s) — the two
// absences the between-present-ledgers gap scan cannot see. It returns a problem
// ledger chosen so the per-source consumer's `problem < genesis ⟹ source-OK`
// test stays correct for a COVERAGE failure (the F1 consumer fail-open): an
// empty range returns `to` (the range tip) so EVERY source with genesis ≤ tip
// fails — not just SDEX at genesis 2 — and a missing head returns haveMin-1 so
// exactly the sources whose data begins inside the absent head fail, while a
// high-genesis source whose data is fully present still (correctly) passes. Pure
// — unit-testable without a live lake. hasProblem=false ⟹ the head is covered
// (interior gaps and the tail are decided elsewhere).
func substrateHeadProblem(from, to uint32, present bool, haveMin uint32) (problem uint32, hasProblem bool, detail string) {
	if !present {
		return to, true, fmt.Sprintf("substrate: no ledgers present in [%d,%d] (empty range — not intact)", from, to)
	}
	if haveMin > from {
		return haveMin - 1, true, fmt.Sprintf("substrate: missing head ledger(s) — first present is %d, expected %d (fails every source with genesis ≤ %d)", haveMin, from, haveMin-1)
	}
	return 0, false, ""
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
