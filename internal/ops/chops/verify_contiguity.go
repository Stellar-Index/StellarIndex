// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// verifyContiguity is the stellarindex-ops `verify-contiguity` subcommand —
// a standing ADR-0034 data-verification tool for the ClickHouse raw lake.
// Two checks:
//
//  1. Ledger substrate contiguity: every ledger_seq in [-from,-to] must be
//     present in stellar.ledgers exactly once (ADR-0034's "100% coverage"
//     claim is provable only if the substrate is gap-free).
//  2. stellar.ledger_entry_changes coverage vs. tx-bearing ledgers: every
//     ledger with tx_count>0 should have at least one entry_changes row,
//     ABOVE the live-ingest floor (-ec-floor, default 63,050,000 — coverage
//     below the floor is backfill-in-progress and expected to be partial;
//     see CLAUDE.md's "entry_changes coverage seam" note).
//
// Exit code = (ledger gaps) + (entry-change deficiencies at/above
// -ec-floor), capped at 255, mirroring reconcile-balances' and
// scripts/dev/r1-smoke.sh's "exit code = number of failed checks"
// convention so cron/Healthchecks.io can consume it directly. Backfill-
// pending entries below -ec-floor are reported but never counted toward
// the exit code — the whole point of the floor is to keep the exit code
// meaningful (fails only on real regressions in the live-covered zone).
//
// Usage: verify-contiguity [-config PATH] [-ch-addr H:P] [-from N] [-to N]
// [-ec-floor N] [-check ledgers|entrychanges|all]. Read-only; touches
// ClickHouse only (no Postgres).
func verifyContiguity(args []string) error {
	fs := flag.NewFlagSet("verify-contiguity", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/stellarindex.toml", "path to stellarindex.toml — used only to resolve the default -ch-addr (this tool reads ClickHouse only, never Postgres); a missing/unreadable file is tolerated when -ch-addr is passed explicitly")
	chAddr := fs.String("ch-addr", "", "ClickHouse native address (default: cfg.storage.clickhouse_addr from -config, falling back to "+defaultCHAddr+" if -config can't be loaded)")
	from := fs.Uint64("from", 2, "first ledger sequence to verify (inclusive); 2 is genesis")
	to := fs.Uint64("to", 0, "last ledger sequence to verify (inclusive); 0 = auto (max ledger_seq in stellar.ledgers)")
	ecFloor := fs.Uint64("ec-floor", defaultECFloor, "ledger at/above which missing stellar.ledger_entry_changes coverage is a hard failure (counts toward the exit code); below it, missing coverage is reported as backfill-pending, informational only")
	checkFlag := fs.String("check", "all", "which check(s) to run: ledgers | entrychanges | all")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *checkFlag != "ledgers" && *checkFlag != "entrychanges" && *checkFlag != "all" {
		return fmt.Errorf("verify-contiguity: -check must be one of ledgers|entrychanges|all, got %q", *checkFlag)
	}

	addr := resolveCHAddr(*chAddr, *cfgPath)

	fromSeq, err := toLedgerSeq("-from", *from)
	if err != nil {
		return err
	}
	ecFloorSeq, err := toLedgerSeq("-ec-floor", *ecFloor)
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
		return fmt.Errorf("verify-contiguity: resolved range [%d,%d] is empty (-to < -from)", fromSeq, toSeq)
	}

	fmt.Fprintf(os.Stderr, "verify-contiguity: range=[%d,%d] ch-addr=%s ec-floor=%d check=%s\n",
		fromSeq, toSeq, addr, ecFloorSeq, *checkFlag)

	var ledgerGaps, ecDeficiency, ecPending uint64

	if *checkFlag == "ledgers" || *checkFlag == "all" {
		ledgerGaps, err = runLedgerContiguityCheck(ctx, addr, fromSeq, toSeq)
		if err != nil {
			return err
		}
	}
	if *checkFlag == "entrychanges" || *checkFlag == "all" {
		ecDeficiency, ecPending, err = runEntryChangesCheck(ctx, addr, fromSeq, toSeq, ecFloorSeq)
		if err != nil {
			return err
		}
	}

	total := ledgerGaps + ecDeficiency
	fmt.Printf("\nverify-contiguity: summary check1_missing_ledgers=%d check2_deficiency=%d check2_backfill_pending=%d\n",
		ledgerGaps, ecDeficiency, ecPending)

	if total == 0 {
		fmt.Println("verify-contiguity: PASSED")
		return nil
	}
	code := total
	if code > 255 {
		fmt.Fprintf(os.Stderr, "verify-contiguity: %d exceeds the max process exit code (255) — reporting 255\n", total)
		code = 255
	}
	return &opsutil.ExitCodeError{Code: int(code)} //nolint:gosec // capped above; always in [1,255].
}

