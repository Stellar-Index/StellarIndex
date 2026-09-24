//go:build linux || darwin

package archive

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
)

// The trim (compute-trim-cutoff.sh -> trim-galexie-archive) and the hourly
// fill (galexie-archive-fill.sh) share one host archive. These tests run the
// REAL shipped scripts against stub psql/mc binaries (#696).

const (
	fillScript   = "configs/ansible/roles/archival-node/files/galexie-archive-fill.sh"
	cutoffScript = "configs/ansible/roles/archival-node/files/compute-trim-cutoff.sh"
)

// fillHarness is one sandboxed host: stub binaries, the fill's and trim's
// state files, and the logs the stubs append to.
type fillHarness struct {
	dir, bin, envFile, trimEnv, floorFile, mirrored, mcCalls, lock string
}

func newFillHarness(t *testing.T) *fillHarness {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not on PATH")
	}
	dir := t.TempDir()
	h := &fillHarness{
		dir:       dir,
		bin:       filepath.Join(dir, "bin"),
		floorFile: filepath.Join(dir, "state", "hot-floor"),
		mirrored:  filepath.Join(dir, "mirrored.txt"),
		mcCalls:   filepath.Join(dir, "mc-calls.txt"),
		lock:      filepath.Join(dir, "fill.lock"),
		envFile:   filepath.Join(dir, "stellarindex.env"),
		trimEnv:   filepath.Join(dir, "trim.env"),
	}
	if err := os.Mkdir(h.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	writeStub(t, h.bin, "psql", "#!/usr/bin/env bash\necho \"$STUB_TIP\"\n")
	writeStub(t, h.bin, "mc", `#!/usr/bin/env bash
echo "$*" >> "$STUB_MC_CALLS"
target="${*: -1}"
case "$1" in
  ls)
    case "$target" in
      aws-public/*/pubnet/) cat "$STUB_AWS_LIST" ;;
      local/galexie-archive/) cat "$STUB_LOCAL_LIST" ;;
    esac ;;
  mirror) echo "$target" >> "$STUB_MIRRORED" ;;
esac
`)
	h.portableTools(t)
	return h
}

// portableTools makes the GNU/util-linux tools the script needs resolvable on
// a macOS dev box: GNU xargs (-a) and flock(1). flock is provided as a
// flock(2) call on the inherited fd — the same syscall util-linux makes.
func (h *fillHarness) portableTools(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "linux" {
		return
	}
	gx, err := exec.LookPath("gxargs")
	if err != nil {
		t.Skip("GNU xargs (gxargs) not available on this non-Linux host")
	}
	if err := os.Symlink(gx, filepath.Join(h.bin, "xargs")); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("flock"); err == nil {
		return
	}
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("neither flock(1) nor python3 available")
	}
	writeStub(t, h.bin, "flock", `#!/usr/bin/env python3
import fcntl, sys
try:
    fcntl.flock(int(sys.argv[-1]), fcntl.LOCK_EX | fcntl.LOCK_NB)
except OSError:
    sys.exit(1)
`)
}

