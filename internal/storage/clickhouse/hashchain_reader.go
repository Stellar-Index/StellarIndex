// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package clickhouse

import (
	"context"
	"fmt"
)

// HashChainWindowResult is one window's ADR-0034 hash-chain in-window
// result: within [From,To], how many present ledgers have a prev_hash that
// does NOT equal the immediately-preceding PRESENT ledger's ledger_hash
// (per lagInFrame(ledger_hash) ordered by ledger_seq), out of how many
// present ledgers were actually eligible to be checked. Backs
// stellarindex-ops verify-hashchain's in-window check — the sibling of
// verify-contiguity's LedgerWindowCoverage for the "hash-chained" half of
// ADR-0034's provable-100% claim.
type HashChainWindowResult struct {
	From, To uint32
	Present  uint64 // present ledgers in [From,To] (post-FINAL dedup)
	Broken   uint64 // of those, how many have prev_hash != want_prev
}

// Checked is the count of in-window links QueryHashChainWindowLinks
// actually evaluated: Present-1. The window's first present ledger has no
// in-window predecessor to compare against (lagInFrame yields "" for it,
// which the query's WHERE want_prev != "" filters out) — its link lives at
// the PREVIOUS window's seam and is checked separately by
// QueryHashChainBoundary. SATURATES at 0 rather than underflowing when
// Present==0 — an entirely-missing window (a contiguity gap spanning the
// whole bucket; verify-contiguity is the tool that reports that precisely).
// Mirrors ECWindowCoverage.Missing()'s saturating-subtraction discipline
// (see that method's doc comment): never let a displayed count wrap.
func (w HashChainWindowResult) Checked() uint64 {
	if w.Present == 0 {
		return 0
	}
	return w.Present - 1
}

// hashChainWindowLinksQuery is the in-window headline: a single pass over
// [from,to] that computes both Present (count()) and Broken
// (countIf(...)) in one query, so the headline pass costs exactly one
// query per window regardless of how many links turn out to be broken.
//
// FINAL is required here — unlike QueryLedgerRangeCoverage's uniqExact(),
// which collapses duplicate ledger_seq rows from stellar.ledgers'
// unmerged ReplacingMergeTree(ingested_at) parts for free, this query reads
// ledger_hash/prev_hash VALUES through a window function. An unmerged
// duplicate row for the same ledger_seq would introduce a tie in the
// ORDER BY ledger_seq the window function relies on, corrupting
// lagInFrame's notion of "the immediately-preceding ledger" — the exact
// class of bug FINAL exists to prevent (see tier1_schema.sql's "Query with
// FINAL / GROUP BY for read-time dedup until merges settle" and this
// package's gate.go / explorer_reader.go, which FINAL for the same reason
// whenever they read stellar.ledgers' column VALUES rather than just its
// key set). boundedScanSettings caps per-query memory the same way every
// other full-history-capable reader in this package does (event_reader.go).
const hashChainWindowLinksQuery = `
	SELECT count() AS present, countIf(want_prev != '' AND prev_hash != want_prev) AS broken
	FROM (
		SELECT ledger_seq, prev_hash,
		       lagInFrame(ledger_hash) OVER (ORDER BY ledger_seq) AS want_prev
		FROM stellar.ledgers FINAL
		WHERE ledger_seq BETWEEN ? AND ?
	)
	` + boundedScanSettings

// QueryHashChainWindowLinks runs the ADR-0034 hash-chain in-window headline
// over [from,to], one query per stride-wide window (see
// forEachLedgerWindow) so peak query cost never exceeds one lake partition
// regardless of the overall range's size — same windowing discipline as
// QueryLedgerWindowCoverage. Unlike Check 1's headline (a single unwindowed
// uniqExact() that only windows AFTER finding a deficit), this is windowed
// unconditionally: the hash-chain check needs window-sized buckets anyway
// so the caller can run QueryHashChainBoundary at each seam, so there is no
// cheaper unwindowed alternative to fall back to first. Still cheap per
// call — one COUNT-shaped query per window, no row-level detail — so a
// healthy chain's steady-state cron run pays for windowing but not for
// per-ledger localization (that's QueryBrokenHashLinks, called only for
// windows this function already flagged Broken>0).
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

// HashChainBoundaryResult is one window-seam's hash-chain result: does
// ledger Seq's prev_hash equal ledger PredecessorSeq's (Seq-1's)
// ledger_hash? Windowing splits the chain at every stride-wide bucket edge
// — QueryHashChainWindowLinks' lagInFrame never sees across a WHERE
// ledger_seq BETWEEN boundary — so the seam link needs this separate 2-row
// point lookup.
//
// SeqPresent / PredecessorPresent let the caller distinguish "a genuine
// substrate gap at the seam" (one or both ledgers absent — verify-contiguity
// is the tool that reports gaps precisely; this tool still counts it as a
// broken link per ADR-0034 CLAUDE.md guidance: a missing predecessor IS a
// real chain break) from "both present but the hash doesn't match" (a true
// corruption). Linked is true only when both are present AND the hashes
// agree.
type HashChainBoundaryResult struct {
	Seq, PredecessorSeq            uint32
	SeqPresent, PredecessorPresent bool
	Linked                         bool
}

// hashChainBoundaryQuery reads both endpoints of a seam in one round trip.
// FINAL for the same value-correctness reason as hashChainWindowLinksQuery
// — a 2-row point lookup is cheap enough that the extra merge-on-read cost
// is immaterial, and correctness (not reading a stale unmerged duplicate)
// matters more here than anywhere else in this file, since a boundary
// result is never re-checked by the in-window pass.
const hashChainBoundaryQuery = `SELECT ledger_seq, ledger_hash, prev_hash FROM stellar.ledgers FINAL WHERE ledger_seq IN (?, ?)`

// QueryHashChainBoundary is the boundary point lookup for a single window
// seam: IN (seq-1, seq) reads at most two rows regardless of lake size, so
// running one per window (a few dozen even across full mainnet history at
// the 1M stride) is cheap enough to run unconditionally as part of the
// headline pass, unlike QueryBrokenHashLinks' per-ledger localization.
//
// seq==0 returns a zero-value result immediately without querying — no
// predecessor exists below ledger 0, and callers never legitimately pass
// seq==0 in practice (ADR-0034's genesis floor is ledger 2), but this
// avoids a PredecessorSeq=seq-1 uint32 underflow rather than relying on
// callers to never ask.
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

// BrokenHashLink is one individual broken in-window link, as located by
// QueryBrokenHashLinks.
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

// QueryBrokenHashLinks lists every individual broken in-window link within
// [from,to]. Callers MUST bound [from,to] to a single window
// (QueryHashChainWindowLinks already flagged Broken>0 for it) — mirrors
// QueryMissingLedgerSeqs' calling convention, so cost is bounded by the
// window's width rather than by how many breaks it contains.
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
