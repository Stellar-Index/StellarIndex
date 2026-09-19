// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

//go:build k015evidence

package ingest

import (
	"strings"
	"testing"
)

// ─── K015: the two subcommands that still default to WRITE ─────────
//
// `backfill` (here) and `ch-backfill` (internal/ops/chops) declare
// `-dry-run false` — they APPLY unless the operator remembers a flag,
// the default-WRITE shape opsutil.WriteGate exists to remove. Every
// other mutating ops subcommand now previews by default.
//
// RED and build-tagged on purpose. The flip is not a code-only change:
// scripts/ops/ch-live-catchup.sh (a timer), ch-full-backfill.sh,
// phaseD-backfill.sh, phaseD-range.sh, ordinal-rederive-chunks.sh and
// restore-drill.sh all invoke these with no mode flag, so flipping the
// default without them turns a production catch-up into a silent
// preview that still exits 0 — the DO-NOTHING half of the same trap.
// Those files are outside this unit's set; this test is the evidence
// and the acceptance check for whoever lands the sweep. Drop the build
// tag when it is green:
//
//	go test -tags k015evidence ./internal/ops/ingest/ -run TestBackfillDefaultsToWrite -v
func TestBackfillDefaultsToWrite(t *testing.T) {
	usage := subcommandUsage(t, "backfill")
	writeLine := usageFlagLine(usage, "write")
	switch {
	case writeLine == "":
		t.Errorf("backfill declares no -write flag: it APPLIES unless -dry-run is passed, " +
			"so a mistyped range is written on the first run (K015). Flip it with its callers")
	case !strings.Contains(writeLine, "fail-closed DRY RUN"):
		t.Errorf("backfill declares a -write flag that is not the shared opsutil gate. Got:\n%s", writeLine)
	}
	if dryRun := usageFlagLine(usage, "dry-run"); dryRun != "" && !strings.Contains(dryRun, "the DEFAULT") {
		t.Errorf("backfill's -dry-run is an opt-OUT of writing, not the shared no-op alias. Got:\n%s", dryRun)
	}
}
