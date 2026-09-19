// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

// scripts/ops/ch-rebuild-projected.sh is destructive: per window it DELETEs
// served-tier rows and then asks `ch-rebuild -write` to re-derive them. The
// tests here EXECUTE the shipped script — not a copy of it — against stubs
// for `psql` and the ops binary, the way scripts/ci/run-heavy-job-test.sh
// executes the heavy-job wrapper, and assert on what it actually did: which
// rows it could delete, what it asked the binary to re-derive, in what
// order, and what it recorded.
//
// The stub binary speaks the real binary's shapes, including the ugly ones:
// progress noise on stderr, a refusal as a non-zero exit with nothing on
// stdout, and the final report on stdout.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const projectedScriptPath = "../../../scripts/ops/ch-rebuild-projected.sh"

const stubPsql = `#!/usr/bin/env bash
{ printf 'PSQL %s\n' "$*"; cat; printf 'PSQL-END\n'; } >> "$STUB_CALLS"
exit "${STUB_PSQL_RC:-0}"
`

const stubOps = `#!/usr/bin/env bash
printf 'OPS %s\n' "$*" >> "$STUB_CALLS"
from=""; to=""; srcs=""; pre=0
while [ $# -gt 0 ]; do
  case "$1" in
    -from) from="$2"; shift ;;
    -to) to="$2"; shift ;;
    -sources) srcs="$2"; shift ;;
    -preflight) pre=1 ;;
  esac
  shift
done
echo "ch-rebuild: seeded 4821 soroswap pairs from PG" >&2
# The real guards run on EVERY -write invocation, preflight or not: a
# stub that refused only under -preflight could not show what an
# un-preflighted script does when the binary says no.
if [ "${STUB_PREFLIGHT:-ok}" = refuse ] && { [ -z "${STUB_REFUSE_FROM:-}" ] || [ "$from" = "$STUB_REFUSE_FROM" ]; }; then
  echo "ch-rebuild: refusing to -write [$from,$to]: live projector cursor is below -to" >&2
  exit 1
fi
if [ "$pre" = 1 ]; then
  case "${STUB_PREFLIGHT:-ok}" in
    ok|refuse)
      if [ -n "${STUB_PREFLIGHT_LINE:-}" ]; then
        printf '%s\n' "$STUB_PREFLIGHT_LINE"
      else
        echo "$STUB_PREFLIGHT_PREFIX [$from,$to] rederive=${STUB_REDERIVE-$srcs}"
      fi ;;
    oldbinary)
      echo "flag provided but not defined: -preflight" >&2
      exit 2 ;;
    silent) ;;
  esac
  exit 0
fi
if [ -n "${STUB_FAIL_WRITE_FROM:-}" ] && [ "$from" = "$STUB_FAIL_WRITE_FROM" ]; then
  echo "ch-rebuild: event stream: read timeout" >&2
  exit 1
fi
printf '\n=== ch-rebuild [%s] WRITE ===\n' "$from"
`

// scriptCall is one invocation the script made of a stubbed program.
type scriptCall struct {
	kind  string // "OPS" or "PSQL"
	args  string
	stdin string // PSQL only: the SQL the script fed it
}

func (c scriptCall) flag(name string) string {
	fields := strings.Fields(c.args)
	for i, f := range fields {
		if f == name && i+1 < len(fields) {
			return fields[i+1]
		}
	}
	return ""
}

func (c scriptCall) has(name string) bool {
	for _, f := range strings.Fields(c.args) {
		if f == name {
			return true
		}
	}
	return false
}

type scriptRun struct {
	exit  int
	calls []scriptCall
	log   string
	state string
	dir   string
}

// deletes returns the psql invocations, in order.
func (r scriptRun) deletes() []scriptCall { return r.ofKind("PSQL", false, false) }

// writes returns the binary's -write invocations that are NOT preflights.
func (r scriptRun) writes() []scriptCall { return r.ofKind("OPS", true, false) }

// preflights returns the binary's -write -preflight invocations.
func (r scriptRun) preflights() []scriptCall { return r.ofKind("OPS", true, true) }

