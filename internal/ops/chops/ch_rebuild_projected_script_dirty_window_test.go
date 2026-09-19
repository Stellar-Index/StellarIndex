// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

// An emptied window has TWO records to keep, and scripts/ops/ch-rebuild-
// projected.sh used to keep only one (F075):
//
//   - $DIRTY, the local marker that drives this script's own recovery. It
//     must be WRITTEN before anything is deleted — a failed append may not
//     be shrugged off, or the DELETE proceeds with nothing left to rebuild
//     the window.
//   - the ADR-0033 projection dirty window, which is what the completeness
//     verdict reads. Nothing on this box's filesystem reaches it, so until
//     the emptied range is filed there, /v1/coverage keeps carrying its
//     prior clean claim over a hole.
//
// These tests EXECUTE the shipped script (the harness in
// ch_rebuild_projected_script_test.go) and pin both, plus the case that
// must NOT change: a window that re-derived cleanly files nothing (#408 —
// filing one per routine window would point the next nightly at ~12.9M
// ledgers across 8 sources and time every verdict out).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// verdictNoteRE-ish: the note is matched as plain text on purpose — it is
// what an operator greps for in /var/log/ch-rebuild-projected.log.
const verdictNoteHead = "TELL THE VERDICT"

// recordCommandFor returns the `-record-dirty-window` command line the
// script printed for [lo,hi], or "" if it printed none.
func recordCommandFor(log, lo, hi string) string {
	var seen bool
	for _, line := range strings.Split(log, "\n") {
		switch {
		case strings.HasPrefix(line, verdictNoteHead+" ["+lo+","+hi+"]"):
			seen = true
		case seen && strings.Contains(line, "-record-dirty-window"):
			return strings.TrimSpace(line)
		case seen && strings.HasPrefix(line, verdictNoteHead):
			seen = false // a different window's note; keep looking
		}
	}
	return ""
}

// assertRecordCommand checks the printed command names the emptied range
// and exactly the deleted sources — a note that points at the wrong window
// files the wrong obligation.
func assertRecordCommand(t *testing.T, cmd, lo, hi, sources string) {
	t.Helper()
	for _, want := range []string{
		"ch-rebuild", "-from " + lo, "-to " + hi, "-sources " + sources, "-record-dirty-window",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("printed record command %q does not carry %q", cmd, want)
		}
	}
	if strings.Contains(cmd, "-write") {
		t.Errorf("printed record command %q carries -write — the record is not a re-derive, and the binary refuses the pair", cmd)
	}
}

// TestChRebuildProjectedScript_EmptiedWindowTellsTheVerdictHow is the
// completeness-verdict leg of F075: the DELETE landed, the re-derive died,
// so the window is EMPTY. $DIRTY records that for the next run of this
// script; nothing recorded it for /v1/coverage, which kept certifying the
// hole complete=true until someone noticed.
func TestChRebuildProjectedScript_EmptiedWindowTellsTheVerdictHow(t *testing.T) {
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
	cmd := recordCommandFor(run.log, "61000000", "61999999")
	if cmd == "" {
		t.Fatalf("the window [61000000,61999999] was left EMPTY and the log says nothing about the completeness verdict — "+
			"it keeps carrying its prior clean claim over the hole and /v1/coverage certifies it complete (F075)\n%s", run.log)
	}
	assertRecordCommand(t, cmd, "61000000", "61999999", "soroswap,cctp")
}

// A failure ON the COMMIT deleted the rows and the script cannot tell, so
// the note is printed there too: a spurious window costs one re-reconcile,
// the other way round costs a certified hole.
func TestChRebuildProjectedScript_AmbiguousDeleteTellsTheVerdictHow(t *testing.T) {
	t.Parallel()
	run := runProjectedScript(t, "", map[string]string{"SRC": "cctp", "STUB_PSQL_RC": "3"})
	if run.exit != 1 {
		t.Fatalf("exit = %d, want 1 (the DELETE failed)\n%s", run.exit, run.log)
	}
	cmd := recordCommandFor(run.log, "61000000", "61999999")
	if cmd == "" {
		t.Fatalf("a DELETE that may have COMMITted says nothing about the completeness verdict\n%s", run.log)
	}
	assertRecordCommand(t, cmd, "61000000", "61999999", "cctp")
}

