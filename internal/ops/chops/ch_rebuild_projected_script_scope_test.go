// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

// scripts/ops/ch-rebuild-projected.sh deleted a HARD-CODED table set
// whatever -sources it then passed, keyed its done-state by window alone,
// and recorded nothing when the re-derive failed after the DELETE. So a
// narrowed run emptied eleven tables it never rebuilt, marked the window
// done for every source, and a failed run left a hole that a later
// narrowed run then certified.
//
// These execute the shipped script (harness in
// ch_rebuild_projected_script_test.go) and hold it to one rule: a table is
// emptied only for a source the SAME window re-derives, and a window that
// was emptied and not rebuilt is never forgotten.

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
)

// scriptDefaultSources is the script's SRC default. It is asserted against
// the script's own text below, so this copy cannot drift silently.
const scriptDefaultSources = "aquarius,soroswap,phoenix,comet,blend,cctp,rozo,defindex"

// scriptKnownSources parses the script's KNOWN_SOURCES so the ownership
// tests cover every source it will delete for, not only the SRC default.
func scriptKnownSources(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "scripts", "ops", "ch-rebuild-projected.sh"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^KNOWN_SOURCES="([^"]*)"`).FindSubmatch(b)
	if m == nil {
		t.Fatal("KNOWN_SOURCES not found in ch-rebuild-projected.sh")
	}
	return strings.Fields(string(m[1]))
}

var deleteTableRE = regexp.MustCompile(`DELETE FROM ([a-z0-9_]+)`)

// tablesDeleted returns the distinct tables one DELETE batch names, except
// that `trades` is reported per source ("trades:soroswap") since the script
// shares that table between sources and scopes it by the source column.
func tablesDeleted(sql string) []string {
	seen := map[string]bool{}
	for _, m := range deleteTableRE.FindAllStringSubmatch(sql, -1) {
		if m[1] != "trades" {
			seen[m[1]] = true
		}
	}
	for _, src := range tradeSourcesDeleted(sql) {
		seen["trades:"+src] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestChRebuildProjectedScript_DefaultSourcesConstantMatchesTheScript(t *testing.T) {
	t.Parallel()
	body := readIfExists(t, projectedScriptPath)
	if !strings.Contains(body, `SRC=${SRC:-"`+scriptDefaultSources+`"}`) {
		t.Fatalf("the script's SRC default is no longer %q — update scriptDefaultSources with it", scriptDefaultSources)
	}
}

// The narrowed run is the finding's first scenario, tested WITH its
// trigger: the destructive branch must still fire for the named source
// (soroswap's own tables ARE deleted) and for nothing else.
func TestChRebuildProjectedScript_NarrowedRunDeletesOnlyItsOwnTables(t *testing.T) {
	t.Parallel()
	run := runProjectedScript(t, "", map[string]string{"SRC": "soroswap"})
	if run.exit != 0 {
		t.Fatalf("exit %d\n%s", run.exit, run.log)
	}
	dels := run.deletes()
	if len(dels) != 1 {
		t.Fatalf("want one DELETE batch, got %d\n%s", len(dels), run.log)
	}
	got := strings.Join(tablesDeleted(dels[0].stdin), " ")
	if want := "soroswap_liquidity soroswap_skim_events trades:soroswap"; got != want {
		t.Errorf("SRC=soroswap deleted %q, want exactly %q — every other table was emptied for a source this run never re-derives", got, want)
	}
	if srcs := run.writes()[0].flag("-sources"); srcs != "soroswap" {
		t.Errorf("-write was asked to re-derive %q, want soroswap", srcs)
	}
}

// Ownership is not the script's to assert: the reconciliation catalogue
// says which source re-derives which table, and whether it owns the whole
// table. An unfiltered DELETE on a table the catalogue shares between
// sources, or gives to a different source, empties rows the run will not
// rewrite.
func TestChRebuildProjectedScript_EachSourceDeletesOnlyTablesTheCatalogueSaysItOwns(t *testing.T) {
	t.Parallel()
	cat, _, err := buildReconciliationCatalogue(config.Config{})
	if err != nil {
		t.Fatalf("catalogue: %v", err)
	}
	owns := map[string]map[string]string{} // source → table → whereFilter
	for _, src := range cat {
		owns[src.name] = map[string]string{}
		for _, tg := range src.targets {
			owns[src.name][tg.table] = tg.whereFilter
		}
	}
	for _, source := range scriptKnownSources(t) {
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			run := runProjectedScript(t, "", map[string]string{"SRC": source})
			if run.exit != 0 || len(run.deletes()) != 1 {
				t.Fatalf("exit %d, %d DELETE batches\n%s", run.exit, len(run.deletes()), run.log)
			}
			tables := tablesDeleted(run.deletes()[0].stdin)
			if len(tables) == 0 {
				t.Fatalf("SRC=%s deleted nothing — the clean-slate repair is a no-op for it, and this test asserted nothing", source)
			}
			for _, tbl := range tables {
				if name, isTrades := strings.CutPrefix(tbl, "trades:"); isTrades {
					if name != source {
						t.Errorf("SRC=%s deletes trades for %q", source, name)
					}
					if f := owns[source]["trades"]; f != "source = '"+source+"'" {
						t.Errorf("SRC=%s deletes its trades, but the catalogue's trades target for it is filtered %q", source, f)
					}
					continue
				}
				filter, ok := owns[source][tbl]
				switch {
				case !ok:
					t.Errorf("SRC=%s deletes %s, which the catalogue does not list as a table %s re-derives", source, tbl, source)
				case filter != "":
					t.Errorf("SRC=%s deletes ALL of %s, but the catalogue says %s owns only the rows matching %q", source, tbl, source, filter)
				}
			}
		})
	}
}

