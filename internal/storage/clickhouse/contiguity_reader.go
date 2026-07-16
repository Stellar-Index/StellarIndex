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
// stellar.ledgers are tx-bearing (tx_count > 0) within [From,To], versus how
// many of those ledger_seqs stellar.ledger_entry_changes holds at least one
// row for. Used for BOTH the below-ec-floor (backfill-pending) and
// at/above-ec-floor (live-covered, hard-gated) scans — verify-contiguity
// scopes [From,To] to one side of -ec-floor before calling
// QueryECWindowCoverage, so a single window is never ambiguous about which
// side of the floor it's on (see chops.ecFloorSegments).
type ECWindowCoverage struct {
	From, To             uint32
	TxLedgers, ECCovered uint64
}

// Missing is the count of tx-bearing ledgers in [From,To] with zero
// stellar.ledger_entry_changes rows — a SATURATING TxLedgers-ECCovered.
//
// ECCovered counts DISTINCT ledger_seq in ledger_entry_changes regardless of
// tx_count, so it can legitimately EXCEED TxLedgers: a protocol-upgrade
// ledger (or, in early history, a config/base-reserve change) mutates
// LedgerEntry state with tx_count==0, landing in entry_changes but not in the
// tx-bearing count. Treating that as "meets-or-exceeds coverage → no
// deficiency" is correct for this check's purpose; a raw uint64 subtraction
// would instead wrap to ~1.8e19 and catastrophically false-fail the whole
// run off a single such ledger. This makes Missing() a lower bound on the
// true per-ledger gap (the exact gap needs an anti-join), which is the right
// trade for a coarse coverage signal whose backfills fill whole ranges.
func (w ECWindowCoverage) Missing() uint64 {
	if w.ECCovered >= w.TxLedgers {
		return 0
	}
	return w.TxLedgers - w.ECCovered
}

// QueryECWindowCoverage runs the Check-2 coverage scan over [from,to], one
// pair of queries per stride-wide window (see forEachLedgerWindow): a
// tx-bearing-ledger count from stellar.ledgers and a distinct-ledger-covered
// count from stellar.ledger_entry_changes. Adapted from the ad hoc query run
// by hand to first find the ledger 63,050,000 live-ingest floor (see
// CLAUDE.md); windowing bounds per-query cost to one lake partition
// regardless of the overall range's size.
func QueryECWindowCoverage(ctx context.Context, addr string, from, to, stride uint32) ([]ECWindowCoverage, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	const txQ = `SELECT count() FROM stellar.ledgers WHERE ledger_seq BETWEEN ? AND ? AND tx_count > 0`
	const ecQ = `SELECT uniqExact(ledger_seq) FROM stellar.ledger_entry_changes WHERE ledger_seq BETWEEN ? AND ?`

	var out []ECWindowCoverage
	err = forEachLedgerWindow(from, to, stride, func(lo, hi uint32) error {
		var txLedgers uint64
		if qerr := conn.QueryRow(ctx, txQ, lo, hi).Scan(&txLedgers); qerr != nil {
			return fmt.Errorf("clickhouse: query tx-bearing ledgers [%d,%d]: %w", lo, hi, qerr)
		}
		var ecCovered uint64
		if qerr := conn.QueryRow(ctx, ecQ, lo, hi).Scan(&ecCovered); qerr != nil {
			return fmt.Errorf("clickhouse: query entry-change coverage [%d,%d]: %w", lo, hi, qerr)
		}
		out = append(out, ECWindowCoverage{From: lo, To: hi, TxLedgers: txLedgers, ECCovered: ecCovered})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
