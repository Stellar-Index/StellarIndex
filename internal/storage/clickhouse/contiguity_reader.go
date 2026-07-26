// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"fmt"
)

// LedgerWindowCoverage is one range's (or bucket's) Check-1 substrate
// contiguity result: how many DISTINCT ledger_seq values stellar.ledgers
// actually holds within [From,To], against the range's own size
// (Expected = To-From+1). Backs stellarindex-ops verify-contiguity's
// ledger-contiguity check (ADR-0034: the raw lake's ledger substrate must
// be gap-free).
type LedgerWindowCoverage struct {
	From, To          uint32
	Expected, Present uint64
}

// Missing is Expected-Present — the count of ledger_seq values in [From,To]
// that stellar.ledgers has zero rows for.
func (c LedgerWindowCoverage) Missing() uint64 {
	return c.Expected - c.Present
}

// QueryLedgerRangeCoverage is Check 1's headline: a single uniqExact() over
// the WHOLE [from,to] range. uniqExact on one narrow UInt32 column is cheap
// even across full history — unlike the wide argMax/multi-column reads that
// have driven CH memory ceilings elsewhere in this package (see gate.go,
// recognition.go's doc comments) — so this deliberately does NOT window,
// letting the caller skip the (more expensive) bucket-level scan entirely
// when the range is already fully contiguous.
func QueryLedgerRangeCoverage(ctx context.Context, addr string, from, to uint32) (LedgerWindowCoverage, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return LedgerWindowCoverage{}, err
	}
	defer func() { _ = conn.Close() }()

	var present uint64
	const q = `SELECT uniqExact(ledger_seq) FROM stellar.ledgers WHERE ledger_seq BETWEEN ? AND ?`
	if err := conn.QueryRow(ctx, q, from, to).Scan(&present); err != nil {
		return LedgerWindowCoverage{}, fmt.Errorf("clickhouse: query ledger range coverage [%d,%d]: %w", from, to, err)
	}
	return LedgerWindowCoverage{From: from, To: to, Expected: uint64(to-from) + 1, Present: present}, nil
}

