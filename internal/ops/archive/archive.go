// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

// Package archive holds the stellarindex-ops archive-integrity + WASM
// history subcommands: `verify-archive`, `archive-completeness`,
// `cross-region-check`, `cross-region-monitor`, `trim-galexie-archive`,
// `rehydrate-galexie-archive`, `wasm-history`,
// `wasm-history-merge-jsonl`, `extract-wasm-from-galexie`.
//
// wasm-history/extract-wasm-from-galexie live here rather than in
// internal/ops/discovery (the maintainability-audit-2026-07-01 target
// list groups "Soroban discovery / WASM tracking" together) because
// they're genuinely code-coupled with verify-archive: verify_archive.go
// declares the wasm-history JSON output types (wasmRange,
// contractHistory, wasmContractState, storageChange,
// contractStorageHistory, codeUpload) that wasm_history.go builds, and
// wasm_history.go's RangeChunk-splitting helper (now
// internal/ops/opsutil.SplitRange) is what verify-archive's chunked
// walker uses too. Splitting them into separate packages would have
// meant a cross-package cycle or moving six structurally-tied types
// into opsutil for no real decoupling benefit — keeping the archive
// walker and its WASM-tracking sibling in one package is the more
// honest reflection of the actual code, discovery.go's own concern
// (auto-discovered SEP-41 contracts) is unrelated to either.
//
// Extracted from cmd/stellarindex-ops (maintainability audit
// 2026-07-01, D1 finding M1-5); main.go's dispatch table calls Run
// below.
package archive

import (
	"fmt"
)

// Run is the internal/ops/archive package's entry point — see
// discovery.Run's doc comment for the calling convention shared by
// every internal/ops/* package post-split. args[0] is the subcommand
// verb (one of the nine this package owns, `archive-completeness` and
// `wasm-history-merge-jsonl` included); args[1:] are its flags.
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
	case "wasm-history":
		return wasmHistory(args[1:])
	case "wasm-history-merge-jsonl":
		return wasmHistoryMergeJSONL(args[1:])
	case "extract-wasm-from-galexie":
		return extractWasmFromGalexie(args[1:])
	default:
		return fmt.Errorf("internal/ops/archive: unknown subcommand %q", args[0])
	}
}
