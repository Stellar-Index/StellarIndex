// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// verifyLake is the stellarindex-ops `verify-lake` subcommand — a single
// "is the lake sound?" invocation with one verdict + exit code, run daily by
// verify-lake.timer and used as the restore acceptance gate. Over one
// resolved [-from,-to] range it runs:
//
//  1. Ledger substrate contiguity (verify-contiguity's Check 1).
//  2. stellar.ledger_entry_changes coverage, floor-gated at -ec-floor
//     (verify-contiguity's Check 2; 0 = auto-derived from the lake) —
//     below the floor is backfill-pending and informational only, and the
//     exempted range is printed, same as verify-contiguity.
//  3. Hash-chain integrity, in-window + boundary links (verify-hashchain's
//     one check).
//  4. Raw-table census: transactions, operations and contract_events
//     against the ledger headers' tx/op/soroban_event counts per 1M-ledger
//     partition; operation_results and operation_participants presence-only
//     (verify_raw_census.go).
//
// Checks 1-3 call the same run* funcs verify-contiguity and verify-hashchain
// call. Each check prints its own report section, then one summary block.
//
// Exit code = ledger gaps + entry-change deficiencies at/above -ec-floor +
// hash-chain broken links + short raw-table partitions, capped at 255 —
// backfill-pending entry_changes below -ec-floor are reported but never
// counted, mirroring verify-contiguity's own floor-gating.
//
// -textfile PATH writes lake_verify.prom (per-check failure counts, range,
// last-run time) once every requested check has completed; a run that errors
// out writes nothing, so the stale alert covers it.
//
// Usage: verify-lake [-config PATH] [-ch-addr H:P] [-from N] [-to N]
// [-ec-floor N] [-checks contiguity,entrychanges,hashchain,rawcensus]
// [-textfile PATH]. Read-only; touches ClickHouse only (no Postgres).
//
// reconcile-balances (the ADR-0033 external-Horizon balance-sample
// check) is deliberately NOT composed in here: it's network-bound
// (calls public Horizon) and account-sampled rather than range-scoped —
// a different shape from these structural lake checks. Run it
// separately: `stellarindex-ops reconcile-balances -sample N`.
func verifyLake(args []string) error {
	fs := flag.NewFlagSet("verify-lake", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/stellarindex.toml", "path to stellarindex.toml — used only to resolve the default -ch-addr (this tool reads ClickHouse only, never Postgres); a missing/unreadable file is tolerated when -ch-addr is passed explicitly")
	chAddr := fs.String("ch-addr", "", "ClickHouse native address (default: cfg.storage.clickhouse_addr from -config, falling back to "+defaultCHAddr+" if -config can't be loaded)")
	from := fs.Uint64("from", 2, "first ledger sequence to verify (inclusive); 2 is genesis")
	to := fs.Uint64("to", 0, "last ledger sequence to verify (inclusive); 0 = auto (max ledger_seq in stellar.ledgers)")
	ecFloor := fs.Uint64("ec-floor", 0, ecFloorUsage)
	checksFlag := fs.String("checks", lakeChecksDefault, "comma-separated subset of checks to run: contiguity | entrychanges | hashchain | rawcensus")
	textfile := fs.String("textfile", "", "node_exporter textfile path, e.g. /var/lib/node_exporter/textfile_collector/lake_verify.prom; empty = none")
	if err := fs.Parse(args); err != nil {
		return err
	}

	c, err := parseLakeChecks(*checksFlag)
	if err != nil {
		return err
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

	toSeq, err := resolveToSeq(ctx, "verify-lake", *cfgPath, addr, *to)
	if err != nil {
		return err
	}
	if toSeq < fromSeq {
		return fmt.Errorf("verify-lake: resolved range [%d,%d] is empty (-to < -from)", fromSeq, toSeq)
	}

	fmt.Fprintf(os.Stderr, "verify-lake: range=[%d,%d] ch-addr=%s ec-floor=%s checks=%s\n",
		fromSeq, toSeq, addr, ecFloorFlagLabel(ecFloorSeq), *checksFlag)

	n, err := runLakeChecks(ctx, c, addr, fromSeq, toSeq, ecFloorSeq)
	if err != nil {
		return err
	}
	total := n.total()

	fmt.Printf("\n=== verify-lake: LAKE VERIFICATION [%d,%d] ===\n", fromSeq, toSeq)
	fmt.Print(lakeCheckLine("contiguity", c.contiguity, fmt.Sprintf("%d missing ledger(s)", n.gaps)))
	fmt.Print(lakeCheckLine("entry_changes", c.entryChanges, fmt.Sprintf("%d deficiency (%d backfill-pending, informational)", n.ecDeficiency, n.ecPending)))
	fmt.Print(lakeCheckLine("hash_chain", c.hashChain, fmt.Sprintf("%d broken link(s)", n.hashChainBroken)))
	fmt.Print(lakeCheckLine("raw_census", c.rawCensus, fmt.Sprintf("%d short partition(s)", n.censusShort)))

	verdict := "PASSED"
	if total > 0 {
		verdict = "FAILED"
	}
	fmt.Printf("verify-lake: summary total_failures=%d checks_run=%s  (%s)\n", total, lakeChecksRunLabel(c), verdict)

	if *textfile != "" {
		if err := writeAtomic(*textfile, renderLakeVerifyProm(c, n, fromSeq, toSeq, time.Now())); err != nil {
			return fmt.Errorf("verify-lake: write textfile: %w", err)
		}
	}

	if total == 0 {
		return nil
	}
	if total > 255 {
		fmt.Fprintf(os.Stderr, "verify-lake: %d exceeds the max process exit code (255) — reporting 255\n", total)
	}
	return &opsutil.ExitCodeError{Code: lakeExitCode(n.gaps, n.ecDeficiency, n.hashChainBroken, n.censusShort)}
}

// lakeChecks is the set of checks a verify-lake run requested.
type lakeChecks struct{ contiguity, entryChanges, hashChain, rawCensus bool }

// lakeCounts holds each check's failure count; zero for a check that did not run.
type lakeCounts struct {
	gaps, ecDeficiency, ecPending, hashChainBroken, censusShort uint64
}

// total excludes ecPending: backfill-pending entry changes are informational.
func (n lakeCounts) total() uint64 {
	return n.gaps + n.ecDeficiency + n.hashChainBroken + n.censusShort
}

func runLakeChecks(ctx context.Context, c lakeChecks, addr string, from, to, ecFloor uint32) (lakeCounts, error) {
	var n lakeCounts
	var err error
	if c.contiguity {
		if n.gaps, err = runLedgerContiguityCheck(ctx, addr, from, to); err != nil {
			return n, fmt.Errorf("verify-lake: contiguity check: %w", err)
		}
	}
	if c.entryChanges {
		if n.ecDeficiency, n.ecPending, err = runEntryChangesCheck(ctx, addr, from, to, ecFloor); err != nil {
			return n, fmt.Errorf("verify-lake: entry-changes check: %w", err)
		}
	}
	if c.hashChain {
		inWindow, boundary, err := runHashChainCheck(ctx, addr, from, to)
		if err != nil {
			return n, fmt.Errorf("verify-lake: hash-chain check: %w", err)
		}
		n.hashChainBroken = inWindow + boundary
	}
	if c.rawCensus {
		if n.censusShort, err = runRawCensusCheck(ctx, addr, from, to); err != nil {
			return n, fmt.Errorf("verify-lake: raw-census check: %w", err)
		}
	}
	return n, nil
}

// lakeExitCode computes verify-lake's exit code from the composed checks'
// failure counts, capped at the max process exit code (255) — mirrors
// verify-contiguity's and verify-hashchain's own capping. Pure.
func lakeExitCode(gaps, deficiency, broken, censusShort uint64) int {
	total := gaps + deficiency + broken + censusShort
	if total > 255 {
		return 255
	}
	return int(total) //nolint:gosec // capped above; always in [0,255].
}

// renderLakeVerifyProm renders lake_verify.prom. A check narrowed out via
// -checks emits no failures series, so a narrowed run never reads as a pass
// for a check it did not perform. Pure.
func renderLakeVerifyProm(c lakeChecks, n lakeCounts, from, to uint32, now time.Time) string {
	var b strings.Builder
	b.WriteString("# HELP stellarindex_lake_verify_failures Failures verify-lake found per check over its range (missing ledgers, entry-change deficiencies, broken hash links, short raw-table partitions). Emitted only for checks that ran.\n")
	b.WriteString("# TYPE stellarindex_lake_verify_failures gauge\n")
	for _, f := range []struct {
		check string
		ran   bool
		n     uint64
	}{
		{"contiguity", c.contiguity, n.gaps},
		{"entry_changes", c.entryChanges, n.ecDeficiency},
		{"hash_chain", c.hashChain, n.hashChainBroken},
		{"raw_census", c.rawCensus, n.censusShort},
	} {
		if f.ran {
			fmt.Fprintf(&b, "stellarindex_lake_verify_failures{check=%q} %d\n", f.check, f.n)
		}
	}
	b.WriteString("# HELP stellarindex_lake_verify_from_ledger First ledger of the last completed verify-lake run.\n")
	b.WriteString("# TYPE stellarindex_lake_verify_from_ledger gauge\n")
	fmt.Fprintf(&b, "stellarindex_lake_verify_from_ledger %d\n", from)
	b.WriteString("# HELP stellarindex_lake_verify_to_ledger Last ledger of the last completed verify-lake run.\n")
	b.WriteString("# TYPE stellarindex_lake_verify_to_ledger gauge\n")
	fmt.Fprintf(&b, "stellarindex_lake_verify_to_ledger %d\n", to)
	b.WriteString("# HELP stellarindex_lake_verify_last_run_unix When verify-lake last completed every requested check.\n")
	b.WriteString("# TYPE stellarindex_lake_verify_last_run_unix gauge\n")
	fmt.Fprintf(&b, "stellarindex_lake_verify_last_run_unix %d\n", now.Unix())
	return b.String()
}

// lakeCheckLine formats one verify-lake summary line. A check narrowed out
// via -checks prints "SKIPPED (not requested)" rather than a zero count —
// "not run" and "ran, found zero" must not share one representation
// , or a report from a narrowed run reads as full coverage. Pure
// — unit-testable without a live lake.
func lakeCheckLine(label string, ran bool, detail string) string {
	if !ran {
		return fmt.Sprintf("  %-15s SKIPPED (not requested)\n", label+":")
	}
	return fmt.Sprintf("  %-15s %s\n", label+":", detail)
}

// lakeChecksRunLabel renders the -checks tokens that actually ran, for the
// summary line's checks_run= field — so a pasted report is self-describing
// about its own coverage without the reader having to infer it from which
// lines say SKIPPED. Pure — unit-testable without a live lake.
func lakeChecksRunLabel(c lakeChecks) string {
	var ran []string
	if c.contiguity {
		ran = append(ran, lakeCheckContiguity)
	}
	if c.entryChanges {
		ran = append(ran, lakeCheckEntryChanges)
	}
	if c.hashChain {
		ran = append(ran, lakeCheckHashChain)
	}
	if c.rawCensus {
		ran = append(ran, lakeCheckRawCensus)
	}
	if len(ran) == 0 {
		return "none"
	}
	return strings.Join(ran, ",")
}

// -checks flag tokens.
const (
	lakeCheckContiguity   = "contiguity"
	lakeCheckEntryChanges = "entrychanges"
	lakeCheckHashChain    = "hashchain"
	lakeCheckRawCensus    = "rawcensus"

	lakeChecksDefault = lakeCheckContiguity + "," + lakeCheckEntryChanges + "," + lakeCheckHashChain + "," + lakeCheckRawCensus
)

// parseLakeChecks turns the -checks flag's comma-separated token list
// into which of the composed checks to run, so an operator can
// subset a run (e.g. -checks hashchain to re-verify just the chain
// after a targeted fix). Unknown tokens and an empty resulting set are
// both errors — a typo in -checks silently running zero checks would
// defeat the whole point of a "is the lake sound?" gate. Pure — no
// ClickHouse dependency — so it's unit-testable without a live lake.
func parseLakeChecks(raw string) (lakeChecks, error) {
	var c lakeChecks
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		switch tok {
		case lakeCheckContiguity:
			c.contiguity = true
		case lakeCheckEntryChanges:
			c.entryChanges = true
		case lakeCheckHashChain:
			c.hashChain = true
		case lakeCheckRawCensus:
			c.rawCensus = true
		default:
			return lakeChecks{}, fmt.Errorf("verify-lake: -checks: unknown check %q (want %s|%s|%s|%s)",
				tok, lakeCheckContiguity, lakeCheckEntryChanges, lakeCheckHashChain, lakeCheckRawCensus)
		}
	}
	if c == (lakeChecks{}) {
		return lakeChecks{}, fmt.Errorf("verify-lake: -checks: no valid checks specified (want a comma list of %s|%s|%s|%s)",
			lakeCheckContiguity, lakeCheckEntryChanges, lakeCheckHashChain, lakeCheckRawCensus)
	}
	return c, nil
}