// defaultCHAddr / defaultECFloor are verify-contiguity's flag defaults.
// defaultECFloor is the known live-ingest floor documented in CLAUDE.md's
// "entry_changes coverage seam" note (2026-07-16): coverage is 100% from
// this ledger to tip and partial below it (backfill in progress).
const (
	defaultCHAddr  = "127.0.0.1:9300"
	defaultECFloor = 63_050_000
)

// resolveCHAddr implements "-ch-addr overrides; unset falls back to
// -config's clickhouse_addr; -config itself is best-effort" — mirrors
// internal/ops/ingest/state_snapshot.go's resolveArchiveTarget, which
// tolerates a missing/invalid -config the same way for a tool that doesn't
// strictly need the full config (this one touches ClickHouse only, never
// Postgres, so a config that fails Validate() on an unrelated section
// shouldn't block a ClickHouse-only read).
func resolveCHAddr(chAddrFlag, cfgPath string) string {
	if chAddrFlag != "" {
		return chAddrFlag
	}
	if cfg, err := config.LoadWithEnv(cfgPath); err == nil && cfg.Storage.ClickHouseAddr != "" {
		return cfg.Storage.ClickHouseAddr
	}
	return defaultCHAddr
}

// toLedgerSeq safely narrows a uint64 flag value to the uint32 range
// stellar.ledgers.ledger_seq actually uses, erroring on overflow instead of
// silently wrapping (money/precision discipline applied to ledger
// arithmetic, not just token amounts: never truncate silently).
func toLedgerSeq(flagName string, v uint64) (uint32, error) {
	if v > math.MaxUint32 {
		return 0, fmt.Errorf("verify-contiguity: %s=%d exceeds a ledger sequence's uint32 range", flagName, v)
	}
	return uint32(v), nil
}

// resolveToSeq implements "-to 0 means auto (CH max ledger)".
func resolveToSeq(ctx context.Context, addr string, to uint64) (uint32, error) {
	if to != 0 {
		return toLedgerSeq("-to", to)
	}
	hi, err := clickhouse.MaxLedger(ctx, addr)
	if err != nil {
		return 0, fmt.Errorf("verify-contiguity: resolve -to (CH max ledger): %w", err)
	}
	return hi, nil
}

// contiguityBucketStride is the windowed-scan bucket width both checks use
// once the headline query finds a deficit — 1M ledgers, matching the lake's
// own partitioning (PARTITION BY intDiv(ledger_seq, 1000000), see
// recognition.go's recognitionScanWindow) so each window query touches
// exactly one lake partition.
const contiguityBucketStride = 1_000_000

// contiguityGapRangeCap bounds how many individual gap ranges Check 1's
// finer pass will enumerate across the whole run — a lake with millions of
// missing ledgers (e.g. -from/-to spanning an unbackfilled region) must
// never turn this report into an unbounded print loop.
const contiguityGapRangeCap = 200