func writeStub(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (h *fillHarness) env(extra ...string) []string {
	return append(os.Environ(),
		append([]string{
			"PATH=" + h.bin + string(os.PathListSeparator) + os.Getenv("PATH"),
			"TMPDIR=" + h.dir,
			"STUB_MC_CALLS=" + h.mcCalls,
			"STUB_MIRRORED=" + h.mirrored,
			"PARTIAL_CHECK_WINDOW=0",
		}, extra...)...)
}

// run executes a sandboxed copy of the shipped script: its host paths are
// rewritten into the harness dir, nothing else changes. The paths the run
// cannot do without must be present; the floor file and lock are rewritten
// when the script names them.
func (h *fillHarness) run(t *testing.T, script string, extra ...string) (string, error) {
	t.Helper()
	body := readRepoFile(t, script)
	for _, p := range []struct {
		host, sandbox string
		required      bool
	}{
		{"/etc/default/stellarindex", h.envFile, script == cutoffScript},
		{"/run/galexie-archive-trim.env", h.trimEnv, script == cutoffScript},
		{"/var/log/galexie-mirror.log", filepath.Join(h.dir, "fill.log"), script == fillScript},
		{"/etc/default/galexie-archive-fill", filepath.Join(h.dir, "no-such-defaults"), script == fillScript},
		{"/var/lib/galexie-archive/hot-floor", h.floorFile, false},
		{"/run/lock/galexie-archive-fill.lock", h.lock, false},
	} {
		if p.required && !strings.Contains(body, p.host) {
			t.Fatalf("%s no longer names %s; update the sandbox", script, p.host)
		}
		body = strings.ReplaceAll(body, p.host, p.sandbox)
	}
	sandboxed := filepath.Join(h.dir, filepath.Base(script))
	if err := os.WriteFile(sandboxed, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", sandboxed)
	cmd.Env = h.env(extra...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (h *fillHarness) writeList(t *testing.T, name string, partitions ...string) string {
	t.Helper()
	var b strings.Builder
	for _, p := range partitions {
		b.WriteString("[2026-09-01 00:00:00 UTC]     0B " + p + "/\n")
	}
	path := filepath.Join(h.dir, name)
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

// TestTrimCutoffBecomesFillFloor: after the trim's ExecStartPre computes its
// cutoff, the next fill must not re-mirror any partition the trim is about to
// delete (every file below the cutoff). With the floor sourced from a static
// inventory var (default 0) instead, the fill re-downloads the whole trimmed
// range every hour.
func TestTrimCutoffBecomesFillFloor(t *testing.T) {
	t.Parallel()
	h := newFillHarness(t)

	if err := os.WriteFile(h.envFile, []byte("STELLARINDEX_POSTGRES_DSN=postgres://stub\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// tip 3,000,000 - 1,555,200 = cutoff 1,444,800.
	out, err := h.run(t, cutoffScript,
		"STUB_TIP=3000000")
	if err != nil {
		t.Fatalf("compute-trim-cutoff.sh: %v\n%s", err, out)
	}
	if got := readLines(t, h.trimEnv); !slices.Equal(got, []string{"TRIM_CUTOFF=1444800"}) {
		t.Fatalf("trim env = %v, want TRIM_CUTOFF=1444800", got)
	}

	const (
		genesis    = "FFFFFFFF--0-63999"
		below      = "FFEBFFFF--1280000-1343999"
		straddling = "FFE9FFFF--1408000-1471999"
		above      = "FFD5FFFF--2688000-2751999"
	)
	aws := h.writeList(t, "aws.list", genesis, below, straddling, above)
	local := h.writeList(t, "local.list")
	out, err = h.run(t, fillScript, "STUB_AWS_LIST="+aws, "STUB_LOCAL_LIST="+local)
	if err != nil {
		t.Fatalf("galexie-archive-fill: %v\n%s", err, out)
	}
	got := readLines(t, h.mirrored)
	slices.Sort(got)
	want := []string{
		"local/galexie-archive/" + above + "/",
		"local/galexie-archive/" + straddling + "/",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("fill mirrored %v, want only the partitions at/above the trim cutoff %v — "+
			"anything below it is deleted by the next trim and re-downloaded by the next fill", got, want)
	}

	// The floor only rises: a later, lower cutoff trims less but must not
	// hand the fill back a range that was already trimmed.
	if out, err := h.run(t, cutoffScript,
		"STUB_TIP=2000000"); err != nil {
		t.Fatalf("compute-trim-cutoff.sh (lower tip): %v\n%s", err, out)
	}
	if got := readLines(t, h.floorFile); !slices.Equal(got, []string{"1444800"}) {
		t.Fatalf("hot floor after a lower cutoff = %v, want it held at 1444800", got)
	}
}

// TestFillRefusesConcurrentRun: a second fill (a manual PARTIALS=... run while
// the timer's run is mid-mirror) must not start, whoever invoked it — its
// work lists would otherwise be rewritten under the first run's xargs.
func TestFillRefusesConcurrentRun(t *testing.T) {
	t.Parallel()
	h := newFillHarness(t)

	f, err := os.OpenFile(h.lock, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	aws := h.writeList(t, "aws.list", "FFD5FFFF--2688000-2751999")
	local := h.writeList(t, "local.list")
	out, err := h.run(t, fillScript, "STUB_AWS_LIST="+aws, "STUB_LOCAL_LIST="+local)
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 75 {
		t.Fatalf("fill with its lock held: err=%v, want exit 75\n%s", err, out)
	}
	if calls := readLines(t, h.mcCalls); len(calls) != 0 {
		t.Fatalf("fill touched mc while another run held the lock: %v", calls)
	}
}
