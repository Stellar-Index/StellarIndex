package clickhouse

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Regression tests for F112: stellar.account_activity's "exact upper bound
// by construction" invariant (deploy/clickhouse/account_activity.sql) only
// holds once the Step-2 windowed backfill has covered every account, and the
// bound it feeds is a HARD `ledger_seq <=` on the account-history readers —
// a partial backfill silently hides history.
//
// These tests run the runbook's OWN text, extracted verbatim from the SQL
// file's comments (what an operator copy-pastes, not a paraphrase), through
// the SHIPPED /usr/local/sbin/run-heavy-job.sh, extracted from the ansible
// task that installs it (the technique of scripts/ci/run-heavy-job-test.sh).
// A pass-through stand-in for the wrapper cannot see the wrapper's own
// lock branch: when the per-job lock is held every caller is refused
// with exit 75 (a manual run prints "refusing to start", a
// systemd-launched one prints "skipping this fire") without running
// the payload.
//
// Every way the runbook can end WITHOUT covering (Step 2) or checking
// (Step 3) every window must end non-zero and without its success line.
// Whether the Step-3 SQL itself can see a gap in a later window needs a real
// ClickHouse: test/integration/account_activity_backfill_verify_test.go.

// runbookCodeLineRE matches a runbook code line: `--` followed by at least
// three spaces (the file's indent convention for embedded bash/SQL; prose
// comments use one or two).
var runbookCodeLineRE = regexp.MustCompile(`^--   (.*)$`)

const (
	runbookWrapperPath = "/usr/local/sbin/run-heavy-job.sh"
	step2Done          = "account_activity backfill: COMPLETE"
	step3Done          = "account_activity verify: PASSED"
)

// extractRunbookSection pulls the literal code lines between two marker
// substrings out of a deploy/clickhouse/*.sql file.
func extractRunbookSection(t *testing.T, sqlPath, startMarker, endMarker string) string {
	t.Helper()
	b, err := os.ReadFile(sqlPath)
	if err != nil {
		t.Fatalf("read %s: %v", sqlPath, err)
	}
	inSection := false
	var code []string
	for _, line := range strings.Split(string(b), "\n") {
		if strings.Contains(line, startMarker) {
			inSection = true
			continue
		}
		if inSection && strings.Contains(line, endMarker) {
			break
		}
		if !inSection {
			continue
		}
		if m := runbookCodeLineRE.FindStringSubmatch(line); m != nil {
			code = append(code, m[1])
		}
	}
	if len(code) == 0 {
		t.Fatalf("no runbook code lines between %q and %q in %s — the markers or the 3-space "+
			"code-indent convention drifted, and this test would pass vacuously", startMarker, endMarker, sqlPath)
	}
	return strings.Join(code, "\n")
}

func accountActivitySQLPath(t *testing.T) string {
	t.Helper()
	path := filepath.Join(systemdRepoRoot(t), "deploy", "clickhouse", "account_activity.sql")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return path
}

// shippedHeavyJobWrapper returns the content of the ansible copy task that
// installs /usr/local/sbin/run-heavy-job.sh — the script r1 actually runs.
func shippedHeavyJobWrapper(t *testing.T) string {
	t.Helper()
	path := filepath.Join(systemdRepoRoot(t),
		"configs", "ansible", "roles", "archival-node", "tasks", "14-stellarindex-services.yml")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var tasks []map[string]any
	if err := yaml.Unmarshal(b, &tasks); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	for _, task := range tasks {
		cp, ok := task["ansible.builtin.copy"].(map[string]any)
		if !ok || cp["dest"] != runbookWrapperPath {
			continue
		}
		if content, ok := cp["content"].(string); ok && strings.Contains(content, "skipping this fire") {
			return content
		}
	}
	t.Fatalf("no ansible.builtin.copy task with dest %s (and the lock-skip branch) in %s", runbookWrapperPath, path)
	return ""
}