// The reverse of the containment test above: the catalogue is the ceiling
// AND the floor. A table the catalogue says a source owns wholesale
// (whereFilter == "") must be emptied by that source's window, or the
// clean-slate re-derive that follows leaves the catalogue's own rows
// stale forever — the hand-maintained case statement falling behind the
// catalogue in the other direction from the one the containment test
// guards.
func TestChRebuildProjectedScript_EachSourceDeletesEveryTableTheCatalogueSaysItOwnsWholesale(t *testing.T) {
	t.Parallel()
	cat, _, err := buildReconciliationCatalogue(config.Config{})
	if err != nil {
		t.Fatalf("catalogue: %v", err)
	}
	wholesale := map[string][]string{} // source → tables owned outright
	for _, src := range cat {
		for _, tg := range src.targets {
			if tg.table == "trades" || tg.whereFilter != "" {
				continue
			}
			wholesale[src.name] = append(wholesale[src.name], tg.table)
		}
	}
	for _, source := range scriptKnownSources(t) {
		want := wholesale[source]
		if len(want) == 0 {
			continue
		}
		t.Run(source, func(t *testing.T) {
			t.Parallel()
			run := runProjectedScript(t, "", map[string]string{"SRC": source})
			if run.exit != 0 || len(run.deletes()) != 1 {
				t.Fatalf("exit %d, %d DELETE batches\n%s", run.exit, len(run.deletes()), run.log)
			}
			deleted := map[string]bool{}
			for _, tbl := range tablesDeleted(run.deletes()[0].stdin) {
				deleted[tbl] = true
			}
			for _, tbl := range want {
				if !deleted[tbl] {
					t.Errorf("SRC=%s never deletes %s, which the catalogue lists as wholly owned by it — the re-derive that follows leaves it stale", source, tbl)
				}
			}
		})
	}
}

// The DELETE set comes from what the binary says it WILL re-derive, not
// from what was asked for: a source the config cannot resolve is in the
// request and not in the run.
func TestChRebuildProjectedScript_DeleteSetFollowsThePreflightVerdict(t *testing.T) {
	t.Parallel()
	run := runProjectedScript(t, "", map[string]string{"STUB_REDERIVE": "soroswap,cctp"})
	if run.exit != 0 {
		t.Fatalf("exit %d\n%s", run.exit, run.log)
	}
	got := strings.Join(tablesDeleted(run.deletes()[0].stdin), " ")
	if want := "cctp_events soroswap_liquidity soroswap_skim_events trades:soroswap"; got != want {
		t.Errorf("deleted %q, want %q (the verdict named only soroswap,cctp)", got, want)
	}
	if srcs := run.writes()[0].flag("-sources"); srcs != "soroswap,cctp" {
		t.Errorf("-write re-derives %q, want the verdict's soroswap,cctp", srcs)
	}
	for _, src := range strings.Split(scriptDefaultSources, ",") {
		done := strings.Contains(run.state, src+" 61000000 61999999\n")
		if want := src == "soroswap" || src == "cctp"; done != want {
			t.Errorf("%s recorded done=%v, want %v\nstate:\n%s", src, done, want, run.state)
		}
	}
}

