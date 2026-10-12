// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"fmt"
)

// HashChainWindowResult is one window's in-window hash-chain result: how many present ledgers in [From,To] have a prev_hash differing from the preceding PRESENT ledger's ledger_hash (ADR-0034).
type HashChainWindowResult struct {
	From, To uint32
	Present  uint64 // present ledgers in [From,To] (post-FINAL dedup)
	Broken   uint64 // of those, how many have prev_hash != want_prev
}

// Checked is the count of in-window links evaluated, Present-1 (the first ledger's link is checked at the seam by QueryHashChainBoundary), saturating at 0 for an empty window.
func (w HashChainWindowResult) Checked() uint64 {
	if w.Present == 0 {
		return 0
	}
	return w.Present - 1
}

// hashChainWindowLinksQuery computes Present and Broken in one pass over [from,to], so cost is one
// query per window however many links are broken.
//
// FINAL is required: this reads ledger_hash/prev_hash VALUES through a window function, and an
// unmerged duplicate row for a ledger_seq would tie the ORDER BY lagInFrame relies on.
// boundedScanSettings caps memory.
const hashChainWindowLinksQuery = `
	SELECT count() AS present, countIf(want_prev != '' AND prev_hash != want_prev) AS broken
	FROM (
		SELECT ledger_seq, prev_hash,
		       lagInFrame(ledger_hash) OVER (ORDER BY ledger_seq) AS want_prev
		FROM stellar.ledgers FINAL
		WHERE ledger_seq BETWEEN ? AND ?
	)
	` + boundedScanSettings

// QueryHashChainWindowLinks runs the in-window headline over [from,to], one query per stride-wide window (bounded to one lake partition, and needed to run QueryHashChainBoundary at each seam).
func QueryHashChainWindowLinks(ctx context.Context, addr string, from, to, stride uint32) ([]HashChainWindowResult, error) {
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	var out []HashChainWindowResult
	err = forEachLedgerWindow(from, to, stride, func(lo, hi uint32) error {
		var present, broken uint64
		if qerr := conn.QueryRow(ctx, hashChainWindowLinksQuery, lo, hi).Scan(&present, &broken); qerr != nil {
			return fmt.Errorf("clickhouse: query hash chain window links [%d,%d]: %w", lo, hi, qerr)
		}
		out = append(out, HashChainWindowResult{From: lo, To: hi, Present: present, Broken: broken})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// HashChainBoundaryResult is one window seam's result (does Seq's prev_hash equal Seq-1's ledger_hash); SeqPresent/PredecessorPresent separate a substrate gap (counted as broken) from a true hash mismatch, and Linked needs both present and equal.
type HashChainBoundaryResult struct {
	Seq, PredecessorSeq            uint32
	SeqPresent, PredecessorPresent bool
	Linked                         bool
}

// hashChainBoundaryQuery reads both endpoints of a seam in one round trip. FINAL for the same
// reason as hashChainWindowLinksQuery; on a 2-row lookup the merge cost is immaterial, and no
// later pass re-checks a boundary result.
const hashChainBoundaryQuery = `SELECT ledger_seq, ledger_hash, prev_hash FROM stellar.ledgers FINAL WHERE ledger_seq IN (?, ?)`

// QueryHashChainBoundary is the point lookup for one window seam, reading at most two rows; seq==0 returns a zero-value result without querying (no predecessor, avoids uint32 underflow).
func QueryHashChainBoundary(ctx context.Context, addr string, seq uint32) (HashChainBoundaryResult, error) {
	res := HashChainBoundaryResult{Seq: seq}
	if seq == 0 {
		return res, nil
	}
	res.PredecessorSeq = seq - 1

	conn, err := openRead(ctx, addr)
	if err != nil {
		return HashChainBoundaryResult{}, err
	}
	defer func() { _ = conn.Close() }()

	rows, err := conn.Query(ctx, hashChainBoundaryQuery, seq-1, seq)
	if err != nil {
		return HashChainBoundaryResult{}, fmt.Errorf("clickhouse: query hash chain boundary [%d,%d]: %w", seq-1, seq, err)
	}
	defer func() { _ = rows.Close() }()

	var predLedgerHash, seqPrevHash string
	for rows.Next() {
		var ledgerSeq uint32
		var ledgerHash, prevHash string
		if serr := rows.Scan(&ledgerSeq, &ledgerHash, &prevHash); serr != nil {
			return HashChainBoundaryResult{}, fmt.Errorf("clickhouse: scan hash chain boundary row: %w", serr)
		}
		switch ledgerSeq {
		case seq - 1:
			res.PredecessorPresent = true
			predLedgerHash = ledgerHash
		case seq:
			res.SeqPresent = true
			seqPrevHash = prevHash
		}
	}
	if err := rows.Err(); err != nil {
		return HashChainBoundaryResult{}, fmt.Errorf("clickhouse: hash chain boundary rows [%d,%d]: %w", seq-1, seq, err)
	}

	res.Linked = res.SeqPresent && res.PredecessorPresent && seqPrevHash == predLedgerHash
	return res, nil
}

// BrokenHashLink is one individual broken in-window link, as located by QueryBrokenHashLinks.
type BrokenHashLink struct {
	LedgerSeq uint32
	PrevHash  string
	WantPrev  string
}

// brokenHashLinksQuery mirrors hashChainWindowLinksQuery's inner shape but
// returns individual rows instead of a count — the finer localization pass
// verify-hashchain pays for only after QueryHashChainWindowLinks already
// flagged a window's Broken count as nonzero.
const brokenHashLinksQuery = `
	SELECT ledger_seq, prev_hash, want_prev
	FROM (
		SELECT ledger_seq, prev_hash,
		       lagInFrame(ledger_hash) OVER (ORDER BY ledger_seq) AS want_prev
		FROM stellar.ledgers FINAL
		WHERE ledger_seq BETWEEN ? AND ?
	)
	WHERE want_prev != '' AND prev_hash != want_prev
	ORDER BY ledger_seq
	` + boundedScanSettings

// QueryBrokenHashLinks lists every broken in-window link in [from,to]; callers MUST bound the range to one window already flagged Broken>0.
func QueryBrokenHashLinks(ctx context.Context, addr string, from, to uint32) ([]BrokenHashLink, error) {
	if to < from {
		return nil, nil
	}
	conn, err := openRead(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = conn.Close() }()

	rows, err := conn.Query(ctx, brokenHashLinksQuery, from, to)
	if err != nil {
		return nil, fmt.Errorf("clickhouse: query broken hash links [%d,%d]: %w", from, to, err)
	}
	defer func() { _ = rows.Close() }()

	var out []BrokenHashLink
	for rows.Next() {
		var l BrokenHashLink
		if serr := rows.Scan(&l.LedgerSeq, &l.PrevHash, &l.WantPrev); serr != nil {
			return nil, fmt.Errorf("clickhouse: scan broken hash link: %w", serr)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("clickhouse: broken hash links rows [%d,%d]: %w", from, to, err)
	}
	return out, nil
}