// sequence renders the calls as a compact ordered trace, e.g.
// "preflight@61000000 psql write@61000000".
func (r scriptRun) sequence() string {
	var out []string
	for _, c := range r.calls {
		switch {
		case c.kind == "PSQL":
			out = append(out, "psql")
		case c.has("-preflight"):
			out = append(out, "preflight@"+c.flag("-from"))
		default:
			out = append(out, "write@"+c.flag("-from"))
		}
	}
	return strings.Join(out, " ")
}

func (r scriptRun) ofKind(kind string, needWrite, preflight bool) []scriptCall {
	var out []scriptCall
	for _, c := range r.calls {
		if c.kind != kind {
			continue
		}
		if kind == "OPS" && (c.has("-write") != needWrite || c.has("-preflight") != preflight) {
			continue
		}
		out = append(out, c)
	}
	return out
}

// runProjectedScript executes the shipped script in dir (a fresh temp dir
// when empty) with stubs first on PATH. env entries override the defaults.
func runProjectedScript(t *testing.T, dir string, env map[string]string) scriptRun {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the ops script is bash; r1 and CI are Linux")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("bash not found — this test must EXECUTE the script, it cannot pass without it: %v", err)
	}
	script, err := filepath.Abs(projectedScriptPath)
	if err != nil {
		t.Fatal(err)
	}
	if dir == "" {
		dir = t.TempDir()
	}
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"psql": stubPsql, "stellarindex-ops-ch": stubOps} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil { //nolint:gosec // an executable test stub
			t.Fatal(err)
		}
	}
	calls := filepath.Join(dir, "calls.txt")
	_ = os.Remove(calls)
	vars := map[string]string{
		"PATH":                      bin + ":/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin",
		"STUB_CALLS":                calls,
		"STUB_PREFLIGHT_PREFIX":     chRebuildPreflightPrefix,
		"OPS":                       filepath.Join(bin, "stellarindex-ops-ch"),
		"CFG":                       filepath.Join(dir, "stellarindex.toml"),
		"STATE":                     filepath.Join(dir, "state", "rebuild-done-windows.txt"),
		"LOG":                       filepath.Join(dir, "rebuild.log"),
		"STELLARINDEX_POSTGRES_DSN": "postgres://stub-host/stubdb",
		"FROM":                      "61000000",
		"TO":                        "61999999",
		"WIN":                       "1000000",
	}
	for k, v := range env {
		vars[k] = v
	}
	cmd := exec.Command(bash, script) //nolint:gosec // fixed script path, test-controlled env
	for k, v := range vars {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, runErr := cmd.CombinedOutput()
	run := scriptRun{dir: dir}
	if runErr != nil {
		ee, ok := runErr.(*exec.ExitError) //nolint:errorlint // exec returns the concrete type
		if !ok {
			t.Fatalf("run script: %v\n%s", runErr, out)
		}
		run.exit = ee.ExitCode()
	}
	run.calls = parseScriptCalls(t, calls)
	run.log = readIfExists(t, vars["LOG"]) + string(out)
	run.state = readIfExists(t, vars["STATE"])
	return run
}

func readIfExists(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // test temp file
	if err != nil {
		if os.IsNotExist(err) {
			return ""
		}
		t.Fatal(err)
	}
	return string(b)
}

func parseScriptCalls(t *testing.T, path string) []scriptCall {
	t.Helper()
	var out []scriptCall
	var cur *scriptCall
	for _, line := range strings.Split(readIfExists(t, path), "\n") {
		switch {
		case cur != nil && line == "PSQL-END":
			out = append(out, *cur)
			cur = nil
		case cur != nil:
			cur.stdin += line + "\n"
		case strings.HasPrefix(line, "PSQL "):
			cur = &scriptCall{kind: "PSQL", args: strings.TrimPrefix(line, "PSQL ")}
		case strings.HasPrefix(line, "OPS "):
			out = append(out, scriptCall{kind: "OPS", args: strings.TrimPrefix(line, "OPS ")})
		}
	}
	if cur != nil {
		t.Fatalf("unterminated psql call in %s", path)
	}
	return out
}

var (
	tradesInListRE  = regexp.MustCompile(`DELETE FROM trades WHERE source IN \(([^)]*)\)`)
	tradesRunListRE = regexp.MustCompile(`source = ANY \(string_to_array\('([^']*)', ','\)\)`)
)