// Anything the script cannot clean-slate correctly it refuses BEFORE the
// first call to psql or the binary.
func TestChRebuildProjectedScript_RefusesWhatItCannotCleanSlate(t *testing.T) {
	t.Parallel()
	for name, env := range map[string]map[string]string{
		"a source with no DELETE map":         {"SRC": "soroswap,reflector-dex"},
		"the unaudited source":                {"SRC": "soroswap,sushiswap_v3"},
		"SQL in SRC":                          {"SRC": "soroswap','x') OR true --"},
		"SRC with an empty element":           {"SRC": "soroswap,,cctp"},
		"SQL in FROM":                         {"FROM": "61000000; DROP TABLE trades"},
		"non-numeric TO":                      {"TO": "tip"},
		"WIN=0 (used to spin forever)":        {"WIN": "0"},
		"verdict names an unrequested source": {"SRC": "soroswap", "STUB_REDERIVE": "soroswap,phoenix"},
		"verdict names an unmapped source":    {"STUB_REDERIVE": "soroswap,redstone"},
		"verdict list is not a list":          {"STUB_REDERIVE": "soroswap; DROP TABLE trades"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			run := runProjectedScript(t, "", env)
			if run.exit == 0 {
				t.Errorf("exit 0\n%s", run.log)
			}
			if n := len(run.deletes()) + len(run.writes()); n != 0 {
				t.Errorf("reached psql or a -write %d time(s) (trace: %s)", n, run.sequence())
			}
			if strings.TrimSpace(run.state) != "" || strings.TrimSpace(run.dirty) != "" {
				t.Errorf("recorded state for a refused run: state=%q dirty=%q", run.state, run.dirty)
			}
		})
	}
}

// Done-state is per source. Keyed by window alone, a narrowed run marked
// the window done for everyone and the full run that followed skipped it.
func TestChRebuildProjectedScript_NarrowedRunDoesNotMarkOtherSourcesDone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if first := runProjectedScript(t, dir, map[string]string{"SRC": "soroswap"}); first.exit != 0 {
		t.Fatalf("narrowed run: exit %d\n%s", first.exit, first.log)
	}
	full := runProjectedScript(t, dir, nil)
	if full.exit != 0 {
		t.Fatalf("full run: exit %d\n%s", full.exit, full.log)
	}
	if len(full.writes()) != 1 {
		t.Fatalf("the full run skipped a window only soroswap had been rebuilt in (trace: %q)", full.sequence())
	}
	if got, want := full.writes()[0].flag("-sources"), "aquarius,phoenix,comet,blend,cctp,rozo,defindex"; got != want {
		t.Errorf("full run re-derived %q, want the seven sources still pending: %q", got, want)
	}
	for _, tbl := range tablesDeleted(full.deletes()[0].stdin) {
		if tbl == "trades:soroswap" || tbl == "soroswap_skim_events" {
			t.Errorf("the full run deleted %s again although soroswap was already rebuilt and is not re-derived now", tbl)
		}
	}
	again := runProjectedScript(t, dir, nil)
	if again.exit != 0 || len(again.calls) != 0 {
		t.Errorf("a third run over a fully done window still did work: exit %d, trace %q", again.exit, again.sequence())
	}
}

// Not red on the old script — a guard on the migration: state files on r1
// hold bare window starts written by the full-source run, and skipping is
// the non-destructive reading of them.
func TestChRebuildProjectedScript_LegacyDoneLineStillSkipsTheWindow(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "state"), 0o755); err != nil { //nolint:gosec // test temp dir
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state", "rebuild-done-windows.txt"), []byte("61000000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run := runProjectedScript(t, dir, nil)
	if run.exit != 0 || len(run.calls) != 0 {
		t.Errorf("a window with a legacy done line was re-processed: exit %d, trace %q", run.exit, run.sequence())
	}
}

