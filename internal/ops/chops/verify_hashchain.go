// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// verifyHashChain is the stellarindex-ops `verify-hashchain` subcommand —
// the second half of ADR-0034's provable-100% claim, alongside
// verify-contiguity: "ledgers contiguous AND hash-chained to genesis". For
// every ledger n in [-from,-to], its prev_hash must equal ledger n-1's
// ledger_hash.
//
// Two checks, because windowing (1M-ledger buckets — reuses
// contiguityBucketStride, matching verify-contiguity and the lake's own
// PARTITION BY intDiv(ledger_seq,1000000)) splits the chain at bucket
// boundaries:
//
//  1. In-window links: within each window, lagInFrame(ledger_hash) ordered
//     by ledger_seq gives every present ledger's immediate PRESENT
//     predecessor's hash; a mismatch against its own prev_hash is a broken
//     link. The window's first present ledger is excluded — its
//     predecessor lives in the PREVIOUS window and is checked by (2).
//  2. Boundary links: a 2-row point lookup at every window seam, comparing
//     the seam ledger's prev_hash against the previous ledger's
//     ledger_hash — whether or not that predecessor is actually present
//     (an absent predecessor is reported as a break too, distinguished
//     from a present-but-mismatched one; see boundaryTag).
//
// A missing ledger (a gap) is reported as a break by BOTH checks, by
// design: lagInFrame (in-window) and the boundary lookup (at a seam) both
// compare against whatever the nearest PRESENT predecessor's hash is,
// which is never the true immediate predecessor's hash when one is
// missing. Run verify-contiguity FIRST to know whether a break reported
// here is "missing ledger" or "present but wrong hash" — this tool does
// not try to auto-correlate the two; it just reports ledger_seq +
// presence, with enough context for an operator to tell them apart.
//
// Exit code = in-window broken links + boundary broken links, capped at
// 255, mirroring verify-contiguity's / reconcile-balances' "exit code =
// number of failed checks" convention so cron/Healthchecks.io can consume
// it directly.
//
// Usage: verify-hashchain [-config PATH] [-ch-addr H:P] [-from N] [-to N].
// Read-only; touches ClickHouse only (no Postgres).
func verifyHashChain(args []string) error {
	fs := flag.NewFlagSet("verify-hashchain", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/stellarindex.toml", "path to stellarindex.toml — used only to resolve the default -ch-addr (this tool reads ClickHouse only, never Postgres); a missing/unreadable file is tolerated when -ch-addr is passed explicitly")
	chAddr := fs.String("ch-addr", "", "ClickHouse native address (default: cfg.storage.clickhouse_addr from -config, falling back to "+defaultCHAddr+" if -config can't be loaded)")
	from := fs.Uint64("from", 2, "first ledger sequence to verify (inclusive); 2 is genesis")
	to := fs.Uint64("to", 0, "last ledger sequence to verify (inclusive); 0 = auto (max ledger_seq in stellar.ledgers)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	addr := resolveCHAddr(*chAddr, *cfgPath)

	fromSeq, err := toLedgerSeq("-from", *from)
	if err != nil {
		return err
	}

	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	toSeq, err := resolveToSeq(ctx, addr, *to)
	if err != nil {
		return err
	}
	if toSeq < fromSeq {
		return fmt.Errorf("verify-hashchain: resolved range [%d,%d] is empty (-to < -from)", fromSeq, toSeq)
	}

	fmt.Fprintf(os.Stderr, "verify-hashchain: range=[%d,%d] ch-addr=%s\n", fromSeq, toSeq, addr)

	inWindowBroken, boundaryBroken, err := runHashChainCheck(ctx, addr, fromSeq, toSeq)
	if err != nil {
		return err
	}

	total := inWindowBroken + boundaryBroken
	fmt.Printf("\nverify-hashchain: summary in_window_broken=%d boundary_broken=%d\n", inWindowBroken, boundaryBroken)

	if total == 0 {
		fmt.Println("verify-hashchain: PASSED")
		return nil
	}
	code := total
	if code > 255 {
		fmt.Fprintf(os.Stderr, "verify-hashchain: %d exceeds the max process exit code (255) — reporting 255\n", total)
		code = 255
	}
	return &opsutil.ExitCodeError{Code: int(code)} //nolint:gosec // capped above; always in [1,255].
}

// hashChainLinkCap bounds how many individual broken in-window links
// runHashChainCheck's localization pass will print across the whole run —
// mirrors contiguityGapRangeCap (verify_contiguity.go): a chain with
// millions of broken links must never turn this report into an unbounded
// print loop.
const hashChainLinkCap = 200

// runHashChainCheck runs both hash-chain checks over [from,to] using
// contiguityBucketStride windows (reused from verify_contiguity.go — same
// 1M stride, same lake-partition-alignment rationale). The windowed
// headline (QueryHashChainWindowLinks' in-window counts, plus one
// QueryHashChainBoundary point lookup per seam) is cheap and always runs;
// the per-ledger localization listing (QueryBrokenHashLinks) only runs for
// windows the headline already flagged Broken>0 — the same
// pay-only-when-a-problem-exists discipline as verify-contiguity's Check 1.
func runHashChainCheck(ctx context.Context, addr string, from, to uint32) (inWindowBroken, boundaryBroken uint64, err error) {
	fmt.Printf("\n=== verify-hashchain: hash-chain check [%d,%d] (1M-ledger windows) ===\n", from, to)

	windows, err := clickhouse.QueryHashChainWindowLinks(ctx, addr, from, to, contiguityBucketStride)
	if err != nil {
		return 0, 0, fmt.Errorf("verify-hashchain: windowed in-window-link scan: %w", err)
	}

	// Fail-closed guard (F3): a range with NO present ledgers has 0 links and 0
	// breaks, which would otherwise report PASSED — the chain "passes" precisely
	// where the substrate is absent. Refuse to pass when nothing was present to
	// chain. (verify-contiguity localizes WHERE the substrate is missing.)
	var totalPresent uint64
	for _, w := range windows {
		totalPresent += w.Present
	}
	if totalPresent == 0 {
		return 0, 0, fmt.Errorf("verify-hashchain: no ledgers present in [%d,%d] — nothing to hash-chain; refusing to PASS (fail-closed)", from, to)
	}

	var shown int
	var truncated bool
	for _, w := range windows {
		if w.Broken == 0 {
			continue
		}
		inWindowBroken += w.Broken
		fmt.Printf("  [%d,%d]  checked=%d broken=%d\n", w.From, w.To, w.Checked(), w.Broken)

		remaining := hashChainLinkCap - shown
		if remaining <= 0 {
			truncated = true
			continue
		}
		links, lerr := clickhouse.QueryBrokenHashLinks(ctx, addr, w.From, w.To)
		if lerr != nil {
			return 0, 0, fmt.Errorf("verify-hashchain: broken-link localization [%d,%d]: %w", w.From, w.To, lerr)
		}
		toPrint := links
		if len(toPrint) > remaining {
			toPrint = toPrint[:remaining]
			truncated = true
		}
		for _, l := range toPrint {
			fmt.Printf("    broken ledger_seq=%d prev_hash=%s want_prev=%s\n",
				l.LedgerSeq, opsutil.Truncate(l.PrevHash, 16), opsutil.Truncate(l.WantPrev, 16))
		}
		shown += len(toPrint)
		if notShown := saturatingSub(uint64(len(links)), uint64(len(toPrint))); notShown > 0 {
			fmt.Printf("    ...%d more broken link(s) in this window not shown\n", notShown)
			truncated = true
		}
	}
	if truncated {
		fmt.Printf("verify-hashchain: broken-link list truncated at %d entries — more breaks exist; narrow -from/-to for full detail\n",
			hashChainLinkCap)
	}

	fmt.Println("  --- boundary links (window seams) ---")
	for _, seq := range boundarySeqsToCheck(windows, from) {
		b, berr := clickhouse.QueryHashChainBoundary(ctx, addr, seq)
		if berr != nil {
			return 0, 0, fmt.Errorf("verify-hashchain: boundary check at seq=%d: %w", seq, berr)
		}
		if !b.Linked {
			boundaryBroken++
		}
		fmt.Printf("  boundary seq=%d predecessor=%d  %s\n", b.Seq, b.PredecessorSeq, boundaryTag(b))
	}

	fmt.Printf("hash-chain check: %d in-window broken link(s), %d boundary broken link(s) across [%d,%d]\n",
		inWindowBroken, boundaryBroken, from, to)
	return inWindowBroken, boundaryBroken, nil
}

// boundarySeqsToCheck returns the window-seam ledger_seq values
// runHashChainCheck must run QueryHashChainBoundary against: every window's
// From EXCEPT the first, whose From==from has no predecessor IN SCOPE —
// the caller only asked to verify starting at -from, so there is nothing
// to check before it (mirrors verify-contiguity's -ec-floor scoping: the
// requested range's own edge is never treated as a deficiency). Pure — no
// ClickHouse dependency — so it's unit-testable without a live lake.
func boundarySeqsToCheck(windows []clickhouse.HashChainWindowResult, from uint32) []uint32 {
	var out []uint32
	for _, w := range windows {
		if w.From == from {
			continue
		}
		out = append(out, w.From)
	}
	return out
}

// boundaryTag renders a HashChainBoundaryResult's verdict for the report —
// distinguishing "predecessor absent" / "seq absent" (a substrate gap;
// verify-contiguity is the tool that reports gaps precisely) from "both
// present but hashes differ" (a true chain break), rather than collapsing
// both into one generic "broken" tag. Pure — no ClickHouse dependency — so
// it's unit-testable without a live lake.
func boundaryTag(b clickhouse.HashChainBoundaryResult) string {
	switch {
	case b.Linked:
		return "OK"
	case !b.SeqPresent && !b.PredecessorPresent:
		return "BROKEN (both ledgers absent — a substrate gap; run verify-contiguity)"
	case !b.SeqPresent:
		return "BROKEN (ledger_seq absent — a substrate gap; run verify-contiguity)"
	case !b.PredecessorPresent:
		return "BROKEN (predecessor absent — a substrate gap; run verify-contiguity)"
	default:
		return "BROKEN (both present, hash mismatch)"
	}
}

// saturatingSub returns a-b, floored at 0 instead of wrapping. Used to
// compare the windowed headline's Broken count against the follow-up
// per-ledger localization query's row count: the two queries run moments
// apart, so a concurrent ingest/backfill landing between them could, in
// principle, make the detail query return fewer rows than the headline
// counted. A raw uint64 subtraction would wrap that transient skew to
// ~1.8e19 instead of reporting "0 more" — the exact bug class the
// verify-contiguity review caught in this package (never truncate/wrap a
// displayed count silently; see ECWindowCoverage.Missing()'s doc comment).
func saturatingSub(a, b uint64) uint64 {
	if b >= a {
		return 0
	}
	return a - b
}
