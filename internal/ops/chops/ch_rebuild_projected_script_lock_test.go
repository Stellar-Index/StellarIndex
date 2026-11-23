package chops

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
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
