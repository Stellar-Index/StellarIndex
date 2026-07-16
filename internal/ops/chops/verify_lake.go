// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// verifyLake is the stellarindex-ops `verify-lake` subcommand — a single
// "is the lake sound?" invocation for a cron/Healthchecks.io timer that
// wants ONE call (and one verdict + exit code) instead of three. It
// COMPOSES the checks verify-contiguity and verify-hashchain already
// implement, calling the exact same package-private run* funcs those
// subcommands call (runLedgerContiguityCheck, runEntryChangesCheck,
// runHashChainCheck — see verify_contiguity.go / verify_hashchain.go),
// over one resolved [-from,-to] range:
//
//  1. Ledger substrate contiguity (verify-contiguity's Check 1).
//  2. stellar.ledger_entry_changes coverage, floor-gated at -ec-floor
//     (verify-contiguity's Check 2) — below the floor is
//     backfill-pending and informational only, same as verify-contiguity.
//  3. Hash-chain integrity, in-window + boundary links (verify-hashchain's
//     one check).
//
// No check logic is duplicated here — this file only orchestrates:
// resolve addr/from/to once, call the three run* funcs in sequence
// (each prints its own report section, exactly as it does standalone),
// then print one final unified summary block.
//
// Exit code = ledger gaps + entry-change deficiencies at/above
// -ec-floor + hash-chain broken links (in-window + boundary), capped at
// 255 — backfill-pending entry_changes below -ec-floor are reported but
// never counted, mirroring verify-contiguity's own floor-gating.
// Mirrors the sibling verify-* tools' "exit code = number of failed
// checks" convention so cron/Healthchecks.io can consume it directly.
//
// Usage: verify-lake [-config PATH] [-ch-addr H:P] [-from N] [-to N]
// [-ec-floor N] [-checks contiguity,entrychanges,hashchain]. Read-only;
// touches ClickHouse only (no Postgres).
//
// reconcile-balances (the ADR-0033 external-Horizon balance-sample
// check) is deliberately NOT composed in here: it's network-bound
// (calls public Horizon) and account-sampled rather than range-scoped —
// a different shape from these three structural lake checks, and one
// this repo's constraints (no live network calls baked into a
// cron-friendly lake-soundness gate) argue against folding in. Run it
// separately: `stellarindex-ops reconcile-balances -sample N`.
func verifyLake(args []string) error {
	fs := flag.NewFlagSet("verify-lake", flag.ContinueOnError)
	cfgPath := fs.String("config", "/etc/stellarindex.toml", "path to stellarindex.toml — used only to resolve the default -ch-addr (this tool reads ClickHouse only, never Postgres); a missing/unreadable file is tolerated when -ch-addr is passed explicitly")
	chAddr := fs.String("ch-addr", "", "ClickHouse native address (default: cfg.storage.clickhouse_addr from -config, falling back to "+defaultCHAddr+" if -config can't be loaded)")
	from := fs.Uint64("from", 2, "first ledger sequence to verify (inclusive); 2 is genesis")
	to := fs.Uint64("to", 0, "last ledger sequence to verify (inclusive); 0 = auto (max ledger_seq in stellar.ledgers)")
	ecFloor := fs.Uint64("ec-floor", defaultECFloor, "ledger at/above which missing stellar.ledger_entry_changes coverage is a hard failure (counts toward the exit code); below it, missing coverage is backfill-pending, informational only — see verify-contiguity")
	checksFlag := fs.String("checks", "contiguity,entrychanges,hashchain", "comma-separated subset of checks to run: contiguity | entrychanges | hashchain")
	if err := fs.Parse(args); err != nil {
		return err
	}

	runContiguity, runEntryChanges, runHashChain, err := parseLakeChecks(*checksFlag)
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

	toSeq, err := resolveToSeq(ctx, addr, *to)
	if err != nil {
		return err
	}
	if toSeq < fromSeq {
		return fmt.Errorf("verify-lake: resolved range [%d,%d] is empty (-to < -from)", fromSeq, toSeq)
	}

	fmt.Fprintf(os.Stderr, "verify-lake: range=[%d,%d] ch-addr=%s ec-floor=%d checks=%s\n",
		fromSeq, toSeq, addr, ecFloorSeq, *checksFlag)

	var ledgerGaps, ecDeficiency, ecPending, hcInWindow, hcBoundary uint64

	if runContiguity {
		ledgerGaps, err = runLedgerContiguityCheck(ctx, addr, fromSeq, toSeq)
		if err != nil {
			return fmt.Errorf("verify-lake: contiguity check: %w", err)
		}
	}
	if runEntryChanges {
		ecDeficiency, ecPending, err = runEntryChangesCheck(ctx, addr, fromSeq, toSeq, ecFloorSeq)
		if err != nil {
			return fmt.Errorf("verify-lake: entry-changes check: %w", err)
		}
	}
	if runHashChain {
		hcInWindow, hcBoundary, err = runHashChainCheck(ctx, addr, fromSeq, toSeq)
		if err != nil {
			return fmt.Errorf("verify-lake: hash-chain check: %w", err)
		}
	}

	hashChainBroken := hcInWindow + hcBoundary
	total := ledgerGaps + ecDeficiency + hashChainBroken

	fmt.Printf("\n=== verify-lake: LAKE VERIFICATION [%d,%d] ===\n", fromSeq, toSeq)
	fmt.Printf("  contiguity:     %d missing ledger(s)\n", ledgerGaps)
	fmt.Printf("  entry_changes:  %d deficiency (%d backfill-pending, informational)\n", ecDeficiency, ecPending)
	fmt.Printf("  hash_chain:     %d broken link(s)\n", hashChainBroken)

	verdict := "PASSED"
	if total > 0 {
		verdict = "FAILED"
	}
	fmt.Printf("verify-lake: summary total_failures=%d  (%s)\n", total, verdict)

	if total == 0 {
		return nil
	}
	if total > 255 {
		fmt.Fprintf(os.Stderr, "verify-lake: %d exceeds the max process exit code (255) — reporting 255\n", total)
	}
	return &opsutil.ExitCodeError{Code: lakeExitCode(ledgerGaps, ecDeficiency, hashChainBroken)}
}