// The finding's second scenario. The re-derive dies after the DELETE; the
// window is emptied. It must be recorded, never marked done, and the NEXT
// run must rebuild exactly what was emptied before anything else — even a
// narrowed run, or the hole could be certified.
func TestChRebuildProjectedScript_MidRunFailureIsRecordedAndRecoveredFirst(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	failed := runProjectedScript(t, dir, map[string]string{"TO": "62999999", "STUB_FAIL_WRITE_FROM": "61000000"})
	if failed.exit == 0 {
		t.Fatalf("exit 0 although the re-derive failed\n%s", failed.log)
	}
	if got, want := failed.sequence(), "preflight@61000000 psql write@61000000 record@61000000"; got != want {
		t.Errorf("failed run trace = %q, want %q (it must file the emptied window and stop, not move on to the next one)", got, want)
	}
	if strings.TrimSpace(failed.state) != "" {
		t.Errorf("a window whose re-derive FAILED was recorded done: %q", failed.state)
	}
	if want := "61000000 61999999 " + scriptDefaultSources + "\n"; failed.dirty != want {
		t.Fatalf("dirty marker = %q, want %q — the emptied window is recorded nowhere", failed.dirty, want)
	}
	if !strings.Contains(failed.log, "LEFT EMPTIED") {
		t.Errorf("the log does not say the window was left emptied:\n%s", failed.log)
	}

	narrowed := runProjectedScript(t, dir, map[string]string{"TO": "62999999", "SRC": "soroswap"})
	if narrowed.exit != 0 {
		t.Fatalf("recovery run: exit %d\n%s", narrowed.exit, narrowed.log)
	}
	if got, want := narrowed.sequence(),
		"record@61000000 preflight@61000000 psql write@61000000 refresh@61000000 preflight@62000000 psql write@62000000 refresh@62000000"; got != want {
		t.Fatalf("recovery trace = %q, want %q", got, want)
	}
	if got := narrowed.writes()[0].flag("-sources"); got != scriptDefaultSources {
		t.Errorf("the emptied window was re-derived for %q, want everything that was deleted: %q", got, scriptDefaultSources)
	}
	if got := narrowed.writes()[1].flag("-sources"); got != "soroswap" {
		t.Errorf("the next window honoured SRC=%q, want soroswap — recovery must not widen the operator's run", got)
	}
	if strings.TrimSpace(narrowed.dirty) != "" {
		t.Errorf("dirty marker survives a successful recovery: %q", narrowed.dirty)
	}
	for _, src := range strings.Split(scriptDefaultSources, ",") {
		if !strings.Contains(narrowed.state, src+" 61000000 61999999\n") {
			t.Errorf("%s not recorded done for the recovered window\nstate:\n%s", src, narrowed.state)
		}
	}
}

// A recovery that can no longer rebuild something it deleted must not
// quietly rebuild the rest and drop the marker.
func TestChRebuildProjectedScript_RecoveryRefusesWhenADeletedSourceIsNoLongerRederivable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if failed := runProjectedScript(t, dir, map[string]string{"STUB_FAIL_WRITE_FROM": "61000000"}); failed.exit == 0 {
		t.Fatal("fixture: the failing run exited 0")
	}
	run := runProjectedScript(t, dir, map[string]string{"STUB_REDERIVE": "soroswap,aquarius"})
	if run.exit == 0 {
		t.Errorf("recovery exited 0 although six deleted sources are no longer re-derivable\n%s", run.log)
	}
	if n := len(run.deletes()) + len(run.writes()); n != 0 {
		t.Errorf("a partial recovery ran (trace %q)", run.sequence())
	}
	if want := "61000000 61999999 " + scriptDefaultSources + "\n"; run.dirty != want {
		t.Errorf("dirty marker = %q, want it kept intact: %q", run.dirty, want)
	}
}

