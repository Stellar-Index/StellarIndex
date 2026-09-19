// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

//go:build k015evidence

package chops

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"testing"
)

// ─── K015: ch-backfill still defaults to WRITE ─────────────────────
//
// ch-backfill declares `-dry-run false` — it APPLIES unless the
// operator remembers a flag, the default-WRITE shape opsutil.WriteGate
// exists to remove (its sibling `backfill` has the same shape; see
// internal/ops/ingest/write_gate_backfill_evidence_test.go). Every
// other mutating ops subcommand now previews by default.
//
// RED and build-tagged on purpose. The flip is not a code-only change:
// scripts/ops/ch-live-catchup.sh (a timer), ch-full-backfill.sh,
// phaseD-backfill.sh, phaseD-range.sh, ordinal-rederive-chunks.sh and
// restore-drill.sh all invoke ch-backfill with no mode flag, so
// flipping the default without them turns the production catch-up into
// a silent preview that still exits 0 and records its windows as done
// — the DO-NOTHING half of the same trap. Those files are outside this
// unit's set; this test is the evidence and the acceptance check for
// whoever lands the sweep. Drop the build tag when it is green:
//
//	go test -tags k015evidence ./internal/ops/chops/ -run TestCHBackfillDefaultsToWrite -v
//
// Note for that sweep: ch-rebuild is NOT in scope here. It carries its
// own fail-closed `-write` (default false), so it is convention drift,
// not a default-write hazard.
func TestCHBackfillDefaultsToWrite(t *testing.T) {
	usage := chopsSubcommandUsage(t, "ch-backfill")
	writeLine := chopsUsageFlagLine(usage, "write")
	switch {
	case writeLine == "":
		t.Errorf("ch-backfill declares no -write flag: it APPLIES unless -dry-run is passed, " +
			"so a mistyped range is written to the lake on the first run (K015). Flip it with its callers")
	case !strings.Contains(writeLine, "fail-closed DRY RUN"):
		t.Errorf("ch-backfill declares a -write flag that is not the shared opsutil gate. Got:\n%s", writeLine)
	}
	if dryRun := chopsUsageFlagLine(usage, "dry-run"); dryRun != "" && !strings.Contains(dryRun, "the DEFAULT") {
		t.Errorf("ch-backfill's -dry-run is an opt-OUT of writing, not the shared no-op alias. Got:\n%s", dryRun)
	}
}

// chopsSubcommandUsage runs `<verb> -h` through the package's real
// dispatch function and returns what it printed. flag.ContinueOnError
// makes -h print the defaults and return flag.ErrHelp from Parse, before
// the handler touches a config file or a database.
func chopsSubcommandUsage(t *testing.T, verb string) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	done := make(chan string, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, r)
		done <- buf.String()
	}()

	orig := os.Stderr
	os.Stderr = w
	runErr := Run([]string{verb, "-h"})
	os.Stderr = orig
	_ = w.Close()
	usage := <-done
	_ = r.Close()

	if !errors.Is(runErr, flag.ErrHelp) {
		t.Fatalf("%s -h returned %v, want flag.ErrHelp — the handler did work before parsing, "+
			"so this test is not reading its declared flags", verb, runErr)
	}
	if usage == "" {
		t.Fatalf("%s -h printed nothing — this test is asserting nothing", verb)
	}
	return usage
}

// chopsUsageFlagLine returns the usage block for one flag: its `  -name`
// line plus the indented description beneath it, or "" when the flag is
// not declared.
func chopsUsageFlagLine(usage, name string) string {
	lines := strings.Split(usage, "\n")
	for i, line := range lines {
		if strings.TrimRight(line, " ") != "  -"+name {
			continue
		}
		block := line
		for _, next := range lines[i+1:] {
			if !strings.HasPrefix(next, "    	") && !strings.HasPrefix(next, "\t") {
				break
			}
			block += "\n" + next
		}
		return block
	}
	return ""
}
