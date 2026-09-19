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
from=""; srcs=""; pre=0
while [ $# -gt 0 ]; do
  case "$1" in
    -from) from="$2"; shift ;;
    -sources) srcs="$2"; shift ;;
    -preflight) pre=1 ;;
  esac
  shift
done
echo "ch-rebuild: seeded 4821 soroswap pairs from PG" >&2
if [ "$pre" = 1 ]; then
  case "${STUB_PREFLIGHT:-ok}" in
    ok) echo "ch-rebuild: preflight ok [$from] rederive=${STUB_REDERIVE-$srcs}" ;;
    refuse)
      echo "ch-rebuild: refusing to -write [$from]: live projector cursor is below -to" >&2
      exit 1 ;;
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
