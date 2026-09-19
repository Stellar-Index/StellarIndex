package controlwiring

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ─── RWC-529 / GH #529: the WAL-headroom guard must measure pg_wal ──
//
// The archival-node role refuses a max_wal_size that does not fit the
// filesystem pg_wal is ACTUALLY on — the substitution that took r1 down
// on 2026-09-16 (the reasoning said "2.7TB free", which was true of the
// DATA directory and false of pg_wal, a symlink onto the 49GB root).
//
// The probe resolves the symlink, then walks up to the nearest EXISTING
// ancestor so df still names the right volume on a fresh host. The
// defect: `du -sm` ran on that walked-up ancestor and its result was fed
// to the assert as "MB already occupied by WAL", which the assert ADDS
// to free space. On a dangling pg_wal symlink the walk can reach `/`, so
// avail+used approaches the whole volume's SIZE and the guard clears the
// exact configuration it exists to refuse.
//
// Two legs, both executing the shipped artifact:
//
//  1. the probe's real shell body, run against stub trees (real
//     directory / symlink to a real directory / DANGLING symlink /
//     absent) — asserting du measures pg_wal itself and nothing else;
//  2. the file's real assert tasks, run through ansible-playbook against
//     the tuple leg 1 produced — asserting a dangling symlink is
//     REFUSED, and that a healthy host is still admitted.
//
// df is stubbed because the probe uses GNU `df -BM` (the hosts are
// Ubuntu) and the developer machines are macOS; readlink, du and the
// directory shapes are real, and they are where the defect lives.

const walPostgresTasks = "configs/ansible/roles/archival-node/tasks/05-postgres.yml"

// walProbeTaskName / walProdWalPath identify the task under test. A
// rename must re-derive this test rather than silently skip it.
const (
	walProbeTaskName = "Resolve the filesystem pg_wal is really on"
	walProdWalPath   = "/var/lib/postgresql/{{ postgres_version }}/main/pg_wal"
)

// walStubAvailMB is what the stubbed df reports as free. It is large
// enough that the headroom arithmetic passes on free space alone, so a
// refusal in leg 2 can only come from the dangling-symlink condition.
const walStubAvailMB = 100000

// walTasks returns every task node in the postgres task file.
func walTasks(t *testing.T) []yaml.Node {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(walPostgresTasks)))
	if err != nil {
		t.Fatalf("read %s: %v", walPostgresTasks, err)
	}
	var tasks []yaml.Node
	if err := yaml.Unmarshal(raw, &tasks); err != nil {
		t.Fatalf("parse %s: %v", walPostgresTasks, err)
	}
	if len(tasks) == 0 {
		t.Fatalf("%s parsed to zero tasks", walPostgresTasks)
	}
	return tasks
}

// walTaskMap decodes one task node into a generic map.
func walTaskMap(t *testing.T, n yaml.Node) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := n.Decode(&m); err != nil {
		t.Fatalf("decode task: %v", err)
	}
	return m
}

// walProbeBody extracts the probe task's shell body verbatim.
func walProbeBody(t *testing.T) string {
	t.Helper()
	for _, n := range walTasks(t) {
		m := walTaskMap(t, n)
		if name, _ := m["name"].(string); name != walProbeTaskName {
			continue
		}
		body, ok := m["ansible.builtin.shell"].(string)
		if !ok || strings.TrimSpace(body) == "" {
			t.Fatalf("task %q has no ansible.builtin.shell body", walProbeTaskName)
		}
		return body
	}
	t.Fatalf("task %q not found in %s — re-derive this test", walProbeTaskName, walPostgresTasks)
	return ""
}

// walAssertTasks returns every ansible.builtin.assert task in the file,
// re-serialised as a playbook task list. These are the guard's refusal
// conditions exactly as shipped.
func walAssertTasks(t *testing.T) []yaml.Node {
	t.Helper()
	var out []yaml.Node
	for _, n := range walTasks(t) {
		if _, ok := walTaskMap(t, n)["ansible.builtin.assert"]; ok {
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		t.Fatalf("no ansible.builtin.assert task in %s — re-derive this test", walPostgresTasks)
	}
	return out
}

// walRoleDefault reads one key out of the archival-node role defaults.
func walRoleDefault(t *testing.T, key string) any {
	t.Helper()
	const defaultsFile = "configs/ansible/roles/archival-node/defaults/main.yml"
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), filepath.FromSlash(defaultsFile)))
	if err != nil {
		t.Fatalf("read %s: %v", defaultsFile, err)
	}
	defaults := map[string]any{}
	if err := yaml.Unmarshal(raw, &defaults); err != nil {
		t.Fatalf("parse %s: %v", defaultsFile, err)
	}
	v, ok := defaults[key]
	if !ok {
		t.Fatalf("%s has no %s — re-derive this test", defaultsFile, key)
	}
	return v
}