// runLedgerContiguityCheck is Check 1: the substrate contiguity check. It
// starts with the cheap whole-range headline (QueryLedgerRangeCoverage); if
// that finds no deficit, it stops there. Only when a deficit exists does it
// pay for the windowed bucket scan + the bounded per-bucket gap-range
// localization pass, so a healthy lake's steady-state cron run costs one
// query.
func runLedgerContiguityCheck(ctx context.Context, addr string, from, to uint32) (uint64, error) {
	fmt.Printf("\n=== verify-contiguity: check 1 — ledger substrate contiguity [%d,%d] ===\n", from, to)

	overall, err := clickhouse.QueryLedgerRangeCoverage(ctx, addr, from, to)
	if err != nil {
		return 0, fmt.Errorf("verify-contiguity: check 1: %w", err)
	}
	missingTotal := overall.Missing()
	fmt.Printf("expected=%d present=%d missing=%d\n", overall.Expected, overall.Present, missingTotal)
	if missingTotal == 0 {
		fmt.Println("check 1: OK — every ledger present exactly once")
		return 0, nil
	}

	fmt.Println("check 1: deficit found — locating gap ranges (windowed scan, 1M-ledger buckets)...")
	windows, err := clickhouse.QueryLedgerWindowCoverage(ctx, addr, from, to, contiguityBucketStride)
	if err != nil {
		return 0, fmt.Errorf("verify-contiguity: check 1 bucket scan: %w", err)
	}

	var rangesFound int
	var truncatedOverall bool
	for _, w := range windows {
		wm := w.Missing()
		if wm == 0 {
			continue
		}
		fmt.Println(formatCoverageLine(w.From, w.To, w.Expected, w.Present, "GAP"))

		remaining := contiguityGapRangeCap - rangesFound
		if remaining <= 0 {
			truncatedOverall = true
			continue
		}
		missingSeqs, merr := clickhouse.QueryMissingLedgerSeqs(ctx, addr, w.From, w.To)
		if merr != nil {
			return 0, fmt.Errorf("verify-contiguity: check 1 gap localization [%d,%d]: %w", w.From, w.To, merr)
		}
		ranges, truncated := groupMissingIntoRanges(missingSeqs, remaining)
		for _, r := range ranges {
			fmt.Printf("    gap [%d,%d] (%d ledger(s))\n", r.From, r.To, r.Count())
		}
		rangesFound += len(ranges)
		if truncated {
			truncatedOverall = true
		}
	}
	if truncatedOverall {
		fmt.Printf("verify-contiguity: check 1 gap-range list truncated at %d ranges — more gaps exist; narrow -from/-to for full detail\n",
			contiguityGapRangeCap)
	}

	fmt.Printf("check 1: %d ledger(s) missing across [%d,%d]\n", missingTotal, from, to)
	return missingTotal, nil
}

// gapRange is one contiguous run of missing ledger_seq values, as located by
// groupMissingIntoRanges.
type gapRange struct {
	From, To uint32
}

// Count is the number of missing ledgers this range covers.
func (g gapRange) Count() uint64 {
	return uint64(g.To-g.From) + 1
}

// groupMissingIntoRanges collapses a SORTED, ASCENDING slice of individual
// missing ledger_seq values (as returned by
// clickhouse.QueryMissingLedgerSeqs) into contiguous [From,To] ranges,
// capped at capRanges entries. truncated=true when more ranges existed
// beyond the cap, so the caller can print a "list truncated" note instead
// of silently under-reporting. Pure — no ClickHouse dependency — so it's
// unit-testable without a live lake.
func groupMissingIntoRanges(missing []uint32, capRanges int) (ranges []gapRange, truncated bool) {
	i := 0
	for i < len(missing) {
		if len(ranges) >= capRanges {
			return ranges, true
		}
		start := missing[i]
		end := start
		i++
		for i < len(missing) && missing[i] == end+1 {
			end = missing[i]
			i++
		}
		ranges = append(ranges, gapRange{From: start, To: end})
	}
	return ranges, false
}