// lakeExitCode computes verify-lake's exit code from the three
// composed checks' failure counts, capped at the max process exit
// code (255) — mirrors verify-contiguity's and verify-hashchain's own
// capping (see their doc comments). Pure — no ClickHouse dependency —
// so it's unit-testable without a live lake.
func lakeExitCode(gaps, deficiency, broken uint64) int {
	total := gaps + deficiency + broken
	if total > 255 {
		return 255
	}
	return int(total) //nolint:gosec // capped above; always in [0,255].
}

// -checks flag tokens.
const (
	lakeCheckContiguity   = "contiguity"
	lakeCheckEntryChanges = "entrychanges"
	lakeCheckHashChain    = "hashchain"
)

// parseLakeChecks turns the -checks flag's comma-separated token list
// into which of the three composed checks to run, so an operator can
// subset a run (e.g. -checks hashchain to re-verify just the chain
// after a targeted fix). Unknown tokens and an empty resulting set are
// both errors — a typo in -checks silently running zero checks would
// defeat the whole point of a "is the lake sound?" gate. Pure — no
// ClickHouse dependency — so it's unit-testable without a live lake.
func parseLakeChecks(raw string) (contiguity, entryChanges, hashChain bool, err error) {
	for _, tok := range strings.Split(raw, ",") {
		tok = strings.TrimSpace(tok)
		if tok == "" {
			continue
		}
		switch tok {
		case lakeCheckContiguity:
			contiguity = true
		case lakeCheckEntryChanges:
			entryChanges = true
		case lakeCheckHashChain:
			hashChain = true
		default:
			return false, false, false, fmt.Errorf("verify-lake: -checks: unknown check %q (want %s|%s|%s)",
				tok, lakeCheckContiguity, lakeCheckEntryChanges, lakeCheckHashChain)
		}
	}
	if !contiguity && !entryChanges && !hashChain {
		return false, false, false, fmt.Errorf("verify-lake: -checks: no valid checks specified (want a comma list of %s|%s|%s)",
			lakeCheckContiguity, lakeCheckEntryChanges, lakeCheckHashChain)
	}
	return contiguity, entryChanges, hashChain, nil
}
