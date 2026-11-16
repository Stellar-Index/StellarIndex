package atomicfile

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// recordOpens swaps openFile for one that records each call's flags.
func recordOpens(t *testing.T) *[]int {
	t.Helper()
	var flags []int
	openFile = func(name string, flag int, perm os.FileMode) (*os.File, error) {
		flags = append(flags, flag)
		return os.OpenFile(name, flag, perm) //nolint:gosec // test path under t.TempDir
	}
	t.Cleanup(func() { openFile = os.OpenFile })
	return &flags
}

// An existing lock must be opened without O_CREAT. In a sticky group-writable directory Linux refuses an O_CREAT open
// of a file owned by neither the caller nor the directory's owner, root included, so a root run of a rollup job could
// not take the lock the service user had created. The kernel rule needs root and a sysctl to reproduce; the flags do not.
func TestOpenLock_ExistingFileIsOpenedWithoutCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "held.lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	flags := recordOpens(t)

	f, err := OpenLock(path, 0o600)
	if err != nil {
		t.Fatalf("OpenLock: %v", err)
	}
	_ = f.Close()
	if len(*flags) != 1 {
		t.Fatalf("opens = %d, want 1", len(*flags))
	}
	if (*flags)[0]&os.O_CREATE != 0 {
		t.Errorf("an existing lock was opened with O_CREAT (flags %#x)", (*flags)[0])
	}
	if (*flags)[0]&syscall.O_NOFOLLOW == 0 {
		t.Errorf("open dropped O_NOFOLLOW (flags %#x)", (*flags)[0])
	}
}

// An absent lock is created exclusively, so the create can never land on a file another process just made.
func TestOpenLock_AbsentFileIsCreatedExclusively(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.lock")
	flags := recordOpens(t)

	f, err := OpenLock(path, 0o640)
	if err != nil {
		t.Fatalf("OpenLock: %v", err)
	}
	_ = f.Close()
	if len(*flags) != 2 {
		t.Fatalf("opens = %d, want a plain open then a create", len(*flags))
	}
	const want = os.O_CREATE | os.O_EXCL | syscall.O_NOFOLLOW
	if (*flags)[1]&want != want {
		t.Errorf("create flags %#x lack O_CREAT|O_EXCL|O_NOFOLLOW", (*flags)[1])
	}
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm()&^0o640 != 0 {
		t.Errorf("created lock: %v mode %v, want at most 0640", err, st.Mode().Perm())
	}
}

// A lock the caller may read but not write is still lockable: flock needs no write access.
func TestOpenLock_ReadOnlyLockFallsBackToReadOpen(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the file mode")
	}
	path := filepath.Join(t.TempDir(), "ro.lock")
	if err := os.WriteFile(path, nil, 0o400); err != nil {
		t.Fatal(err)
	}
	f, err := OpenLock(path, 0o600)
	if err != nil {
		t.Fatalf("OpenLock on a read-only lock: %v", err)
	}
	defer func() { _ = f.Close() }()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("flock on the read-only handle: %v", err)
	}
}