// walStubTree builds a stub cluster directory and returns the path the
// probe should be pointed at. Every shape puts BALLAST outside pg_wal —
// in the cluster directory and at the tree root — so a du that measured
// an ancestor instead of pg_wal reports a large non-zero number.
func walStubTree(t *testing.T, shape string) string {
	t.Helper()
	root := t.TempDir()
	main := filepath.Join(root, "main")
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", main, err)
	}
	writeMB(t, filepath.Join(root, "root-ballast.bin"), 6)
	writeMB(t, filepath.Join(main, "cluster-ballast.bin"), 6)

	wal := filepath.Join(main, "pg_wal")
	switch shape {
	case "dir":
		if err := os.Mkdir(wal, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", wal, err)
		}
		writeMB(t, filepath.Join(wal, "000000010000000000000001"), 3)
	case "symlink":
		target := filepath.Join(root, "pgwal-volume", "pg_wal")
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", target, err)
		}
		writeMB(t, filepath.Join(target, "000000010000000000000001"), 3)
		if err := os.Symlink(target, wal); err != nil {
			t.Fatalf("symlink %s: %v", wal, err)
		}
	case "dangling":
		// The 2026-09-16 shape before the volume exists: pg_wal names a
		// mount that has not been applied.
		if err := os.Symlink(filepath.Join(root, "unmounted-volume", "pg_wal"), wal); err != nil {
			t.Fatalf("symlink %s: %v", wal, err)
		}
	case "absent":
		// Fresh host, pg_wal not created yet.
	default:
		t.Fatalf("unknown stub shape %q", shape)
	}
	return wal
}