// QueryLedgerWindowCoverage runs the Check-1 gap-localization scan over
// [from,to], one uniqExact() query per stride-wide window (see
// forEachLedgerWindow) so peak query cost never exceeds one lake partition
// regardless of the overall range's size. Only called by verify-contiguity
// after QueryLedgerRangeCoverage's headline already found a deficit — the
// per-window breakdown is what makes the report actionable (which buckets
// have gaps), not a substitute for the headline check.
func QueryLedgerWindowCoverage(ctx context.Context, addr string, from, to, stride uint32) ([]LedgerWindowCoverage, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	var out []LedgerWindowCoverage
	const q = `SELECT uniqExact(ledger_seq) FROM stellar.ledgers WHERE ledger_seq BETWEEN ? AND ?`
	err = forEachLedgerWindow(from, to, stride, func(lo, hi uint32) error {
		var present uint64
		if qerr := conn.QueryRow(ctx, q, lo, hi).Scan(&present); qerr != nil {
			return fmt.Errorf("clickhouse: query ledger window coverage [%d,%d]: %w", lo, hi, qerr)
		}
		out = append(out, LedgerWindowCoverage{From: lo, To: hi, Expected: uint64(hi-lo) + 1, Present: present})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// QueryMissingLedgerSeqs returns every individual ledger_seq absent from
// stellar.ledgers within [from,to]. Callers MUST bound [from,to] to a single
// lake partition or smaller (e.g. one QueryLedgerWindowCoverage window)
// before calling this — verify-contiguity's Check 1 orchestration only ever
// calls it on buckets QueryLedgerWindowCoverage already flagged with
// missing>0, never over an unbounded whole-history range. Implemented as
// numbers(from, to-from+1) (one candidate row per ledger_seq in range)
// anti-joined against the present set, so cost is bounded by the window's
// width, not by how sparse or dense the gaps within it are.
func QueryMissingLedgerSeqs(ctx context.Context, addr string, from, to uint32) ([]uint32, error) {
	if to < from {
		return nil, nil
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	const q = `
		SELECT number
		FROM numbers(?, ?)
		WHERE number NOT IN (
			SELECT ledger_seq FROM stellar.ledgers WHERE ledger_seq BETWEEN ? AND ?
		)
		ORDER BY number`
	rows, err := conn.Query(ctx, q, uint64(from), uint64(to-from+1), from, to)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: query missing ledger seqs [%d,%d]: %w", from, to, err)
	}
	defer func() { _ = rows.Close() }()

	var out []uint32
	for rows.Next() {
		var n uint64
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("clickhouse: scan missing ledger seq: %w", err)
		}
		out = append(out, uint32(n))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: missing ledger seqs rows [%d,%d]: %w", from, to, err)
	}
	return out, nil
}

// ECWindowCoverage is one window's Check-2 result: how many ledgers in
// stellar.ledgers are tx-bearing (tx_count > 0) within [From,To], and how
// many of THOSE ledger_seqs stellar.ledger_entry_changes holds at least one
// row for. Used for BOTH the below-ec-floor (backfill-pending) and
// at/above-ec-floor (live-covered, hard-gated) scans — verify-contiguity
// scopes [From,To] to one side of -ec-floor before calling
// QueryECWindowCoverage, so a single window is never ambiguous about which
// side of the floor it's on (see chops.ecFloorSegments).
type ECWindowCoverage struct {
	From, To uint32

	// TxLedgers is the distinct tx-bearing (tx_count > 0) ledger_seq count
	// in [From,To].
	TxLedgers uint64

	// ECCoveredTxLedgers is the subset of those TxLedgers that
	// stellar.ledger_entry_changes holds at least one row for — a per-ledger
	// semi-join, NOT a standalone cardinality of entry_changes. It is
	// therefore ≤ TxLedgers by construction. See [ECWindowCoverage.Missing].
	ECCoveredTxLedgers uint64
}

// Missing is the EXACT count of tx-bearing ledgers in [From,To] with zero
// stellar.ledger_entry_changes rows: TxLedgers - ECCoveredTxLedgers.
//
// C4-085 (audit-2026-07-23). This used to subtract two INDEPENDENT
// cardinalities — tx-bearing ledgers from stellar.ledgers against
// uniqExact(ledger_seq) over ALL of ledger_entry_changes in the window — and
// saturate at zero. Entry-change rows exist for ledgers that carry no
// transactions at all: a protocol-upgrade ledger (or, in early history, a
// config/base-reserve change) mutates LedgerEntry state with tx_count == 0,
// so it landed in the "present" side while never appearing in the "expected"
// side. Inside a 1,000,000-ledger window those ledgers padded `present` and
// NETTED OUT genuinely-uncovered tx-bearing ledgers one-for-one: a window
// holding 5 protocol-upgrade ledgers reported zero deficiency while 5
// tx-bearing ledgers had no entry-change coverage at all, and Check 2 —
// the hard gate above -ec-floor — passed on a real gap.
//
// The fix is the anti-join this comment used to name as the thing it was
// NOT doing: ECCoveredTxLedgers is now computed per-ledger against the
// tx-bearing set, so a tx_count == 0 ledger can never contribute coverage
// it does not have, and Missing() is the true gap rather than a lower bound.
//
// The saturating guard is retained as defence-in-depth only: the subset
// relation makes ECCoveredTxLedgers > TxLedgers unreachable through
// [QueryECWindowCoverage], but a hand-constructed value must still not wrap
// uint64 to ~1.8e19 and catastrophically false-fail a whole run.
func (w ECWindowCoverage) Missing() uint64 {
	if w.ECCoveredTxLedgers >= w.TxLedgers {
		return 0
	}
	return w.TxLedgers - w.ECCoveredTxLedgers
}

// ecWindowCoverageQuery is the Check-2 per-window scan: one query returning
// (tx-bearing ledgers, tx-bearing ledgers WITH entry-change coverage).
//
// Split out as a builder so the anti-join shape is pinned by a unit test
// without a live lake — the same discipline distinctShapesWindowQuery uses.
//
// Shape notes:
//
//   - uniqExact(ledger_seq), not count(): stellar.ledgers is
//     ReplacingMergeTree, so count() over an un-merged re-ingested ledger
//     double-counts a tx-bearing ledger and manufactures a false coverage
//     surplus (audit C2-12). uniqExact counts distinct ledgers — matching
//     the two uniqExact reads above.
//   - uniqExactIf(..., ledger_seq IN (SELECT … FROM ledger_entry_changes …))
//     is the anti-join's complement, evaluated over the SAME tx-bearing row
//     set as the total. Restricting coverage to that set is the whole fix
//     for C4-085; a standalone uniqExact over ledger_entry_changes counts
//     tx_count == 0 ledgers as coverage of ledgers that are not in the
//     expected set at all.
//   - Both sides are primary-key range scans bounded by the caller's stride
//     window, so the IN-set is one window wide — the cost class is unchanged
//     from the two-query form it replaces (in fact one fewer round trip).
//
// The four `?` placeholders bind positionally in text order:
// (subquery lo, subquery hi, outer lo, outer hi) — the same pair twice.
func ecWindowCoverageQuery() string {
	return `
		SELECT
		    uniqExact(ledger_seq),
		    uniqExactIf(ledger_seq, ledger_seq IN (
		        SELECT ledger_seq
		        FROM stellar.ledger_entry_changes
		        WHERE ledger_seq BETWEEN ? AND ?
		    ))
		FROM stellar.ledgers
		WHERE ledger_seq BETWEEN ? AND ? AND tx_count > 0`
}

// QueryECWindowCoverage runs the Check-2 coverage scan over [from,to], one
// query per stride-wide window (see forEachLedgerWindow): the tx-bearing
// ledger count from stellar.ledgers alongside how many of those same ledgers
// stellar.ledger_entry_changes covers. Adapted from the ad hoc query run by
// hand to first find the ledger 63,050,000 live-ingest floor (see CLAUDE.md);
// windowing bounds per-query cost to one lake partition regardless of the
// overall range's size.
func QueryECWindowCoverage(ctx context.Context, addr string, from, to, stride uint32) ([]ECWindowCoverage, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	q := ecWindowCoverageQuery()

	var out []ECWindowCoverage
	err = forEachLedgerWindow(from, to, stride, func(lo, hi uint32) error {
		var txLedgers, ecCovered uint64
		if qerr := conn.QueryRow(ctx, q, lo, hi, lo, hi).Scan(&txLedgers, &ecCovered); qerr != nil {
			return fmt.Errorf("clickhouse: query entry-change coverage [%d,%d]: %w", lo, hi, qerr)
		}
		out = append(out, ECWindowCoverage{
			From: lo, To: hi,
			TxLedgers:          txLedgers,
			ECCoveredTxLedgers: ecCovered,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