// Stubs for the binaries the wrapper and the runbook call. flock fails on
// its AA_FLOCK_FAIL_ON-th call (the wrapper's "lock held" branch); id
// answers AA_UID so both wrapper branches run on any machine; systemd-run
// drops its own options and execs the command, as a scope does.
const (
	stubFlock = `#!/bin/bash
n=$(( $(cat "$AA_FLOCK_COUNT" 2>/dev/null || echo 0) + 1 ))
echo "$n" > "$AA_FLOCK_COUNT"
[ "$n" = "${AA_FLOCK_FAIL_ON:-}" ] && exit 1
exit 0
`
	stubID = `#!/bin/bash
echo "${AA_UID:-1000}"
`
	stubSystemdRun = `#!/bin/bash
while [ "$#" -gt 0 ]; do
  case "$1" in
    --scope) shift ;;
    --unit|-p) shift 2 ;;
    *) break ;;
  esac
done
exec "$@"
`
	// clickhouse-client: logs one line per query, then answers by query kind.
	stubClickHouseClient = `#!/bin/bash
q="${!#}"
printf '%s' "$q" | tr '\n' ' ' >> "$AA_LOG"; echo >> "$AA_LOG"
case "$q" in
  *"FROM stellar.ledgers"*)
    [ -n "${AA_TIP_RC:-}" ] && exit "$AA_TIP_RC"
    [ -n "${AA_TIP_OUT:-}" ] && echo "$AA_TIP_OUT"
    exit 0 ;;
  *"countIf(wm < truth)"*)
    [ -n "${AA_VERIFY_RC:-}" ] && exit "$AA_VERIFY_RC"
    case "$q" in *"ledger_seq >= ${AA_BAD_WINDOW:-none} "*) echo 3 ;; *) echo 0 ;; esac
    exit 0 ;;
  *INSERT*)
    if [ -n "${AA_FAIL_MATCH:-}" ]; then
      case "$q" in *"$AA_FAIL_MATCH"*) echo "Code: 241. DB::Exception: MEMORY_LIMIT_EXCEEDED" >&2; exit 241 ;; esac
    fi
    exit 0 ;;
esac
echo "clickhouse-client stub: unexpected query: $q" >&2
exit 99
`
)

type runbookResult struct {
	ok      bool   // the pasted block's exit status was 0
	out     string // stdout + stderr
	queries []string
}