// The dirty marker is read back into a DELETE, so it is held to the same
// rules as SRC — and an unreadable one stops the run rather than being
// skipped, because it may be the only record of an emptied window.
func TestChRebuildProjectedScript_CorruptDirtyMarkerStopsTheRun(t *testing.T) {
	t.Parallel()
	for name, marker := range map[string]string{
		"sql in the source list": "61000000 61999999 soroswap','x') OR true --\n",
		"unmapped source":        "61000000 61999999 soroswap,sushiswap_v3\n",
		"truncated line":         "61000000\n",
		"non-numeric bound":      "61000000 tip soroswap\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "state"), 0o755); err != nil { //nolint:gosec // test temp dir
				t.Fatal(err)
			}
			path := filepath.Join(dir, "state", "rebuild-done-windows.txt.dirty")
			if err := os.WriteFile(path, []byte(marker), 0o600); err != nil {
				t.Fatal(err)
			}
			run := runProjectedScript(t, dir, nil)
			if run.exit == 0 || len(run.calls) != 0 {
				t.Errorf("exit %d, trace %q — want a refusal before any call", run.exit, run.sequence())
			}
			if run.dirty != marker {
				t.Errorf("the marker was rewritten: %q → %q", marker, run.dirty)
			}
		})
	}
}

// A failed DELETE batch rolls back, but a failure on the COMMIT itself is
// ambiguous — so the marker stays, and the next run redoes the window.
func TestChRebuildProjectedScript_FailedDeleteIsNotRebuiltOrMarkedDone(t *testing.T) {
	t.Parallel()
	run := runProjectedScript(t, "", map[string]string{"STUB_PSQL_RC": "3"})
	if run.exit == 0 {
		t.Errorf("exit 0 although psql failed\n%s", run.log)
	}
	if len(run.writes()) != 0 {
		t.Errorf("ch-rebuild -write ran after a failed DELETE (trace %q)", run.sequence())
	}
	if strings.TrimSpace(run.state) != "" {
		t.Errorf("recorded done after a failed DELETE: %q", run.state)
	}
	if strings.TrimSpace(run.dirty) == "" {
		t.Error("no dirty marker after a failed DELETE — a COMMIT that failed in flight may have emptied the window")
	}
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
	if rf.flag("-from") != "61000000" || rf.flag("-to") != "61999999" || rf.flag("-config") == "" || !rf.has("-write") {
		t.Errorf("refresh call = %q, want -config CFG -from 61000000 -to 61999999 -write", rf.args)
	}
	if rf.has("-sources") {
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

// TestChRebuildProjectedScript_EmptiedWindowTellsTheVerdictHow is the
// completeness-verdict leg of the emptied-window case: the DELETE landed, the re-derive died,
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
	// discharges it may. A successful recovery clears the local
	// marker and nothing else.
	if strings.TrimSpace(run.dirty) != "" {
		t.Errorf("$DIRTY after a successful recovery = %q, want empty", run.dirty)
	}
}

// The measured clean-window decision, which this test deliberately keeps.
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

// TestChRebuildProjectedScript_UnusableDirtyMarkerRefusesBeforeAnyDelete:
// an unwritable/missing state directory must not be silent — otherwise every
// append to $DIRTY fails and the DELETE runs anyway, so the window is emptied with
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

// TestChRebuildProjectedScript_FilesTheEmptiedWindowItself: the filing
// leg — the script RUNS the record command, it does not only print it.
func TestChRebuildProjectedScript_FilesTheEmptiedWindowItself(t *testing.T) {
	t.Parallel()
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
	// The contract: only the verdict that discharges the obligation may
	// retract it. A successful recovery clears the local marker, nothing else.
	if strings.TrimSpace(run.dirty) != "" {
		t.Errorf("$DIRTY after a successful recovery = %q, want empty", run.dirty)
	}
}

// Filing side: a window that re-derived
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

// A trade source named in the DELETE but absent from the
// re-derive is wiped and never rewritten, and the window is marked done.
// sushiswap_v3 was exactly that — and since the BackfillSafe gate it
// cannot be added to the re-derive either, so it must not be deleted.
func TestChRebuildProjectedScript_DeletesOnlyTradeSourcesItRederives(t *testing.T) {
	t.Parallel()
	run := runProjectedScript(t, "", nil)
	if run.exit != 0 {
		t.Fatalf("default run exited %d\n%s", run.exit, run.log)
	}
	writes := run.writes()
	if len(writes) != 1 {
		t.Fatalf("want exactly one ch-rebuild -write for the one window, got %d\n%s", len(writes), run.log)
	}
	rederived := csvSet(writes[0].flag("-sources"))
	if len(rederived) == 0 {
		t.Fatalf("the -write call named no sources: %q", writes[0].args)
	}
	seen := 0
	for _, del := range run.deletes() {
		for _, src := range tradeSourcesDeleted(del.stdin) {
			seen++
			if !rederived[src] {
				t.Errorf("window DELETEs trades for %q but ch-rebuild -write was asked to re-derive only %q — "+
					"those rows are deleted, never rewritten, and the window is marked done", src, writes[0].flag("-sources"))
			}
		}
	}
	if seen == 0 {
		t.Fatalf("no trades DELETE reached psql — this test asserted nothing\n%s", run.log)
	}
}

// ch-rebuild's refusals (BackfillSafe, the live-cursor one-writer
// guard, the buffered-range ceiling) must fire before the -write
// run, not after the script's DELETE has committed — or the guard doing its
// job would leave the window's tables empty. A refusal must cost nothing.
//
// Every shape a "no" can arrive in is covered, not just the polite one: a
// guard refusal, a deployed binary that predates -preflight (flag parse
// error), and a binary that exits 0 without a verdict line.
func TestChRebuildProjectedScript_RefusalDeletesNothing(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"refuse", "oldbinary", "silent"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			run := runProjectedScript(t, "", map[string]string{"STUB_PREFLIGHT": mode})
			if run.exit == 0 {
				t.Errorf("the script exited 0 although the binary gave no go-ahead\n%s", run.log)
			}
			if n := len(run.deletes()); n != 0 {
				t.Errorf("%d DELETE batch(es) reached psql although ch-rebuild would not run — "+
					"the window's tables are now empty and nothing will rewrite them (trace: %s)", n, run.sequence())
			}
			if strings.TrimSpace(run.state) != "" {
				t.Errorf("a refused window was recorded as done: %q", run.state)
			}
		})
	}
}

