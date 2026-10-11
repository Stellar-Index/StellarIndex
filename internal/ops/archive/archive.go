// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

// Package archive holds the stellarindex-ops archive-integrity and WASM-history subcommands (verify-archive,
// archive-completeness, cross-region-*, galexie archive trim/rehydrate/mirror-verify, wasm-history*, compare-entry-changes).
// wasm-history lives here, not in ops/discovery, because it shares verify_archive.go's JSON output types and SplitRange chunking. Run is called from cmd/stellarindex-ops/main.go.
package archive

import (
	"fmt"
)

// Run is the package entry point (see discovery.Run for the shared convention); args[0] is the subcommand verb, args[1:] its flags.
func Run(args []string) error {
	switch args[0] {
	case "verify-archive":
		return verifyArchive(args[1:])
	case "archive-completeness":
		return archiveCompleteness(args[1:])
	case "cross-region-check":
		return crossRegionCheck(args[1:])
	case "cross-region-monitor":
		return crossRegionMonitor(args[1:])
	case "trim-galexie-archive":
		return trimGalexieArchive(args[1:])
	case "rehydrate-galexie-archive":
		return rehydrateGalexieArchive(args[1:])
	case "galexie-mirror-verify":
		return galexieMirrorVerify(args[1:])
	case "wasm-history":
		return wasmHistory(args[1:])
	case "wasm-history-merge-jsonl":
		return wasmHistoryMergeJSONL(args[1:])
	case "extract-wasm-from-galexie":
		return extractWasmFromGalexie(args[1:])
	case "compare-entry-changes":
		return compareEntryChanges(args[1:])
	default:
		return fmt.Errorf("internal/ops/archive: unknown subcommand %q", args[0])
	}
}
