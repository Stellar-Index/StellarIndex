// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// rollupWriteGatedSubcommands are the chops ClickHouse writers that carry
// the shared opsutil.WriteGate. The two rollups (F079) declared no
// -write/-dry-run pair and wrote unconditionally; ch-backfill (#868)
// wrote unless -dry-run was passed.
var rollupWriteGatedSubcommands = []string{
	"ch-census-rollup",
	"ch-holders-rollup",
	"ch-backfill",
}

// TestRollupSubcommandsRegisterTheSharedWriteGate drives each subcommand
// through the real dispatch entry point with -h and reads the flag set it
// declares off its own usage output.
func TestRollupSubcommandsRegisterTheSharedWriteGate(t *testing.T) {
	for _, verb := range rollupWriteGatedSubcommands {
		usage := rollupSubcommandUsage(t, verb)
		writeLine := rollupUsageFlagLine(usage, "write")
		switch {
		case writeLine == "":
			t.Errorf("%s declares no -write flag: it writes to ClickHouse unconditionally on every run (F079)", verb)
		case !strings.Contains(writeLine, "fail-closed DRY RUN"):
			t.Errorf("%s declares a -write flag that is not the shared opsutil gate. Got:\n%s", verb, writeLine)
		}
		dryRunLine := rollupUsageFlagLine(usage, "dry-run")
		if dryRunLine == "" || !strings.Contains(dryRunLine, "the DEFAULT") {
			t.Errorf("%s does not declare the shared -dry-run no-op alias with dry run as the DEFAULT. Got:\n%s", verb, dryRunLine)
		}
	}
}

// TestChCensusRollupDryRunTouchesNoClickHouse proves the behavioural half:
// against an unreachable ClickHouse address, the default (no -write) run
// must return nil — it never dials out — while -write must fail trying to
// connect. -from-day sidesteps the incremental path's own CensusMaxDay
// read so the loop body is the only thing under test.
func TestChCensusRollupDryRunTouchesNoClickHouse(t *testing.T) {
	unreachable := "127.0.0.1:1" // reserved port, nothing ever listens here
	today := time.Now().UTC().Format("2006-01-02")

	if err := Run([]string{"ch-census-rollup", "-ch-addr", unreachable, "-from-day", today}); err != nil {
		t.Errorf("dry run (no -write) returned %v, want nil — it must not dial ClickHouse at all", err)
	}

	err := Run([]string{"ch-census-rollup", "-ch-addr", unreachable, "-from-day", today, "-write"})
	if err == nil {
		t.Fatal("-write against an unreachable address returned nil — the write path was never exercised")
	}
	if !strings.Contains(err.Error(), "clickhouse") {
		t.Errorf("-write failed for an unexpected reason (want a ClickHouse connect error): %v", err)
	}
}

// TestChHoldersRollupDryRunTouchesNoClickHouse mirrors the census case: a
// dry run must neither take the advisory lock nor dial ClickHouse, and
// -write must fail against an unreachable address.
func TestChHoldersRollupDryRunTouchesNoClickHouse(t *testing.T) {
	unreachable := "127.0.0.1:1"
	lockPath := filepath.Join(t.TempDir(), "ch-holders-rollup.lock")

	if err := Run([]string{"ch-holders-rollup", "-ch-addr", unreachable, "-lock-file", lockPath}); err != nil {
		t.Errorf("dry run (no -write) returned %v, want nil — it must not dial ClickHouse at all", err)
	}

	err := Run([]string{"ch-holders-rollup", "-ch-addr", unreachable, "-lock-file", lockPath, "-write"})
	if err == nil {
		t.Fatal("-write against an unreachable address returned nil — the write path was never exercised")
	}
	if !strings.Contains(err.Error(), "clickhouse") {
		t.Errorf("-write failed for an unexpected reason (want a ClickHouse connect error): %v", err)
	}
}

// rollupSubcommandUsage runs `<verb> -h` through the package's real
// dispatch function and returns what it printed. flag.ContinueOnError
// makes -h print the defaults and return flag.ErrHelp from Parse, before
// the handler touches a config file or a database.
func rollupSubcommandUsage(t *testing.T, verb string) string {
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

// rollupUsageFlagLine returns the usage block for one flag: its `  -name`
// line plus the indented description beneath it, or "" when the flag is
// not declared.
func rollupUsageFlagLine(usage, name string) string {
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
