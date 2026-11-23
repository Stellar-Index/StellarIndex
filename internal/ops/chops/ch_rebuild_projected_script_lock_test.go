package chops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// flockShim implements `flock [-n] FD` for hosts without util-linux flock.
const flockShim = `#!/usr/bin/perl
use Fcntl qw(:flock);
my $nb = 0;
if ($ARGV[0] eq '-n') { $nb = 1; shift @ARGV; }
open(my $fh, '<&=', $ARGV[0]) or exit 1;
exit(flock($fh, LOCK_EX | ($nb ? LOCK_NB : 0)) ? 0 : 1);
`

// A second run while one holds the state lock must touch nothing and exit 75, and every exit leaves its code in the log: a
// concurrent run would otherwise read the first run's in-flight $DIRTY line as a hole and DELETE that window under it.
func TestProjectedScript_ConcurrentRunIsExcluded(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "state", "rebuild-done-windows.txt")
	if err := os.MkdirAll(filepath.Dir(state), 0o755); err != nil {
		t.Fatal(err)
	}
	lock, err := os.OpenFile(state+".lock", os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	held := runProjectedScript(t, dir, nil)
	if held.exit != 75 {
		t.Errorf("run while the lock is held: exit %d, want 75\n%s", held.exit, held.log)
	}
	if len(held.calls) != 0 {
		t.Errorf("run while the lock is held made %d calls (%s), want none", len(held.calls), held.sequence())
	}
	if !strings.Contains(held.log, "EXIT rc=75") {
		t.Errorf("log does not record the exit code:\n%s", held.log)
	}

	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	free := runProjectedScript(t, dir, nil)
	if !strings.Contains(free.log, "EXIT rc=") || len(free.calls) == 0 {
		t.Errorf("run after the lock was released did not proceed (exit %d, %d calls):\n%s", free.exit, len(free.calls), free.log)
	}
}

// Under run-heavy-job.sh the payload inherits the per-job lock on fd 9, so the script must keep it held for its whole run; and a
// run killed by SIGTERM (systemd stop, the wrapper's watchdog) must log its real exit code, not rc=0.
func TestProjectedScript_KeepsWrapperLockAndLogsSignalExit(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ops-started")
	slowOps := filepath.Join(dir, "slow-ops")
	if err := os.WriteFile(slowOps, []byte("#!/usr/bin/env bash\n: > \"$STUB_MARKER\"\nexec sleep 30\n"), 0o755); err != nil { //nolint:gosec // an executable test stub
		t.Fatal(err)
	}
	jobLock := filepath.Join(dir, "job.lock")
	held, err := os.OpenFile(jobLock, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd, vars := projectedScriptCmd(ctx, t, dir, map[string]string{"OPS": slowOps, "STUB_MARKER": marker})
	cmd.ExtraFiles = []*os.File{nil, nil, nil, nil, nil, nil, held} // fd 9, as the wrapper's exec leaves it
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := held.Close(); err != nil { // the payload is now the lock's only holder, as after the wrapper's exec
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("the script never reached its first ops call")
		}
		time.Sleep(20 * time.Millisecond)
	}

	probe, err := os.Open(jobLock) //nolint:gosec // test temp file
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		t.Error("the wrapper's per-job lock on fd 9 was released mid-run: the script reused fd 9 for its own lock")
	}
	_ = probe.Close()

	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if log := readIfExists(t, vars["LOG"]); !strings.Contains(log, "EXIT rc=143") {
		t.Errorf("a SIGTERM'd run must log EXIT rc=143:\n%s", log)
	}
}