// tradeSourcesDeleted returns the trade sources one psql call's SQL can
// delete rows for: the statement's IN list, intersected with its run
// scope when it carries one.
func tradeSourcesDeleted(sql string) []string {
	var out []string
	for _, stmt := range strings.Split(sql, ";") {
		m := tradesInListRE.FindStringSubmatch(stmt)
		if m == nil {
			continue
		}
		var scope map[string]bool
		if s := tradesRunListRE.FindStringSubmatch(stmt); s != nil {
			scope = map[string]bool{}
			for _, name := range strings.Split(s[1], ",") {
				scope[strings.TrimSpace(name)] = true
			}
		}
		for _, raw := range strings.Split(m[1], ",") {
			name := strings.Trim(strings.TrimSpace(raw), "'")
			if name != "" && (scope == nil || scope[name]) {
				out = append(out, name)
			}
		}
	}
	sort.Strings(out)
	return out
}

func csvSet(csv string) map[string]bool {
	out := map[string]bool{}
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out[s] = true
		}
	}
	return out
}

// RLT-380: a trade source named in the DELETE but absent from the
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

// RLT-381: ch-rebuild's refusals (BackfillSafe, the live-cursor one-writer
// guard, the buffered-range ceiling) used to fire only inside the -write
// run, AFTER the script's DELETE had committed — so the guard doing its job
// left the window's tables empty. A refusal must now cost nothing.
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
	if got, want := run.sequence(), "preflight@61000000 psql write@61000000 preflight@62000000"; got != want {
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
	if got, want := run.sequence(), "preflight@61000000 psql write@61000000 preflight@62000000 psql write@62000000"; got != want {
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
// test/integration/ch_rebuild_projected_script_test.go.)
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
	if got, want := run.sequence(), "preflight@61000000 psql write@61000000"; got != want {
		t.Errorf("call trace = %q, want %q", got, want)
	}
}

func TestCHRebuild_PreflightWithoutWriteIsRefused(t *testing.T) {
	t.Parallel()
	err := chRebuild(chRebuildArgs(t, "-preflight", "-sources", "aquarius"))
	if err == nil || !strings.Contains(err.Error(), "-preflight") {
		t.Fatalf("a bare -preflight runs none of the -write guards and must not answer ok; got: %v", err)
	}
}

func TestCHRebuild_PreflightRunsTheNamedSourceGate(t *testing.T) {
	t.Parallel()
	err := chRebuild(chRebuildArgs(t, "-write", "-preflight", "-sources", "aquarius,sushiswap_v3"))
	if err == nil || !strings.Contains(err.Error(), "not BackfillSafe") {
		t.Fatalf("-write -preflight over an unaudited source was not refused by the BackfillSafe gate: %v", err)
	}
}

// chRebuild needs Postgres and ClickHouse past the config load, so — like
// the BackfillSafe legs — the preflight's POSITION is pinned at the source:
// after the last refusal, before the first lake read. (It is executed for
// real in test/integration/ch_rebuild_preflight_test.go.)
func TestCHRebuild_PreflightReturnsAfterEveryGuardAndBeforeTheLake(t *testing.T) {
	t.Parallel()
	b, err := os.ReadFile("ch_rebuild.go")
	if err != nil {
		t.Fatal(err)
	}
	body := funcBodyFrom(string(b), "chRebuild")
	exit := strings.Index(body, "return reportCHRebuildPreflight(")
	leg2 := strings.Index(body, "checkCHRebuildBackfillSafe(reDerivedSourcesInRun(")
	overlap := strings.Index(body, "checkCHRebuildLiveOverlap(")
	rangeGuard := strings.Index(body, "> maxBufferedRange")
	lake := strings.Index(body, "clickhouse.Stream")
	if exit < 0 || leg2 < 0 || overlap < 0 || rangeGuard < 0 || lake < 0 {
		t.Fatalf("anchors not found (exit %d, leg2 %d, overlap %d, range %d, lake %d) — this test is asserting nothing",
			exit, leg2, overlap, rangeGuard, lake)
	}
	for name, pos := range map[string]int{"BackfillSafe leg 2": leg2, "live-cursor guard": overlap, "buffered-range guard": rangeGuard} {
		if exit < pos {
			t.Errorf("-preflight returns BEFORE the %s — it would answer ok to a run the guard refuses", name)
		}
	}
	if exit > lake {
		t.Error("-preflight returns AFTER the first lake read — it is no longer a preflight")
	}
}
