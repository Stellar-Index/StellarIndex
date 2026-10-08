// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"fmt"
)

// LedgerWindowCoverage is one range's Check-1 result: distinct ledger_seq values
// stellar.ledgers holds in [From,To] against Expected (ADR-0034: gap-free substrate).
type LedgerWindowCoverage struct {
	From, To          uint32
	Expected, Present uint64

	// Rows is count() over the range; un-merged ReplacingMergeTree re-ingests make
	// Rows > Present. Only QueryLedgerRangeCoverage fills it.
	Rows uint64
}

// Missing is Expected-Present: ledger_seq values in [From,To] with zero rows.
func (c LedgerWindowCoverage) Missing() uint64 {
	return c.Expected - c.Present
}

// DuplicateRows is Rows-Present, which uniqExact alone cannot see; 0 when Rows is unset.
func (c LedgerWindowCoverage) DuplicateRows() uint64 {
	if c.Rows <= c.Present {
		return 0
	}
	return c.Rows - c.Present
}

// QueryLedgerRangeCoverage is Check 1's headline: one uniqExact()+count() over the whole
// range. Deliberately unwindowed: uniqExact on a narrow UInt32 column is cheap, and a clean
// result lets the caller skip the bucket scan.
func QueryLedgerRangeCoverage(ctx context.Context, addr string, from, to uint32) (LedgerWindowCoverage, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return LedgerWindowCoverage{}, err
	}
	defer func() { _ = conn.Close() }()

	var present, rows uint64
	const q = `SELECT uniqExact(ledger_seq), count() FROM stellar.ledgers WHERE ledger_seq BETWEEN ? AND ?`
	if err := conn.QueryRow(ctx, q, from, to).Scan(&present, &rows); err != nil {
		return LedgerWindowCoverage{}, fmt.Errorf("clickhouse: query ledger range coverage [%d,%d]: %w", from, to, err)
	}
	return LedgerWindowCoverage{From: from, To: to, Expected: uint64(to-from) + 1, Present: present, Rows: rows}, nil
}

// QueryLedgerWindowCoverage localizes gaps with one uniqExact() per stride-wide window
// (forEachLedgerWindow), keeping peak cost within one lake partition. Run only after the
// headline check found a deficit.
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

// QueryMissingLedgerSeqs returns each ledger_seq absent from stellar.ledgers in [from,to].
// Callers MUST bound the range to one lake partition or less: it anti-joins
// numbers(from, to-from+1) against the present set, so cost scales with window width.
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

// ECWindowCoverage is one window's Check-2 result: tx-bearing ledgers in [From,To] and how
// many have a stellar.ledger_entry_changes row. Callers scope a window to one side of
// -ec-floor (chops.ecFloorSegments).
type ECWindowCoverage struct {
	From, To uint32

	// TxLedgers is the distinct tx-bearing (tx_count > 0) ledger_seq count
	// in [From,To].
	TxLedgers uint64

	// ECCoveredTxLedgers is the tx-bearing subset with entry-change rows (a per-ledger
	// semi-join, so always <= TxLedgers). See [ECWindowCoverage.Missing].
	ECCoveredTxLedgers uint64
}

// Missing is TxLedgers - ECCoveredTxLedgers, the exact count of uncovered tx-bearing ledgers.
// Never subtract an independent uniqExact over all entry changes: tx_count == 0 ledgers
// (protocol upgrades) carry entry changes and would net out real gaps one-for-one.
// The saturating guard only protects hand-built values from uint64 wrap.
func (w ECWindowCoverage) Missing() uint64 {
	if w.ECCoveredTxLedgers >= w.TxLedgers {
		return 0
	}
	return w.TxLedgers - w.ECCoveredTxLedgers
}

// ecTxScopedRow selects transaction-scoped ledger_entry_changes rows. Snapshot
// seeds (SnapshotEntryRow) and eviction rows carry an empty tx_hash; tx_hash is
// the second sort-key column, so the ledger_seq range still prunes granules.
const ecTxScopedRow = "tx_hash != ''"

// ecWindowCoverageQuery is the Check-2 per-window scan; a builder so a unit test pins the shape.
//
//   - uniqExact(ledger_seq), not count(): stellar.ledgers is ReplacingMergeTree, so count()
//     double-counts un-merged re-ingests.
//   - uniqExactIf(... IN (subquery)) restricts coverage to the same tx-bearing set as the
//     total; a standalone uniqExact would count tx_count == 0 ledgers as coverage.
//   - Both sides are primary-key range scans bounded by the stride window.
//   - The subquery admits only ecTxScopedRow rows: a snapshot or eviction row is not coverage.
//
// The four `?` bind positionally: (subquery lo, hi, outer lo, hi).
func ecWindowCoverageQuery() string {
	return `
		SELECT
		    uniqExact(ledger_seq),
		    uniqExactIf(ledger_seq, ledger_seq IN (
		        SELECT ledger_seq
		        FROM stellar.ledger_entry_changes
		        WHERE ledger_seq BETWEEN ? AND ? AND ` + ecTxScopedRow + `
		    ))
		FROM stellar.ledgers
		WHERE ledger_seq BETWEEN ? AND ? AND tx_count > 0`
}

// QueryECWindowCoverage runs the Check-2 scan over [from,to], one query per stride-wide
// window (forEachLedgerWindow) to bound cost to one lake partition.
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

// QueryECLowerEdge returns the lowest ledger in [from,to] with a transaction-scoped
// ledger_entry_changes row (non-empty tx_hash); found=false if none. Snapshot seed rows have
// empty tx_hash and span all history, so counting them would drag the edge to genesis.
// ORDER BY the sort-key prefix + LIMIT 1 reads in order and stops at the first match.
func QueryECLowerEdge(ctx context.Context, addr string, from, to uint32) (edge uint32, found bool, err error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = conn.Close() }()

	const q = `
		SELECT ledger_seq
		FROM stellar.ledger_entry_changes
		WHERE ledger_seq BETWEEN ? AND ? AND ` + ecTxScopedRow + `
		ORDER BY ledger_seq
		LIMIT 1`
	rows, err := conn.Query(ctx, q, from, to)
	if err != nil {
		return 0, false, fmt.Errorf("clickhouse: query entry-change lower edge [%d,%d]: %w", from, to, err)
	}
	defer func() { _ = rows.Close() }()
	if rows.Next() {
		if err := rows.Scan(&edge); err != nil {
			return 0, false, fmt.Errorf("clickhouse: scan entry-change lower edge: %w", err)
		}
		found = true
	}
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("clickhouse: entry-change lower edge rows [%d,%d]: %w", from, to, err)
	}
	return edge, found, nil
}
