// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

// F075's last leg. scripts/ops/ch-rebuild-projected.sh knew when it had left
// a window EMPTY and printed the command that tells the ADR-0033
// completeness verdict about it — but it did not run the command, so the
// obligation existed only for as long as it took an operator to read
// /var/log/ch-rebuild-projected.log. Between a failed re-derive and that
// reading, /v1/coverage kept certifying a range with no rows in it
// complete=true.
//
// These execute the shipped script (harness in
// ch_rebuild_projected_script_test.go) and pin the filing itself: every
// state that leaves a window emptied FILES one projection dirty window for
// exactly the deleted sources, a state that empties nothing files none
// (#408), and a filing that fails is loud rather than silent.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assertFiledWindow checks the one -record-dirty-window invocation the
// script made names the emptied range and exactly the deleted sources. A
// record for the wrong window files the wrong obligation, and one carrying
// -write is refused by the binary (checkCHRebuildRecordDirtyFlags).
func assertFiledWindow(t *testing.T, run scriptRun, lo, hi, sources string) {
	t.Helper()
	recs := run.records()
	if len(recs) != 1 {
		t.Fatalf("the script left [%s,%s] EMPTY and filed %d projection dirty window(s), want 1 — "+
			"until one is filed /v1/coverage carries its prior clean claim over the hole (trace: %s)\n%s",
			lo, hi, len(recs), run.sequence(), run.log)
	}
	rec := recs[0]
	if rec.flag("-from") != lo || rec.flag("-to") != hi || rec.flag("-sources") != sources {
		t.Errorf("filed window = [%s,%s] sources=%s, want [%s,%s] sources=%s",
			rec.flag("-from"), rec.flag("-to"), rec.flag("-sources"), lo, hi, sources)
	}
	if rec.has("-write") || rec.has("-preflight") {
		t.Errorf("the record invocation carries -write/-preflight (%q) — the binary refuses either pairing", rec.args)
	}
	if rec.flag("-config") == "" {
		t.Errorf("the record invocation names no -config (%q), so it cannot reach Postgres", rec.args)
	}
}

// The finding's second scenario, at its last step: the DELETE landed, the
// re-derive died, the window is EMPTY — and the verdict is now told so by
// the run that emptied it, not by whoever reads the log next.
func TestChRebuildProjectedScript_FailedRederiveFilesTheEmptiedWindow(t *testing.T) {
	t.Parallel()
	run := runProjectedScript(t, "", map[string]string{
		"SRC":                  "soroswap,cctp",
		"STUB_FAIL_WRITE_FROM": "61000000",
	})
	if run.exit != 1 {
		t.Fatalf("exit = %d, want 1 (the re-derive failed)\n%s", run.exit, run.log)
	}
	if !strings.Contains(run.log, "LEFT EMPTIED") {
		t.Fatalf("fixture: the run did not report an emptied window\n%s", run.log)
	}
	// The filing follows the DELETE and the failed re-derive: it records
	// what happened, so it cannot precede it.
	if got, want := run.sequence(), "preflight@61000000 psql write@61000000 record@61000000"; got != want {
		t.Errorf("call trace = %q, want %q", got, want)
	}
	assertFiledWindow(t, run, "61000000", "61999999", "soroswap,cctp")
}

// A failure ON the COMMIT deleted the rows and the script cannot tell, so
// the range is filed there too: a spurious window costs one re-reconcile,
// the other way round costs a certified hole.
func TestChRebuildProjectedScript_AmbiguousDeleteFilesTheEmptiedWindow(t *testing.T) {
	t.Parallel()
	run := runProjectedScript(t, "", map[string]string{"SRC": "cctp", "STUB_PSQL_RC": "3"})
	if run.exit != 1 {
		t.Fatalf("exit = %d, want 1 (the DELETE failed)\n%s", run.exit, run.log)
	}
	if n := len(run.writes()); n != 0 {
		t.Errorf("%d re-derive(s) ran after a failed DELETE (trace %q)", n, run.sequence())
	}
	assertFiledWindow(t, run, "61000000", "61999999", "cctp")
}

// The gap no in-process handler can cover: the box died between the DELETE
// and the filing, so only $DIRTY survived. The next run files that window
// BEFORE it tries to recover it — the recovery can fail too.
func TestChRebuildProjectedScript_PendingDirtyWindowIsFiledBeforeRecovery(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	state := filepath.Join(dir, "state", "rebuild-done-windows.txt")
	if err := os.MkdirAll(filepath.Dir(state), 0o755); err != nil { //nolint:gosec // test temp dir
		t.Fatal(err)
	}
	if err := os.WriteFile(state+".dirty", []byte("60000000 60999999 cctp\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := runProjectedScript(t, dir, map[string]string{"SRC": "cctp"})
	if run.exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s", run.exit, run.log)
	}
	assertFiledWindow(t, run, "60000000", "60999999", "cctp")
	if got, want := run.sequence(), "record@60000000 preflight@60000000 psql write@60000000 preflight@61000000 psql write@61000000"; got != want {
		t.Errorf("call trace = %q, want %q (the filing must precede the recovery it describes)", got, want)
	}
	// F072's contract: only the verdict that discharges the obligation may
	// retract it. A successful recovery clears the local marker, nothing else.
	if strings.TrimSpace(run.dirty) != "" {
		t.Errorf("$DIRTY after a successful recovery = %q, want empty", run.dirty)
	}
}

// #408's measured decision, on the filing side: a window that re-derived
// cleanly empties nothing, so it files nothing. One obligation per routine
// window would point the next nightly at ~12.9M ledgers across 8
// un-prefiltered sources and time every source's verdict out.
func TestChRebuildProjectedScript_CleanWindowFilesNothing(t *testing.T) {
	t.Parallel()
	run := runProjectedScript(t, "", map[string]string{"SRC": "cctp"})
	if run.exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s", run.exit, run.log)
	}
	if n := len(run.writes()); n != 1 {
		t.Fatalf("fixture: %d re-derive(s), want the one clean window (trace %q)", n, run.sequence())
	}
	if recs := run.records(); len(recs) != 0 {
		t.Errorf("a clean window filed %d projection dirty window(s) (trace %q) — the next nightly would re-reconcile a range nothing emptied",
			len(recs), run.sequence())
	}
}

// The filing can itself fail (Postgres down, binary missing). That is the
// one case where the printed command is all the operator has, so the run
// must say so instead of exiting on the strength of a record it never made.
func TestChRebuildProjectedScript_FilingFailureIsLoudAndKeepsTheCommand(t *testing.T) {
	t.Parallel()
	run := runProjectedScript(t, "", map[string]string{
		"SRC":                  "cctp",
		"STUB_FAIL_WRITE_FROM": "61000000",
		"STUB_FAIL_RECORD":     "61000000",
	})
	if run.exit != 1 {
		t.Fatalf("exit = %d, want 1\n%s", run.exit, run.log)
	}
	if !strings.Contains(run.log, "COULD NOT FILE") {
		t.Errorf("the log does not say the filing failed, so the hole looks filed:\n%s", run.log)
	}
	cmd := recordCommandFor(run.log, "61000000", "61999999")
	if cmd == "" {
		t.Fatalf("no record command left in the log for the operator to re-run\n%s", run.log)
	}
	assertRecordCommand(t, cmd, "61000000", "61999999", "cctp")
	if strings.TrimSpace(run.dirty) == "" {
		t.Error("$DIRTY was cleared although the window is emptied and unfiled")
	}
}