// A refusal on a LATER window must leave that window untouched and the
// earlier ones complete — never a deleted window waiting on a run that
// was refused.
func TestChRebuildProjectedScript_RefusalOnSecondWindowLeavesItUntouched(t *testing.T) {
	t.Parallel()
	run := runProjectedScript(t, "", map[string]string{
		"TO":               "62999999",
		"STUB_PREFLIGHT":   "refuse",
		"STUB_REFUSE_FROM": "62000000",
	})
	if run.exit == 0 {
		t.Fatalf("exit 0 despite a refused window\n%s", run.log)
	}
	if got, want := run.sequence(), "preflight@61000000 psql write@61000000 refresh@61000000 preflight@62000000"; got != want {
		t.Errorf("call trace = %q, want %q", got, want)
	}
}

// The preflight is only worth anything if it asks about the SAME run: same
// range, same sources, and it must come before the DELETE in every window.
func TestChRebuildProjectedScript_PreflightMatchesTheWriteItPrecedes(t *testing.T) {
	t.Parallel()
	run := runProjectedScript(t, "", map[string]string{"TO": "62999999"})
	if run.exit != 0 {
		t.Fatalf("exit %d\n%s", run.exit, run.log)
	}
	if got, want := run.sequence(), "preflight@61000000 psql write@61000000 refresh@61000000 preflight@62000000 psql write@62000000 refresh@62000000"; got != want {
		t.Fatalf("call trace = %q, want %q", got, want)
	}
	pre, wr := run.preflights(), run.writes()
	for i := range wr {
		for _, f := range []string{"-config", "-from", "-to"} {
			if pre[i].flag(f) != wr[i].flag(f) {
				t.Errorf("window %d: preflight %s=%q but write %s=%q", i, f, pre[i].flag(f), f, wr[i].flag(f))
			}
		}
		if !pre[i].has("-write") {
			t.Errorf("window %d: preflight ran without -write — a dry run is not guarded, so it checked nothing", i)
		}
	}
}

