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

// verifyHashChain checks prev_hash == ledger n-1's ledger_hash for every n in [-from,-to] (ADR-0034): in-window (lagInFrame) plus a lookup
// at each bucket seam. A gap reads as a break; run verify-contiguity first. Exit code is the broken-link count capped at 255; read-only.
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

	toSeq, err := resolveToSeq(ctx, "verify-hashchain", *cfgPath, addr, *to)
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

// hashChainLinkCap bounds the broken in-window links printed per run, so a badly broken chain
// cannot become an unbounded print loop.
const hashChainLinkCap = 200

// runHashChainCheck runs both checks over [from,to] in contiguityBucketStride windows; per-ledger localization runs only for windows
// already flagged broken.
func runHashChainCheck(ctx context.Context, addr string, from, to uint32) (inWindowBroken, boundaryBroken uint64, err error) {
	fmt.Printf("\n=== verify-hashchain: hash-chain check [%d,%d] (1M-ledger windows) ===\n", from, to)

	windows, err := clickhouse.QueryHashChainWindowLinks(ctx, addr, from, to, contiguityBucketStride)
	if err != nil {
		return 0, 0, fmt.Errorf("verify-hashchain: windowed in-window-link scan: %w", err)
	}

	// Fail closed: a range with no present ledgers has 0 breaks and would otherwise PASS
	// exactly where the substrate is absent.
	if hashChainWindowsPresent(windows) == 0 {
		return 0, 0, fmt.Errorf("verify-hashchain: no ledgers present in [%d,%d] — nothing to hash-chain; refusing to PASS (fail-closed)", from, to)
	}

	inWindowBroken, err = reportInWindowBrokenLinks(ctx, addr, windows)
	if err != nil {
		return 0, 0, err
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

// reportInWindowBrokenLinks tallies the in-window broken links, printing each flagged window's
// count and per-ledger detail for the first hashChainLinkCap links overall.
func reportInWindowBrokenLinks(ctx context.Context, addr string, windows []clickhouse.HashChainWindowResult) (uint64, error) {
	var inWindowBroken uint64
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
			return 0, fmt.Errorf("verify-hashchain: broken-link localization [%d,%d]: %w", w.From, w.To, lerr)
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
	return inWindowBroken, nil
}

// hashChainWindowsPresent sums the present-ledger count across the windows. Zero means nothing
// to chain, so "0 broken links" is vacuous and must not read as a PASS.
func hashChainWindowsPresent(windows []clickhouse.HashChainWindowResult) uint64 {
	var total uint64
	for _, w := range windows {
		total += w.Present
	}
	return total
}

// boundarySeqsToCheck returns every window's From except the first, whose predecessor is out
// of scope: the requested range's own edge is never a deficiency.
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

// boundaryTag renders a boundary verdict, separating an absent ledger (a substrate gap for
// verify-contiguity) from present ledgers whose hashes differ (a true chain break).
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

// saturatingSub returns a-b floored at 0: the headline and detail queries run moments apart, so concurrent ingest could make the detail
// smaller and a raw uint64 subtraction would wrap to ~1.8e19.
func saturatingSub(a, b uint64) uint64 {
	if b >= a {
		return 0
	}
	return a - b
}