// runEntryChangesCheck is Check 2: stellar.ledger_entry_changes coverage vs.
// tx-bearing ledgers, split at -ec-floor into a below-floor
// (backfill-pending, informational) and at/above-floor (live-covered,
// hard-gated) sub-range via ecFloorSegments BEFORE windowing — so every
// window handed to QueryECWindowCoverage sits entirely on one side of the
// floor and its Missing() count is never ambiguous about which zone it
// belongs to.
func runEntryChangesCheck(ctx context.Context, addr string, from, to, ecFloor uint32) (deficiency, pending uint64, err error) {
	fmt.Printf("\n=== verify-contiguity: check 2 — ledger_entry_changes coverage [%d,%d] (ec-floor=%d) ===\n",
		from, to, ecFloor)

	pendingFrom, pendingTo, hasPending, gatedFrom, gatedTo, hasGated := ecFloorSegments(from, to, ecFloor)

	if hasPending {
		windows, werr := clickhouse.QueryECWindowCoverage(ctx, addr, pendingFrom, pendingTo, contiguityBucketStride)
		if werr != nil {
			return 0, 0, fmt.Errorf("verify-contiguity: check 2 backfill-pending scan: %w", werr)
		}
		for _, w := range windows {
			pending += w.Missing()
			fmt.Println(formatCoverageLine(w.From, w.To, w.TxLedgers, w.ECCovered, "BACKFILL-PENDING (below -ec-floor, informational only)"))
		}
	}
	if hasGated {
		windows, werr := clickhouse.QueryECWindowCoverage(ctx, addr, gatedFrom, gatedTo, contiguityBucketStride)
		if werr != nil {
			return 0, 0, fmt.Errorf("verify-contiguity: check 2 gated scan: %w", werr)
		}
		for _, w := range windows {
			wm := w.Missing()
			deficiency += wm
			tag := "OK"
			if wm > 0 {
				tag = "DEFICIENCY (at/above -ec-floor)"
			}
			fmt.Println(formatCoverageLine(w.From, w.To, w.TxLedgers, w.ECCovered, tag))
		}
	}

	fmt.Printf("check 2: %d deficiency (at/above floor, counts toward exit code), %d backfill-pending (below floor, informational)\n",
		deficiency, pending)
	return deficiency, pending, nil
}

// ecFloorSegments splits [from,to] into the below-ecFloor (backfill-pending)
// and at/above-ecFloor (live-covered, hard-gated) sub-ranges Check 2 scans
// separately. hasPending/hasGated report whether that segment actually
// overlaps [from,to] — e.g. -ec-floor <= -from means the whole requested
// range is gated and there is no pending segment at all. Pure — no
// ClickHouse dependency — so it's unit-testable without a live lake.
func ecFloorSegments(from, to, ecFloor uint32) (pendingFrom, pendingTo uint32, hasPending bool, gatedFrom, gatedTo uint32, hasGated bool) {
	if ecFloor > from {
		pendingFrom = from
		pendingTo = ecFloor - 1
		if pendingTo > to {
			pendingTo = to
		}
		hasPending = pendingTo >= pendingFrom
	}

	gatedFrom = ecFloor
	if gatedFrom < from {
		gatedFrom = from
	}
	gatedTo = to
	hasGated = gatedFrom <= gatedTo

	return pendingFrom, pendingTo, hasPending, gatedFrom, gatedTo, hasGated
}

// formatCoverageLine renders one window's report line — shared by both
// checks, since both are (from, to, expected-count, present-count) shaped:
// Check 1 passes (Expected, Present); Check 2 passes (TxLedgers, ECCovered).
// tag carries the check-specific verdict ("GAP", "OK",
// "DEFICIENCY (at/above -ec-floor)", "BACKFILL-PENDING (...)"). Pure — no
// ClickHouse dependency — so it's unit-testable without a live lake.
func formatCoverageLine(from, to uint32, expected, present uint64, tag string) string {
	return fmt.Sprintf("  [%d,%d]  expected=%d present=%d missing=%d  %s", from, to, expected, present, expected-present, tag)
}