// The gap no in-process handler can cover: the box died (kill -9, power
// loss) between the DELETE and the re-derive, so only $DIRTY survived. The
// next run must surface that window's obligation BEFORE it starts
// recovering — the recovery can fail too.
func TestChRebuildProjectedScript_PendingDirtyWindowTellsTheVerdictAtStartup(t *testing.T) {
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
	cmd := recordCommandFor(run.log, "60000000", "60999999")
	if cmd == "" {
		t.Fatalf("a window an earlier run left EMPTY was recovered without the log ever naming the obligation it owes the verdict\n%s", run.log)
	}
	assertRecordCommand(t, cmd, "60000000", "60999999", "cctp")
	// Before the recovery it precedes — which may itself fail.
	if noteAt, recoverAt := strings.Index(run.log, verdictNoteHead), strings.Index(run.log, "PREFLIGHT sources=cctp"); noteAt < 0 || recoverAt < 0 || noteAt > recoverAt {
		t.Errorf("the note (at %d) must precede the recovery attempt (at %d)", noteAt, recoverAt)
	}
	// The script never retracts the obligation: only the verdict that
	// discharges it may (F072). A successful recovery clears the local
	// marker and nothing else.
	if strings.TrimSpace(run.dirty) != "" {
		t.Errorf("$DIRTY after a successful recovery = %q, want empty", run.dirty)
	}
}

// #408's measured decision, which this fix deliberately does NOT reverse.
func TestChRebuildProjectedScript_CleanWindowTellsTheVerdictNothing(t *testing.T) {
	t.Parallel()
	run := runProjectedScript(t, "", map[string]string{"SRC": "cctp"})
	if run.exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s", run.exit, run.log)
	}
	if strings.Contains(run.log, verdictNoteHead) {
		t.Errorf("a clean window asked for a projection dirty window (#408: the nightly re-reconcile of ~12.9M ledgers × 8 sources would time out EVERY verdict)\n%s", run.log)
	}
}

// TestChRebuildProjectedScript_PrintedRecordCommandIsAcceptedByTheBinary
// closes the drift the note would otherwise invite: a command an operator
// pastes at 03:00 and that the binary rejects is worse than no note. It
// feeds the printed flags to the REAL chRebuild entry point and requires
// the failure to be the missing config, never an unknown flag.
func TestChRebuildProjectedScript_PrintedRecordCommandIsAcceptedByTheBinary(t *testing.T) {
	t.Parallel()
	run := runProjectedScript(t, "", map[string]string{"SRC": "cctp", "STUB_FAIL_WRITE_FROM": "61000000"})
	cmd := recordCommandFor(run.log, "61000000", "61999999")
	if cmd == "" {
		t.Fatalf("fixture: no record command was printed\n%s", run.log)
	}
	fields := strings.Fields(cmd)
	if len(fields) < 3 || fields[1] != "ch-rebuild" {
		t.Fatalf("printed command %q is not `<binary> ch-rebuild ...`", cmd)
	}
	err := chRebuild(fields[2:])
	if err == nil {
		t.Fatalf("chRebuild(%v) succeeded against a config that does not exist", fields[2:])
	}
	if strings.Contains(err.Error(), "not defined") || strings.Contains(err.Error(), "-record-dirty-window records") ||
		strings.Contains(err.Error(), "-record-dirty-window and -preflight") || strings.Contains(err.Error(), "needs -sources") {
		t.Fatalf("the binary rejected the command the script tells operators to run: %v", err)
	}
}

// ── the local marker: rule 3 is only a rule if the write is CHECKED ──────

// TestChRebuildProjectedScript_UnusableDirtyMarkerRefusesBeforeAnyDelete:
// an unwritable/missing state directory used to be silent — every append to
// $DIRTY failed and the DELETE ran anyway, so the window was emptied with
// no record that would ever rebuild it.
func TestChRebuildProjectedScript_UnusableDirtyMarkerRefusesBeforeAnyDelete(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	run := runProjectedScript(t, dir, map[string]string{
		"SRC":   "cctp",
		"DIRTY": filepath.Join(dir, "no-such-dir", "rebuild.dirty"),
	})
	if run.exit != 2 {
		t.Errorf("exit = %d, want 2 (a refusal before anything was touched)\n%s", run.exit, run.log)
	}
	if len(run.calls) != 0 {
		t.Errorf("the run reached %d call(s) with no usable dirty marker: %s", len(run.calls), run.sequence())
	}
	if !strings.Contains(run.log, "REFUSED") {
		t.Errorf("the log does not say the run was refused:\n%s", run.log)
	}
}