// The DELETE batch is one transaction: psql autocommits each statement
// otherwise, and a failure on the ninth table would leave the first eight
// emptied. (Executed against real Postgres in
// test/integration/pg_ops_pipeline_test.go.)
func TestChRebuildProjectedScript_DeleteBatchIsOneTransaction(t *testing.T) {
	t.Parallel()
	run := runProjectedScript(t, "", nil)
	dels := run.deletes()
	if len(dels) != 1 {
		t.Fatalf("want one DELETE batch, got %d\n%s", len(dels), run.log)
	}
	var stmts []string
	for _, s := range strings.Split(dels[0].stdin, ";") {
		if s = strings.TrimSpace(s); s != "" {
			stmts = append(stmts, s)
		}
	}
	if len(stmts) < 3 || stmts[0] != "BEGIN" || stmts[len(stmts)-1] != "COMMIT" {
		t.Errorf("DELETE batch is not wrapped in BEGIN … COMMIT: first=%q last=%q", stmts[0], stmts[len(stmts)-1])
	}
	if !strings.Contains(dels[0].args, "ON_ERROR_STOP=1") {
		t.Errorf("psql runs without ON_ERROR_STOP=1 (%q) — it would carry on to COMMIT past a failed statement", dels[0].args)
	}
}

// The DELETE batch probes each source before emptying it; the -write that
// follows must be told which sources held rows, so a re-derive that brings
// none back fails instead of marking an emptied window done.
func TestChRebuildProjectedScript_RequiresRowsForEveryOccupiedSource(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, psqlOut, want string
	}{
		{"occupied", "BEGIN\noccupied=soroswap\nDELETE 4\noccupied=cctp\noccupied=soroswap\nDELETE 1\nCOMMIT", "-require-rows=cctp,soroswap"},
		{"quiet window", "BEGIN\nDELETE 0\nCOMMIT", "-require-rows="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			run := runProjectedScript(t, "", map[string]string{"SRC": "soroswap,cctp", "STUB_PSQL_OUT": tc.psqlOut})
			dels, writes := run.deletes(), run.writes()
			if len(dels) != 1 || len(writes) != 1 {
				t.Fatalf("want one DELETE and one -write, got %d/%d\n%s", len(dels), len(writes), run.log)
			}
			for _, s := range []string{"soroswap", "cctp"} {
				if !strings.Contains(dels[0].stdin, "SELECT 'occupied="+s+"' WHERE ") {
					t.Errorf("the DELETE batch does not probe %s before emptying it\n%s", s, dels[0].stdin)
				}
			}
			if !writes[0].has(tc.want) {
				t.Errorf("-write args %q lack %s", writes[0].args, tc.want)
			}
		})
	}
}

// A trade the store refused is a row the DELETE removed and nothing put
// back: the binary's failed-write exit must leave the window dirty instead of
// marking it done.
func TestChRebuildProjectedScript_RefusedTradeLeavesWindowDirty(t *testing.T) {
	t.Parallel()
	run := runProjectedScript(t, "", map[string]string{"SRC": "aquarius", "STUB_REFUSED_FROM": "61000000"})
	if run.exit == 0 || run.dirty == "" {
		t.Fatalf("a refused trade must fail the run and leave the window dirty (exit %d, dirty %q)\n%s", run.exit, run.dirty, run.log)
	}
	if strings.Contains(run.state, "61000000") {
		t.Errorf("the window with a refused trade was marked done:\n%s", run.state)
	}
}

// A recovery's DELETE probes a window the earlier run already emptied, so
// it finds nothing: what that earlier DELETE removed must still be required.
func TestChRebuildProjectedScript_RecoveryKeepsTheRowRequirement(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	first := runProjectedScript(t, dir, map[string]string{
		"SRC": "blend", "STUB_FAIL_WRITE_FROM": "61000000",
		"STUB_PSQL_OUT": "BEGIN\noccupied=blend\nDELETE 3\nCOMMIT",
	})
	if first.exit == 0 || first.dirty == "" {
		t.Fatalf("fixture: the first -write must fail and leave the window dirty (exit %d, dirty %q)\n%s", first.exit, first.dirty, first.log)
	}
	second := runProjectedScript(t, dir, map[string]string{"SRC": "blend", "STUB_PSQL_OUT": "BEGIN\nDELETE 0\nCOMMIT"})
	writes := second.writes()
	if len(writes) != 1 {
		t.Fatalf("want one recovery -write, got %d\n%s", len(writes), second.log)
	}
	if !writes[0].has("-require-rows=blend") {
		t.Errorf("recovery -write args %q lack -require-rows=blend — an empty re-derive would mark the emptied window done", writes[0].args)
	}
	if second.exit != 0 || second.dirty != "" {
		t.Fatalf("fixture: the recovery should succeed (exit %d, dirty %q)\n%s", second.exit, second.dirty, second.log)
	}
	if req := readIfExists(t, filepath.Join(dir, "state", "rebuild-done-windows.txt.required")); req != "" {
		t.Errorf("a rebuilt window's row requirement outlived it: %q", req)
	}
}

