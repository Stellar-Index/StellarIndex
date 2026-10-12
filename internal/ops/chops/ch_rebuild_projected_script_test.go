// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

// scripts/ops/ch-rebuild-projected.sh is destructive: per window it DELETEs served-tier rows and
// then asks `ch-rebuild -write` to re-derive them. These tests EXECUTE the shipped script, not a
// copy, against stubs for `psql` and the ops binary (as scripts/ci/run-heavy-job-test.sh does for
// the heavy-job wrapper), and assert what it did: which rows it could delete, what it asked the
// binary to re-derive, in what order, and what it recorded.
//
// The stub binary speaks the real binary's shapes, including the ugly ones: progress noise on
// stderr, a refusal as a non-zero exit with nothing on stdout, and the final report on stdout.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

const projectedScriptPath = "../../../scripts/ops/ch-rebuild-projected.sh"

const stubPsql = `#!/usr/bin/env bash
{ printf 'PSQL %s\n' "$*"; cat; printf 'PSQL-END\n'; } >> "$STUB_CALLS"
[ -z "${STUB_PSQL_OUT:-}" ] || printf '%s\n' "$STUB_PSQL_OUT"
exit "${STUB_PSQL_RC:-0}"
`

const stubOps = `#!/usr/bin/env bash
printf 'OPS %s\n' "$*" >> "$STUB_CALLS"
verb="$1"; from=""; to=""; srcs=""; pre=0; rec=0
while [ $# -gt 0 ]; do
  case "$1" in
    -from) from="$2"; shift ;;
    -to) to="$2"; shift ;;
    -sources) srcs="$2"; shift ;;
    -preflight) pre=1 ;;
    -record-dirty-window) rec=1 ;;
  esac
  shift
done
# The CAGG refresh reads and writes only Postgres; only
# STUB_FAIL_REFRESH_FROM (a statement timeout, PG down) makes it fail.
if [ "$verb" = trades-cagg-refresh ]; then
  if [ -n "${STUB_FAIL_REFRESH_FROM:-}" ] && [ "$from" = "$STUB_FAIL_REFRESH_FROM" ]; then
    echo "trades-cagg-refresh: prices_1m: canceling statement due to statement timeout" >&2
    exit 1
  fi
  echo "trades-cagg-refresh: refreshed [$from,$to]"
  exit 0
fi
echo "ch-rebuild: seeded 4821 soroswap pairs from PG" >&2
# The record is its own mode: it reads no lake and re-derives nothing, so
# neither the -write guards nor a re-derive's failure apply to it. Only
# STUB_FAIL_RECORD (PG down, no binary) can make it fail.
if [ "$rec" = 1 ]; then
  if [ -n "${STUB_FAIL_RECORD:-}" ] && [ "$from" = "$STUB_FAIL_RECORD" ]; then
    echo "ch-rebuild: -record-dirty-window (cctp): dial tcp 127.0.0.1:5432: connect: connection refused" >&2
    exit 1
  fi
  echo "$STUB_RECORDED_PREFIX [$from,$to] sources=$srcs"
  exit 0
fi
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
# The binary's failed-write gate: a trade the store refuses fails the -write.
if [ -n "${STUB_REFUSED_FROM:-}" ] && [ "$from" = "$STUB_REFUSED_FROM" ]; then
  echo "ch-rebuild: 1 event(s) failed to write (rows missing) — see the 'failed' column and re-run to recover" >&2
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

// isCAGGRefresh reports whether this is the trades-cagg-refresh verb.
func (c scriptCall) isCAGGRefresh() bool {
	return c.kind == "OPS" && strings.HasPrefix(c.args, "trades-cagg-refresh ")
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
	state string // the done-state file after the run
	dirty string // the dirty-marker file after the run
	dir   string
}

// deletes returns the psql invocations, in order.
func (r scriptRun) deletes() []scriptCall { return r.ofKind("PSQL", false, false) }

// writes returns the binary's -write invocations that are NOT preflights.
func (r scriptRun) writes() []scriptCall { return r.ofKind("OPS", true, false) }

// preflights returns the binary's -write -preflight invocations.
func (r scriptRun) preflights() []scriptCall { return r.ofKind("OPS", true, true) }

// records returns the binary's -record-dirty-window invocations: the
// projection dirty windows the script FILED, as opposed to the ones it only
// printed a command for.
func (r scriptRun) records() []scriptCall {
	var out []scriptCall
	for _, c := range r.calls {
		if c.kind == "OPS" && c.has("-record-dirty-window") {
			out = append(out, c)
		}
	}
	return out
}

// sequence renders the calls as an ordered trace, e.g. "preflight@61000000 psql write@61000000 record@61000000". The OPS
// modes stay distinct: preflight asks, write re-derives, record files the emptied window, refresh re-materialises the
// trades aggregates; merging record into write would hide whether anything was filed.
func (r scriptRun) sequence() string {
	var out []string
	for _, c := range r.calls {
		switch {
		case c.kind == "PSQL":
			out = append(out, "psql")
		case c.has("-preflight"):
			out = append(out, "preflight@"+c.flag("-from"))
		case c.has("-record-dirty-window"):
			out = append(out, "record@"+c.flag("-from"))
		case c.isCAGGRefresh():
			out = append(out, "refresh@"+c.flag("-from"))
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
		if kind == "OPS" && (c.has("-write") != needWrite || c.has("-preflight") != preflight || c.isCAGGRefresh()) {
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
		"STUB_RECORDED_PREFIX":      chRebuildDirtyRecordedPrefix,
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
	// Bounded: a script that never terminates (e.g. WIN=0 spinning forever)
	// must read as a failure here, not as a stuck suite.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bash, script) //nolint:gosec // fixed script path, test-controlled env
	for k, v := range vars {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	out, runErr := cmd.CombinedOutput()
	run := scriptRun{dir: dir}
	if ctx.Err() != nil {
		t.Fatalf("the script did not terminate within 60s (env %v)", env)
	}
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
	run.dirty = readIfExists(t, vars["STATE"]+".dirty")
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

func TestCHRebuild_PreflightWithoutWriteIsRefused(t *testing.T) {
	t.Parallel()
	err := chRebuild(chRebuildArgs(t, "-preflight", "-sources", "aquarius"))
	if err == nil || !strings.Contains(err.Error(), "-preflight") {
		t.Fatalf("a bare -preflight runs none of the -write guards and must not answer ok; got: %v", err)
	}
}

func TestCHRebuild_PreflightRunsTheNamedSourceGate(t *testing.T) {
	t.Parallel()
	err := chRebuild(chRebuildArgs(t, "-write", "-preflight", "-sources", "aquarius,upshift"))
	if err == nil || !strings.Contains(err.Error(), "not BackfillSafe") {
		t.Fatalf("-write -preflight over an unaudited source was not refused by the BackfillSafe gate: %v", err)
	}
}

// chRebuild needs Postgres and ClickHouse past config load, so, like the BackfillSafe legs, the preflight's position is
// pinned at the source: after the last refusal and before the first lake read. It is executed for real in
// test/integration/lake_test.go.
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

// The script rewrites `trades` years behind tip, beyond any trades continuous aggregate's refresh lookback, so without
// an explicit refresh served OHLC / volume / TWAP keep the pre-repair rows. These tests execute the shipped script
// (stubs in ch_rebuild_projected_script_test.go).

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

// ch-rebuild-projected.sh must itself file the ADR-0033 dirty window whenever it leaves a window empty, or /v1/coverage
// keeps certifying a range with no rows complete=true. Every emptying state files one window for exactly the deleted
// sources; a state that empties nothing files none; a failed filing is loud.

// assertFiledWindow checks the one -record-dirty-window invocation names the emptied range and exactly the deleted
// sources. A wrong window files the wrong obligation, and one carrying -write is refused by the binary
// (checkCHRebuildRecordDirtyFlags).
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

// An emptied window has TWO records to keep in scripts/ops/ch-rebuild-projected.sh:
//
//   - $DIRTY, the local marker that drives this script's own recovery. It must be WRITTEN
//     before anything is deleted; a failed append may not be shrugged off, or the DELETE
//     proceeds with nothing left to rebuild the window.
//   - the ADR-0033 projection dirty window, which the completeness verdict reads. Until the
//     emptied range is filed there, /v1/coverage keeps its prior clean claim over a hole.
//
// These tests EXECUTE the shipped script (the harness in ch_rebuild_projected_script_test.go)
// and pin both, plus the case that must NOT change: a window that re-derived cleanly files
// nothing, since filing one per routine window would point the next nightly at a huge
// ledger range across all sources and time every verdict out.

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

// ── the local marker: rule 3 is only a rule if the write is CHECKED ──────