// TestChRebuildProjectedScript_DirtyMarkerThatFailsMidRunStopsBeforeTheDelete
// is the same defect one step later: the state directory is fine at startup
// and gone by the time the window is marked (a cleanup job, a full disk).
// mark_dirty's exit code was not checked, so the DELETE went ahead.
func TestChRebuildProjectedScript_DirtyMarkerThatFailsMidRunStopsBeforeTheDelete(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(stateDir, 0o755); err != nil { //nolint:gosec // test temp dir
		t.Fatal(err)
	}
	// An ops stub that answers the preflight and, in the same breath,
	// takes the state directory away — the marker is written after the
	// preflight and before the DELETE.
	ops := filepath.Join(dir, "ops-that-wipes-the-state-dir")
	body := "#!/usr/bin/env bash\n" +
		"printf 'OPS %s\\n' \"$*\" >> \"$STUB_CALLS\"\n" +
		"from=\"\"; to=\"\"; srcs=\"\"; pre=0\n" +
		"while [ $# -gt 0 ]; do case \"$1\" in -from) from=\"$2\"; shift ;; -to) to=\"$2\"; shift ;; -sources) srcs=\"$2\"; shift ;; -preflight) pre=1 ;; esac; shift; done\n" +
		"if [ \"$pre\" = 1 ]; then rm -rf \"$WIPE_DIR\"; echo \"$STUB_PREFLIGHT_PREFIX [$from,$to] rederive=$srcs\"; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(ops, []byte(body), 0o755); err != nil { //nolint:gosec // an executable test stub
		t.Fatal(err)
	}
	run := runProjectedScript(t, dir, map[string]string{
		"SRC":      "cctp",
		"OPS":      ops,
		"STATE":    filepath.Join(stateDir, "rebuild-done-windows.txt"),
		"WIPE_DIR": stateDir,
	})
	if run.exit != 1 {
		t.Errorf("exit = %d, want 1 (the marker could not be written)\n%s", run.exit, run.log)
	}
	if n := len(run.deletes()); n != 0 {
		t.Errorf("%d DELETE batch(es) ran although the emptied window could not be recorded — nothing would rebuild the window (trace %q)",
			n, run.sequence())
	}
	if !strings.Contains(run.log, "CANNOT RECORD") {
		t.Errorf("the log does not name the unwritable marker:\n%s", run.log)
	}
}

// TestChRebuildProjectedScript_FilesTheEmptiedWindowItself is the leg of
// F075 this unit could NOT land: the script prints the record command but
// does not run it, so the obligation is only filed if an operator reads the
// log. Wiring the call adds one `$OPS ... -record-dirty-window` invocation
// per emptied window, which breaks two exact call-trace assertions in files
// outside this unit's scope fence —
// internal/ops/chops/ch_rebuild_projected_script_scope_test.go:251,268
// (sequence() renders the new call as `write@…`) — and wants a `record@`
// case in scriptRun.sequence(), in
// internal/ops/chops/ch_rebuild_projected_script_test.go. Unskip together
// with that change; the assertions below are what the wiring must satisfy.
func TestChRebuildProjectedScript_FilesTheEmptiedWindowItself(t *testing.T) {
	t.Skip("NEEDS-COORDINATION: wiring the call requires ch_rebuild_projected_script{,_scope}_test.go, outside this unit's fence")
	run := runProjectedScript(t, "", map[string]string{
		"SRC":                  "soroswap,cctp",
		"STUB_FAIL_WRITE_FROM": "61000000",
	})
	var records []scriptCall
	for _, c := range run.calls {
		if c.kind == "OPS" && c.has("-record-dirty-window") {
			records = append(records, c)
		}
	}
	if len(records) != 1 {
		t.Fatalf("the script left [61000000,61999999] EMPTY and filed %d projection dirty window(s), want 1", len(records))
	}
	rec := records[0]
	if rec.flag("-from") != "61000000" || rec.flag("-to") != "61999999" || rec.flag("-sources") != "soroswap,cctp" {
		t.Errorf("filed window = [%s,%s] sources=%s, want [61000000,61999999] sources=soroswap,cctp",
			rec.flag("-from"), rec.flag("-to"), rec.flag("-sources"))
	}
	if rec.has("-write") {
		t.Errorf("the record invocation must not carry -write: %q", rec.args)
	}
}
