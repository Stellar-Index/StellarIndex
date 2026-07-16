// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

// Package chops holds the stellarindex-ops ClickHouse-lake
// subcommands (named chops, not clickhouse, to avoid a same-named
// import shadowing internal/storage/clickhouse in every file here):
// `ch-backfill`, `ch-gate`, `ch-reproject`, `ch-rebuild`, `ch-supply`,
// `ch-txindex-backfill`, `ch-participant-backfill`, `ch-recognition`,
// `verify-recognition`, `verify-reconciliation`, `compute-completeness`,
// `verify-served-values`, `sdex-claim-audit`,
// `classic-movements-backfill`, `projected-rebuild`,
// `reconcile-balances`, `verify-contiguity`, `verify-hashchain`,
// `verify-lake` — ADR-0033/ADR-0034 completeness + reconciliation checks,
// the ADR-0034 Phase 2-4 lake backfill/gate/reproject/rebuild tools, the
// ADR-0047 pre-P23 classic-movement reconstruction backfill, the ADR-0048
// D3 bulk catch-up path for projected sources, the reconcile-balances
// external (Horizon) balance-reconciliation verifier, verify-contiguity's
// standing ledger-substrate + entry_changes-coverage lake verification,
// verify-hashchain's standing hash-chain verification (the "hash-chained
// to genesis" half of ADR-0034's provable-100% claim that verify-contiguity
// doesn't cover), and verify-lake's composition of all three of the above
// into a single "is the lake sound?" invocation for cron/Healthchecks.io
// (verify_lake.go calls no check logic of its own — it orchestrates the
// same package-private run* funcs verify-contiguity and verify-hashchain
// call), which is why reconciliation_catalogue.go and gated_recon_seed.go
// (shared re-derivation source-set + factory-child preseed helpers used
// by ch-rebuild, ch-reproject, compute-completeness, and
// verify-reconciliation) live here too rather than in a 7th package.
//
// Extracted from cmd/stellarindex-ops (maintainability audit
// 2026-07-01, D1 finding M1-5); main.go's dispatch table calls Run
// below.
package chops

import (
	"fmt"
)

// Run is the internal/ops/chops package's entry point — see
// discovery.Run's doc comment for the calling convention shared by
// every internal/ops/* package post-split. args[0] is the subcommand
// verb (one of the nineteen this package owns); args[1:] are its flags.
func Run(args []string) error {
	switch args[0] {
	case "ch-backfill":
		return chBackfill(args[1:])
	case "ch-gate":
		return chGate(args[1:])
	case "ch-reproject":
		return chReproject(args[1:])
	case "ch-rebuild":
		return chRebuild(args[1:])
	case "ch-supply":
		return chSupply(args[1:])
	case "ch-txindex-backfill":
		return chTxIndexBackfill(args[1:])
	case "ch-participant-backfill":
		return chParticipantBackfill(args[1:])
	case "ch-recognition":
		return chRecognition(args[1:])
	case "verify-recognition":
		return verifyRecognition(args[1:])
	case "verify-reconciliation":
		return verifyReconciliation(args[1:])
	case "compute-completeness":
		return computeCompleteness(args[1:])
	case "verify-served-values":
		return verifyServedValues(args[1:])
	case "sdex-claim-audit":
		return sdexClaimAudit(args[1:])
	case "classic-movements-backfill":
		return classicMovementsBackfill(args[1:])
	case "projected-rebuild":
		return projectedRebuild(args[1:])
	case "reconcile-balances":
		return reconcileBalances(args[1:])
	case "verify-contiguity":
		return verifyContiguity(args[1:])
	case "verify-hashchain":
		return verifyHashChain(args[1:])
	case "verify-lake":
		return verifyLake(args[1:])
	default:
		return fmt.Errorf("internal/ops/chops: unknown subcommand %q", args[0])
	}
}
