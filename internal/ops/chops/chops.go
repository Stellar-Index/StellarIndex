// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

// Package chops holds the stellarindex-ops ClickHouse-lake
// subcommands (named chops, not clickhouse, to avoid a same-named
// import shadowing internal/storage/clickhouse in every file here):
// `ch-backfill`, `ch-gate`, `ch-reproject`, `ch-rebuild`, `ch-supply`,
// `ch-txindex-backfill`, `ch-participant-backfill`, `ch-recognition`,
// `verify-recognition`, `verify-reconciliation`, `compute-completeness`,
// `verify-served-values`, `verify-usd-volume`, `sdex-claim-audit`,
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
// verb (one of the twenty this package owns); args[1:] are its flags.
//
// Split across two dispatch helpers by ROLE — the data-mutating tools and
// the verifiers. The split is what keeps each switch under the gocyclo
// ceiling as verbs accumulate, and the boundary is a real one: a `ch-*` /
// `*-backfill` / `*-rebuild` verb rewrites lake or served DATA, while
// nothing in the verifier half touches trade/event rows at all.
func Run(args []string) error {
	if fn, ok := lakeMutatorVerb(args[0]); ok {
		return fn(args[1:])
	}
	if fn, ok := verifierVerb(args[0]); ok {
		return fn(args[1:])
	}
	return fmt.Errorf("internal/ops/chops: unknown subcommand %q", args[0])
}

// lakeMutatorVerb resolves the WRITING half: the ADR-0034 lake
// backfill/gate/reproject/rebuild tools plus the projected-source and
// classic-movement backfills.
func lakeMutatorVerb(verb string) (func([]string) error, bool) {
	switch verb {
	case "ch-backfill":
		return chBackfill, true
	case "ch-gate":
		return chGate, true
	case "ch-reproject":
		return chReproject, true
	case "ch-rebuild":
		return chRebuild, true
	case "ch-supply":
		return chSupply, true
	case "ch-txindex-backfill":
		return chTxIndexBackfill, true
	case "ch-contract-ledgers-backfill":
		return chContractLedgersBackfill, true
	case "ch-cap67-movements":
		return chCap67Movements, true
	case "ch-participant-backfill":
		return chParticipantBackfill, true
	case "ch-recognition":
		return chRecognition, true
	case "classic-movements-backfill":
		return classicMovementsBackfill, true
	case "projected-rebuild":
		return projectedRebuild, true
	default:
		return nil, false
	}
}

// verifierVerb resolves the verification half: the ADR-0033/0034
// completeness, reconciliation, contiguity, hash-chain and value checks.
// None of these touch trade/event data. Most are strictly read-only; the
// exception is compute-completeness, which writes its VERDICT
// (completeness_snapshots + the projection floors it earns) — bookkeeping
// about the data, never the data itself.
func verifierVerb(verb string) (func([]string) error, bool) {
	switch verb {
	case "verify-recognition":
		return verifyRecognition, true
	case "verify-reconciliation":
		return verifyReconciliation, true
	case "compute-completeness":
		return computeCompleteness, true
	case "verify-served-values":
		return verifyServedValues, true
	case "verify-usd-volume":
		return verifyUSDVolume, true
	case "sdex-claim-audit":
		return sdexClaimAudit, true
	case "reconcile-balances":
		return reconcileBalances, true
	case "verify-contiguity":
		return verifyContiguity, true
	case "verify-hashchain":
		return verifyHashChain, true
	case "verify-lake":
		return verifyLake, true
	default:
		return nil, false
	}
}