// runRunbook executes one runbook section through the shipped wrapper with
// the stubs above. nested reproduces a paste into a nested shell: an outer
// bash reads `bash` and then the block from the same stdin, so whatever the
// inner shell does not consume is run by the outer one.
func runRunbook(t *testing.T, section string, nested bool, env ...string) runbookResult {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(filepath.Join(dir, "lock"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	files := map[string]string{
		"run-heavy-job.sh": shippedHeavyJobWrapper(t), "flock": stubFlock, "id": stubID,
		"systemd-run": stubSystemdRun, "clickhouse-client": stubClickHouseClient,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(body), 0o755); err != nil { //nolint:gosec // test stub must be executable
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if !strings.Contains(section, runbookWrapperPath) {
		t.Fatalf("runbook section never calls %s — a heavy op on the serving host must run under the wrapper",
			runbookWrapperPath)
	}
	script := strings.ReplaceAll(section, runbookWrapperPath, filepath.Join(bin, "run-heavy-job.sh"))

	logPath := filepath.Join(dir, "queries.log")
	outPath := filepath.Join(dir, "out.txt")
	// Output goes to a FILE, not a pipe: in its root branch the wrapper leaves
	// a watchdog `sleep 30` holding stdout, and a pipe would wait on it.
	outFile, err := os.Create(outPath)
	if err != nil {
		t.Fatalf("create %s: %v", outPath, err)
	}
	defer outFile.Close()

	stdin := script + "\n"
	if nested {
		stdin = "bash\n" + stdin
	}
	cmd := exec.Command("bash")
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Stdout, cmd.Stderr = outFile, outFile
	// An operator's paste runs outside systemd; a CI runner that is itself a
	// unit must not flip the wrapper into its unit-fire branch.
	cmd.Env = append(envWithout(os.Environ(), "INVOCATION_ID"),
		"PATH="+bin+":"+os.Getenv("PATH"), "TMPDIR="+dir,
		"HEAVY_JOB_LOCK_DIR="+filepath.Join(dir, "lock"), "HEAVY_JOB_OPS_ENV="+filepath.Join(dir, "absent"),
		"AA_LOG="+logPath, "AA_FLOCK_COUNT="+filepath.Join(dir, "flock.count"))
	cmd.Env = append(cmd.Env, env...)
	runErr := cmd.Run()

	out, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read %s: %v", outPath, err)
	}
	res := runbookResult{ok: runErr == nil, out: string(out)}
	if logged, err := os.ReadFile(logPath); err == nil {
		res.queries = strings.Split(strings.TrimSpace(string(logged)), "\n")
	}
	return res
}

func envWithout(env []string, key string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		if !strings.HasPrefix(kv, key+"=") {
			out = append(out, kv)
		}
	}
	return out
}

func runbookQueryCount(queries []string, substr string) int {
	n := 0
	for _, q := range queries {
		if strings.Contains(q, substr) {
			n++
		}
	}
	return n
}

type runbookCase struct {
	name    string
	nested  bool
	env     []string
	wantOK  bool
	want    []string // substrings the output must contain
	inserts int      // Step 2: INSERTs that must have reached clickhouse-client
	checks  int      // Step 3: verify queries that must have reached clickhouse-client
}

// assertRunbookCase runs one case under both wrapper branches (non-root
// exec, root systemd-run scope) and holds the fail-closed contract: the
// success line prints if and ONLY if the block exits 0.
func assertRunbookCase(t *testing.T, section, doneLine string, tc runbookCase) {
	t.Helper()
	for _, uid := range []string{"1000", "0"} {
		res := runRunbook(t, section, tc.nested, append([]string{"AA_UID=" + uid}, tc.env...)...)
		if res.ok != tc.wantOK {
			t.Errorf("uid %s: exit ok = %v, want %v (F112: a run that did not cover every window must end "+
				"non-zero)\n%s", uid, res.ok, tc.wantOK, res.out)
		}
		if got := strings.Contains(res.out, doneLine); got != tc.wantOK {
			t.Errorf("uid %s: success line %q printed = %v, want %v\n%s", uid, doneLine, got, tc.wantOK, res.out)
		}
		for _, w := range tc.want {
			if !strings.Contains(res.out, w) {
				t.Errorf("uid %s: output lacks %q\n%s", uid, w, res.out)
			}
		}
		if got := runbookQueryCount(res.queries, "INSERT INTO stellar.account_activity"); got != tc.inserts {
			t.Errorf("uid %s: %d backfill INSERTs reached clickhouse-client, want %d\n%s", uid, got, tc.inserts, res.out)
		}
		if got := runbookQueryCount(res.queries, "countIf(wm < truth)"); got != tc.checks {
			t.Errorf("uid %s: %d verify queries reached clickhouse-client, want %d\n%s", uid, got, tc.checks, res.out)
		}
	}
}

// TestAccountActivityRunbook_Step2FailsClosed: TIP 4000001 is two windows
// (2 and 2000002) of three jobs each.
func TestAccountActivityRunbook_Step2FailsClosed(t *testing.T) {
	section := extractRunbookSection(t, accountActivitySQLPath(t),
		"Step 2: windowed historical backfill", "Step 3: verify")
	const tip = "AA_TIP_OUT=4000001"
	cases := []runbookCase{
		{name: "complete run", env: []string{tip}, wantOK: true, inserts: 6, want: []string{"6 of 6 jobs ran to success"}},
		{
			name: "one job of a window fails", env: []string{tip, "AA_FAIL_MATCH=FROM stellar.transactions"},
			inserts: 2, want: []string{"tx window 2 FAILED"},
		},
		{name: "TIP probe exits non-zero", env: []string{"AA_TIP_RC=210"}, want: []string{"is not a ledger number"}},
		{name: "TIP probe answers nothing", want: []string{"is not a ledger number"}},
		{name: "TIP probe answers 0", env: []string{"AA_TIP_OUT=0"}, want: []string{"is not a ledger number"}},
		{
			name: "TIP probe answers an error string", env: []string{"AA_TIP_OUT=Code: 210. DB::NetException"},
			want: []string{"is not a ledger number"},
		},
		{name: "TIP below the first window", env: []string{"AA_TIP_OUT=1"}, want: []string{"0 of 0 jobs"}},
		// The shipped wrapper exits 75 here in both branches, without running the payload.
		{
			name: "per-job lock held", env: []string{tip, "AA_FLOCK_FAIL_ON=2"}, inserts: 1,
			want: []string{"refusing to start acct-activity-tx-2", "tx window 2 DID NOT RUN (the wrapper exited 75"},
		},
		{
			name: "per-job lock held, systemd-launched", env: []string{tip, "AA_FLOCK_FAIL_ON=2", "INVOCATION_ID=0123456789abcdef"},
			inserts: 1, want: []string{"skipping this fire", "tx window 2 DID NOT RUN (the wrapper exited 75"},
		},
		{
			name: "job fails, pasted into a nested shell", nested: true,
			env: []string{tip, "AA_FAIL_MATCH=GROUP BY source_account"}, inserts: 1, want: []string{"ops window 2 FAILED"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assertRunbookCase(t, section, step2Done, tc) })
	}
}

// TestAccountActivityRunbook_Step3FailsClosed: the verify must name a bad
// LATER window, keep checking the rest, and never read "did not run" as 0.
func TestAccountActivityRunbook_Step3FailsClosed(t *testing.T) {
	section := extractRunbookSection(t, accountActivitySQLPath(t), "Step 3: verify", "Expect PASSED")
	const tip = "TIP=4000001"
	cases := []runbookCase{
		{
			name: "every window clean", env: []string{tip}, wantOK: true, checks: 2,
			want: []string{"2 window(s) checked, 0 with a too-LOW watermark"},
		},
		{
			name: "gap in a later window", env: []string{tip, "AA_BAD_WINDOW=2000002"}, checks: 2,
			want: []string{"window 2000002 has 3 sampled account(s)", "2 window(s) checked, 1 with"},
		},
		{
			name: "gap in the first window does not stop the rest", env: []string{tip, "AA_BAD_WINDOW=2"}, checks: 2,
			want: []string{"window 2 has 3 sampled account(s)"},
		},
		{name: "TIP unset", env: []string{"TIP="}, want: []string{"is not a ledger number"}},
		{
			name: "verify query fails", env: []string{tip, "AA_VERIFY_RC=241"}, checks: 1,
			want: []string{"window 2 FAILED to run"},
		},
		{
			name: "per-job lock held", env: []string{tip, "AA_FLOCK_FAIL_ON=2"}, checks: 1,
			want: []string{"refusing to start acct-activity-verify-2000002", "window 2000002 DID NOT RUN (the wrapper exited 75"},
		},
		{
			name: "per-job lock held, systemd-launched", env: []string{tip, "AA_FLOCK_FAIL_ON=2", "INVOCATION_ID=0123456789abcdef"},
			checks: 1, want: []string{"skipping this fire", "window 2000002 DID NOT RUN (the wrapper exited 75"},
		},
		{name: "sample modulus 0", env: []string{tip, "AA_MOD=0"}, want: []string{"is not a positive integer"}},
		{
			name: "gap, pasted into a nested shell", nested: true, env: []string{tip, "AA_BAD_WINDOW=2000002"}, checks: 2,
			want: []string{"window 2000002 has 3 sampled account(s)"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { assertRunbookCase(t, section, step3Done, tc) })
	}
}

// TestAccountActivityRunbook_Step3IsBounded: the verify is a heavy op on the
// serving host. Every read of a lake source table must be scoped by ledger
// (partition pruning) and the statement capped — never again an unbounded
// GROUP BY over stellar.transactions.
func TestAccountActivityRunbook_Step3IsBounded(t *testing.T) {
	section := extractRunbookSection(t, accountActivitySQLPath(t), "Step 3: verify", "Expect PASSED")
	arms := strings.Split(section, "FROM stellar.")[1:]
	sources := 0
	for _, arm := range arms {
		table := strings.Fields(arm)[0]
		if table == "account_activity" {
			continue
		}
		sources++
		if next := strings.Index(arm, "UNION ALL"); next >= 0 {
			arm = arm[:next]
		}
		if !strings.Contains(arm, "ledger_seq >= $W AND ledger_seq < $((W + 2000000))") {
			t.Errorf("Step-3 read of stellar.%s is not scoped to the window's ledger range:\n%s", table, arm)
		}
	}
	if sources != 3 {
		t.Errorf("Step-3 verify reads %d lake source tables, want 3 (operations, transactions, "+
			"operation_participants — the MV sources; never narrow the set)", sources)
	}
	for _, w := range []string{"SETTINGS max_threads = 4, max_memory_usage = 8000000000", "cityHash64(account_id) % $AA_MOD = 0"} {
		if !strings.Contains(section, w) {
			t.Errorf("Step-3 verify lacks %q", w)
		}
	}
}