// Writer → reader: the line the REAL binary prints is what the shipped
// script has to parse. A stub that invents its own line would keep both
// halves green while they drift apart.
func TestChRebuildProjectedScript_ParsesTheRealPreflightLine(t *testing.T) {
	t.Parallel()
	var line strings.Builder
	sources := []string{"aquarius", "soroswap", "phoenix", "comet", "blend", "cctp", "rozo", "defindex"}
	if err := reportCHRebuildPreflight(&line, 61_000_000, 61_999_999, sources); err != nil {
		t.Fatal(err)
	}
	run := runProjectedScript(t, "", map[string]string{"STUB_PREFLIGHT_LINE": strings.TrimSuffix(line.String(), "\n")})
	if run.exit != 0 {
		t.Fatalf("the script rejected the real binary's preflight line %q\n%s", line.String(), run.log)
	}
	if got, want := run.sequence(), "preflight@61000000 psql write@61000000 refresh@61000000"; got != want {
		t.Errorf("call trace = %q, want %q", got, want)
	}
}

// TestChRebuildProjectedScript_DeletesOnlySourcesTradeOfCanRewrite pins the
// other half of the pairing: every source named in the repair script's
// trades DELETE must be a source ch-rebuild can actually re-derive.
func TestChRebuildProjectedScript_DeletesOnlySourcesTradeOfCanRewrite(t *testing.T) {
	deleted := deletedTradeSources(t, "../../../scripts/ops/ch-rebuild-projected.sh")
	if len(deleted) == 0 {
		t.Fatal("no trades DELETE source list found in ch-rebuild-projected.sh")
	}
	targetsBySource := reconciliationTargetsBySource(t)
	for _, name := range deleted {
		if _, ok := targetsBySource[name]; !ok {
			t.Errorf("ch-rebuild-projected.sh deletes trades for %q, which is not in the reconciliation catalogue — "+
				"the repair would delete rows nothing re-derives", name)
		}
	}
}

// TestChRebuildProjectedScript_EveryCaseArmDeleteIsReconcilable is the
// per-arm check: the trades DELETE above is only 1 of the 12 DELETE statements
// window_delete_sql emits — the other 11 clear per-source ancillary
// tables (aquarius_admin, blend_positions, ...) inside a per-source case
// arm. A source/table pair deleted there but absent from ch-rebuild's
// reconciliation catalogue is the same silent-data-loss shape as the
// trades case, just on a different table.
func TestChRebuildProjectedScript_EveryCaseArmDeleteIsReconcilable(t *testing.T) {
	arms := caseArmDeletes(t, "../../../scripts/ops/ch-rebuild-projected.sh")
	if len(arms) == 0 {
		t.Fatal("no per-source case-arm DELETEs found in ch-rebuild-projected.sh")
	}
	targetsBySource := reconciliationTargetsBySource(t)
	for source, tables := range arms {
		known, ok := targetsBySource[source]
		if !ok {
			t.Errorf("ch-rebuild-projected.sh's case arm %q deletes %v, but %q is not in the reconciliation catalogue — "+
				"the repair would delete rows nothing re-derives", source, tables, source)
			continue
		}
		for _, table := range tables {
			if !known[table] {
				t.Errorf("ch-rebuild-projected.sh's %q arm deletes table %q, which is not among %q's reconciliation "+
					"targets (%v) — the repair would delete rows ch-rebuild does not know how to re-derive",
					source, table, source, sortedKeys(known))
			}
		}
	}
}
