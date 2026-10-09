// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

// Package chops holds the stellarindex-ops ClickHouse-lake subcommands
// (named chops, not clickhouse, so the import does not shadow
// internal/storage/clickhouse): the ADR-0034 lake backfill, gate,
// reproject and rebuild tools, the ADR-0047 classic-movement backfill, the
// ADR-0048 projected-source catch-up, and the ADR-0033/ADR-0034
// completeness, reconciliation, contiguity, hash-chain and network-state
// verifiers. The re-derivation helpers those tools share
// (reconciliation_catalogue.go, gated_recon_seed.go) live here too.
//
// cmd/stellarindex-ops main.go's dispatch table calls Run below.
package chops

import (
	"fmt"
)

// Run is the package's entry point; see discovery.Run for the calling
// convention. args[0] is the subcommand verb, args[1:] its flags.
//
// Dispatch is split by role, which keeps each switch under the gocyclo
// ceiling: lake-mutating verbs rewrite lake or served data, while the
// verifiers touch no trade or event row.
func Run(args []string) error {
	if fn, ok := lakeMutatorVerb(args[0]); ok {
		return fn(args[1:])
	}
	if fn, ok := historyDeriveVerb(args[0]); ok {
		return fn(args[1:])
	}
	if fn, ok := verifierVerb(args[0]); ok {
		return fn(args[1:])
	}
	// Re-materialises served aggregates; rewrites no lake, trade or event row.
	if args[0] == tradesCAGGRefreshVerb {
		return tradesCAGGRefresh(args[1:])
	}
	return fmt.Errorf("internal/ops/chops: unknown subcommand %q", args[0])
}

// lakeMutatorVerb resolves the WRITING half: the ADR-0034 lake
// backfill/gate/reproject/rebuild tools plus the projected-source and
// classic-movement backfills, and usd-volume-restamp — the served-tier
// corrective write for verify-usd-volume's exact-tier violations.
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
	case "ch-instance-backfill":
		return chInstanceBackfill, true
	case "ch-census-rollup":
		return chCensusRollup, true
	case "ch-holders-rollup":
		return chHoldersRollup, true
	case "ch-creators-rollup":
		return chCreatorsRollup, true
	case "ch-sponsors-rollup":
		return chSponsorsRollup, true
	case "ch-cohort-rollup":
		return chCohortRollup, true
	case "ch-participant-backfill":
		return chParticipantBackfill, true
	case "ch-recognition":
		return chRecognition, true
	case "projected-rebuild":
		return projectedRebuild, true
	case "usd-volume-restamp":
		return usdVolumeRestamp, true
	default:
		return nil, false
	}
}

// historyDeriveVerb resolves the lake → per-account/per-asset history derives,
// split from lakeMutatorVerb to keep each switch under the gocyclo ceiling.
func historyDeriveVerb(verb string) (func([]string) error, bool) {
	switch verb {
	case "ch-cap67-movements":
		return chCap67Movements, true
	case "ch-entry-history":
		return chEntryHistory, true
	case "classic-movements-backfill":
		return classicMovementsBackfill, true
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
	case "verify-network-state":
		return verifyNetworkState, true
	case "wasm-drift":
		return wasmDrift, true
	default:
		return nil, false
	}
}
