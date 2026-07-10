// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

// Package ingest holds the stellarindex-ops ingest / backfill
// subcommands: `backfill`, `backfill-external`, `backfill-chainlink`,
// `backfill-router`, `detect-gaps`, `list-cursors`, `resume-stalled`,
// `find-data-gaps`, `census-backfill`, `tag-routed-via`,
// `seed-soroswap-pairs`, `seed-protocol-contracts`,
// `seed-entry-counts`, `projector-replay`, `scan-soroban-events`,
// `state-snapshot`, `issuer-enrich`, `sep1-refresh`. Extracted from
// cmd/stellarindex-ops (maintainability audit 2026-07-01, D1 finding
// M1-5); main.go's dispatch table calls Run below.
package ingest

import (
	"fmt"
)

// Run is the internal/ops/ingest package's entry point — see
// discovery.Run's doc comment for the calling convention shared by
// every internal/ops/* package post-split. args[0] is the subcommand
// verb (one of the eighteen this package owns); args[1:] are its flags.
func Run(args []string) error {
	switch args[0] {
	case "backfill":
		return backfill(args[1:])
	case "backfill-external":
		return backfillExternal(args[1:])
	case "backfill-chainlink":
		return backfillChainlink(args[1:])
	case "backfill-router":
		return backfillRouter(args[1:])
	case "detect-gaps":
		return detectGaps(args[1:])
	case "list-cursors":
		return listCursors(args[1:])
	case "resume-stalled":
		return resumeStalled(args[1:])
	case "find-data-gaps":
		return findDataGaps(args[1:])
	case "census-backfill":
		return censusBackfill(args[1:])
	case "tag-routed-via":
		return tagRoutedVia(args[1:])
	case "seed-soroswap-pairs":
		return seedSoroswapPairs(args[1:])
	case "seed-protocol-contracts":
		return seedProtocolContracts(args[1:])
	case "seed-entry-counts":
		return seedEntryCounts(args[1:])
	case "projector-replay":
		return projectorReplay(args[1:])
	case "scan-soroban-events":
		return scanSorobanEvents(args[1:])
	case "state-snapshot":
		return stateSnapshot(args[1:])
	case "issuer-enrich":
		return issuerEnrich(args[1:])
	case "sep1-refresh":
		return sep1RefreshCmd(args[1:])
	default:
		return fmt.Errorf("internal/ops/ingest: unknown subcommand %q", args[0])
	}
}
