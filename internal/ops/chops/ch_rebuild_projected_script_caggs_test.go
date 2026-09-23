// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #782: the script rewrites `trades` over a range years behind tip, and no
// trades continuous aggregate's refresh policy looks back that far. Without
// an explicit refresh the served OHLC / volume / TWAP keep the pre-repair
// rows for good. These tests EXECUTE the shipped script (see
// ch_rebuild_projected_script_test.go for the stubs).

func (r scriptRun) caggRefreshes() []scriptCall {
	var out []scriptCall
	for _, c := range r.calls {
		if c.isCAGGRefresh() {
			out = append(out, c)
		}
	}
	return out
}

func staleCAGGFile(dir string) string {
	return filepath.Join(dir, "state", "rebuild-done-windows.txt.caggs")
}

func TestChRebuildProjectedScript_RefreshesTradesCAGGsAfterTheRederive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	run := runProjectedScript(t, dir, map[string]string{"SRC": "soroswap,cctp"})
	if run.exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s", run.exit, run.log)
	}
	if got, want := run.sequence(), "preflight@61000000 psql write@61000000 refresh@61000000"; got != want {
		t.Fatalf("call trace = %q, want %q — the window's trades were rewritten and no CAGG was refreshed over them", got, want)
	}
	rf := run.caggRefreshes()[0]
	if rf.flag("-from") != "61000000" || rf.flag("-to") != "61999999" || rf.flag("-config") == "" {
		t.Errorf("refresh call = %q, want -config CFG -from 61000000 -to 61999999", rf.args)
	}
	if rf.has("-write") || rf.has("-sources") {
		t.Errorf("refresh call carries re-derive flags: %q", rf.args)
	}
	if got := strings.TrimSpace(readIfExists(t, staleCAGGFile(dir))); got != "" {
		t.Errorf("stale-CAGG record after a successful refresh = %q, want empty", got)
	}
}

// Nothing in `trades` moved, so there is nothing to re-materialise.
func TestChRebuildProjectedScript_NoTradeSourceNoRefresh(t *testing.T) {
	t.Parallel()
	run := runProjectedScript(t, "", map[string]string{"SRC": "cctp,blend"})
	if run.exit != 0 {
		t.Fatalf("exit = %d, want 0\n%s", run.exit, run.log)
	}
	if n := len(run.caggRefreshes()); n != 0 {
		t.Errorf("%d CAGG refresh(es) for a window whose trades were never touched (trace %q)", n, run.sequence())
	}
}

// A failed refresh leaves trades correct and every aggregate over them
// stale. It must be loud, recorded, and done FIRST by the next run —
// even one whose range no longer covers the window, and even though the
// window itself is marked done.
func TestChRebuildProjectedScript_FailedRefreshIsRecordedAndRetriedFirst(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	failed := runProjectedScript(t, dir, map[string]string{"SRC": "soroswap", "STUB_FAIL_REFRESH_FROM": "61000000"})
	if failed.exit == 0 {
		t.Fatalf("exit 0 although the CAGG refresh failed\n%s", failed.log)
	}
	if !strings.Contains(failed.log, "CAGG REFRESH FAILED [61000000,61999999]") {
		t.Errorf("the log does not name the stale window:\n%s", failed.log)
	}
	if got, want := strings.TrimSpace(readIfExists(t, staleCAGGFile(dir))), "61000000 61999999"; got != want {
		t.Fatalf("stale-CAGG record = %q, want %q", got, want)
	}
	if strings.TrimSpace(failed.dirty) != "" {
		t.Errorf("$DIRTY = %q — the window was re-derived, it is not emptied", failed.dirty)
	}
	if n := len(failed.records()); n != 0 {
		t.Errorf("filed %d projection dirty window(s) for a window that is not emptied", n)
	}

	next := runProjectedScript(t, dir, map[string]string{"SRC": "soroswap", "FROM": "62000000", "TO": "62999999"})
	if next.exit != 0 {
		t.Fatalf("recovery run exit = %d\n%s", next.exit, next.log)
	}
	if got, want := next.sequence(), "refresh@61000000 preflight@62000000 psql write@62000000 refresh@62000000"; got != want {
		t.Errorf("recovery trace = %q, want %q (the owed refresh goes first)", got, want)
	}
	if got := strings.TrimSpace(readIfExists(t, staleCAGGFile(dir))); got != "" {
		t.Errorf("stale-CAGG record after the retry = %q, want empty", got)
	}
}

// The re-derive dies after the DELETE: trades are gone from the aggregates'
// point of view too. The obligation is recorded before the DELETE, so the
// recovery that re-derives the window also refreshes it.
func TestChRebuildProjectedScript_EmptiedWindowRecoveryRefreshesTheCAGGs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	failed := runProjectedScript(t, dir, map[string]string{"SRC": "soroswap", "STUB_FAIL_WRITE_FROM": "61000000"})
	if failed.exit == 0 {
		t.Fatalf("exit 0 although the re-derive failed\n%s", failed.log)
	}
	if got, want := strings.TrimSpace(readIfExists(t, staleCAGGFile(dir))), "61000000 61999999"; got != want {
		t.Fatalf("stale-CAGG record after a failed re-derive = %q, want %q", got, want)
	}
	next := runProjectedScript(t, dir, map[string]string{"SRC": "soroswap"})
	if next.exit != 0 {
		t.Fatalf("recovery run exit = %d\n%s", next.exit, next.log)
	}
	if got, want := next.sequence(), "record@61000000 preflight@61000000 psql write@61000000 refresh@61000000"; got != want {
		t.Errorf("recovery trace = %q, want %q", got, want)
	}
}

// Rule 3's discipline applies to the new record: an unwritable one deletes
// nothing, or a failed refresh would be forgotten.
func TestChRebuildProjectedScript_UnusableStaleRecordRefusesBeforeAnyDelete(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(staleCAGGFile(dir), 0o750); err != nil {
		t.Fatal(err)
	}
	run := runProjectedScript(t, dir, map[string]string{"SRC": "soroswap"})
	if run.exit != 2 {
		t.Fatalf("exit = %d, want 2\n%s", run.exit, run.log)
	}
	if n := len(run.deletes()); n != 0 {
		t.Errorf("%d DELETE batch(es) ran with no usable stale-CAGG record", n)
	}
}
