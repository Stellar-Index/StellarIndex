// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

// F075: scripts/ops/ch-rebuild-projected.sh deleted a HARD-CODED table set
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
	for _, source := range strings.Split(scriptDefaultSources, ",") {
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
	for _, source := range strings.Split(scriptDefaultSources, ",") {
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
		"verdict names an unmapped source":    {"STUB_REDERIVE": "soroswap,upshift"},
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
// narrowed run, which is how the hole used to get certified.
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
		"record@61000000 preflight@61000000 psql write@61000000 preflight@62000000 psql write@62000000"; got != want {
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