func writeMB(t *testing.T, path string, mb int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, mb*1024*1024), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// walStubDF writes a df stub that answers both invocations the probe
// makes (`df -P -BM <path>` and `df -P <path>`) with a fixed volume.
func walStubDF(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
echo "Filesystem 1024-blocks Used Available Capacity Mounted on"
echo "/dev/stub-wal-volume 200000 100000 %d 50%% /"
`, walStubAvailMB)
	path := filepath.Join(dir, "df")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write df stub: %v", err)
	}
	return dir
}

// walRunProbe runs the shipped probe body against walPath and returns
// its whitespace-separated output fields.
func walRunProbe(t *testing.T, walPath string) []string {
	t.Helper()
	body := walProbeBody(t)
	if !strings.Contains(body, walProdWalPath) {
		t.Fatalf("probe body no longer contains %q — re-derive this test:\n%s", walProdWalPath, body)
	}
	script := strings.ReplaceAll(body, walProdWalPath, walPath)

	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), "PATH="+walStubDF(t)+string(os.PathListSeparator)+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe failed: %v\noutput: %s\nscript:\n%s", err, out, script)
	}
	fields := strings.Fields(string(out))
	if len(fields) < 4 {
		t.Fatalf("probe emitted %d fields, want at least 4: %q", len(fields), string(out))
	}
	return fields
}

func TestRWC529_WalProbeMeasuresPgWalNotAWalkedUpAncestor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		shape string
		// wantUsedMB: exact when >= 0; -1 means "roughly the 3MB of
		// segments planted inside pg_wal".
		wantUsedMB  int
		wantState   string
		wantPathIs  string // "wal" = the resolved pg_wal dir, "ancestor" = an existing ancestor
		explanation string
	}{
		{
			shape: "dir", wantUsedMB: -1, wantState: "dir", wantPathIs: "wal",
			explanation: "a real pg_wal directory: du measures its segments",
		},
		{
			shape: "symlink", wantUsedMB: -1, wantState: "dir", wantPathIs: "wal",
			explanation: "a symlink to a real directory: df and du both follow it",
		},
		{
			shape: "dangling", wantUsedMB: 0, wantState: "dangling", wantPathIs: "ancestor",
			explanation: "a dangling pg_wal symlink: NOTHING is already WAL, so nothing may be credited",
		},
		{
			shape: "absent", wantUsedMB: 0, wantState: "absent", wantPathIs: "ancestor",
			explanation: "pg_wal not created yet: the cluster directory's contents are not WAL capacity",
		},
	}

	for _, tc := range cases {
		t.Run(tc.shape, func(t *testing.T) {
			t.Parallel()
			walPath := walStubTree(t, tc.shape)
			fields := walRunProbe(t, walPath)

			used, err := strconv.Atoi(fields[3])
			if err != nil {
				t.Fatalf("used field %q is not a number: %v", fields[3], err)
			}
			switch {
			case tc.wantUsedMB == 0 && used != 0:
				t.Errorf("%s: probe credited %dMB as 'already WAL' from outside pg_wal; want 0 (%s)",
					tc.shape, used, tc.explanation)
			case tc.wantUsedMB < 0 && (used < 2 || used > 5):
				t.Errorf("%s: probe reported %dMB used, want ~3MB of planted segments (%s)",
					tc.shape, used, tc.explanation)
			}

			if tc.wantPathIs == "wal" {
				resolved, err := filepath.EvalSymlinks(walPath)
				if err != nil {
					t.Fatalf("resolve %s: %v", walPath, err)
				}
				if got := evalOrSelf(fields[0]); got != resolved {
					t.Errorf("%s: probe reported path %q, want the pg_wal directory %q", tc.shape, got, resolved)
				}
			}

			if len(fields) < 5 {
				t.Fatalf("%s: probe emitted no pg_wal state field (%d fields: %q); a dangling symlink "+
					"cannot be told from a healthy one", tc.shape, len(fields), fields)
			}
			if fields[4] != tc.wantState {
				t.Errorf("%s: probe reported state %q, want %q (%s)", tc.shape, fields[4], tc.wantState, tc.explanation)
			}
		})
	}
}

// evalOrSelf canonicalises p (macOS /var is a symlink to /private/var)
// and falls back to p when it cannot be resolved.
func evalOrSelf(p string) string {
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return p
	}
	return r
}

// walRunAssert feeds a probe tuple through the file's REAL assert tasks
// and reports whether the guard admitted the configuration.
func walRunAssert(t *testing.T, bin, tuple, maxWALSize string) (admitted bool, output string) {
	t.Helper()
	dir := t.TempDir()

	play := []map[string]any{{
		"hosts":        "localhost",
		"connection":   "local",
		"gather_facts": false,
		"vars": map[string]any{
			// The role's own default, not a literal: the guard's
			// messages interpolate it.
			"postgres_version":      walRoleDefault(t, "postgres_version"),
			"postgres_max_wal_size": maxWALSize,
			"pg_wal_fs":             map[string]any{"stdout": tuple},
		},
		"tasks": walAssertTasks(t),
	}}
	buf, err := yaml.Marshal(play)
	if err != nil {
		t.Fatalf("marshal playbook: %v", err)
	}
	pb := filepath.Join(dir, "guard.yml")
	if err := os.WriteFile(pb, buf, 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}

	cmd := exec.Command(bin, "-i", "localhost,", pb)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"ANSIBLE_CONFIG="+filepath.Join(dir, "none.cfg"),
		"ANSIBLE_LOCALHOST_WARNING=False",
		"ANSIBLE_INVENTORY_UNPARSED_WARNING=False",
		"ANSIBLE_DEPRECATION_WARNINGS=False",
	)
	out, err := cmd.CombinedOutput()
	return err == nil, string(out)
}

func TestRWC529_GuardRefusesADanglingPgWalSymlink(t *testing.T) {
	t.Parallel()

	bin, err := exec.LookPath("ansible-playbook")
	if err != nil {
		t.Skipf("ansible-playbook not on PATH: %v (the probe leg, "+
			"TestRWC529_WalProbeMeasuresPgWalNotAWalkedUpAncestor, runs unconditionally)", err)
	}

	// The tuple is not hand-written: it is what the shipped probe
	// actually emits for a pg_wal symlink whose volume is not there.
	dangling := strings.Join(walRunProbe(t, walStubTree(t, "dangling")), " ")
	admitted, out := walRunAssert(t, bin, dangling, "16GB")
	if admitted {
		t.Errorf("guard ADMITTED max_wal_size=16GB on a dangling pg_wal symlink (probe tuple %q); "+
			"this is the 2026-09-16 shape the guard exists to refuse\n%s", dangling, out)
	}

	// Positive control: a healthy host must still be admitted, so the
	// refusal above cannot be bought by failing everything.
	healthy := strings.Join(walRunProbe(t, walStubTree(t, "symlink")), " ")
	admitted, out = walRunAssert(t, bin, healthy, "2GB")
	if !admitted {
		t.Errorf("guard REFUSED a healthy pg_wal directory with ample room (probe tuple %q)\n%s", healthy, out)
	}
}
