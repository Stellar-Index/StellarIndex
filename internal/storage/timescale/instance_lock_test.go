package timescale

import (
	"context"
	"database/sql/driver"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func instanceLockResult(v bool) scriptedResult {
	return scriptedResult{cols: []string{"pg_try_advisory_lock"}, rows: [][]driver.Value{{v}}}
}

var (
	instancePingOK   = scriptedResult{cols: []string{"?column?"}, rows: [][]driver.Value{{int64(1)}}}
	instancePingDead = scriptedResult{err: errors.New("conn closed: unexpected EOF")}
)

// TestHoldInstanceLock_RefusesWhenHeld pins the spelling (non-blocking
// session lock on hashtext of the name) and that a held lock is the named
// error after exactly one statement — never a wait, never a retry.
func TestHoldInstanceLock_RefusesWhenHeld(t *testing.T) {
	t.Parallel()
	store, conn := newScriptedStore(t, instanceLockResult(false))
	l, err := store.HoldInstanceLock(context.Background(), IndexerInstanceLockName, func() {}, slog.New(slog.DiscardHandler))
	if !errors.Is(err, ErrInstanceLockHeld) || l != nil {
		t.Fatalf("held: lock=%v err=%v, want ErrInstanceLockHeld", l, err)
	}
	if n := len(conn.stmts); n != 1 {
		t.Fatalf("a refused lock issued %d statements, want 1:\n%s", n, strings.Join(conn.statements(), "\n"))
	}
	got := conn.stmts[0]
	if !strings.Contains(got.sql, "pg_try_advisory_lock(hashtext(") {
		t.Errorf("lock statement = %q, want the NON-blocking pg_try_advisory_lock on hashtext", got.sql)
	}
	if got.arg(t, 1) != "instance:stellarindex-indexer" {
		t.Errorf("keyed on %v, want instance:stellarindex-indexer", got.arg(t, 1))
	}
}

func TestHoldInstanceLock_ReleaseUnlocks(t *testing.T) {
	t.Parallel()
	store, conn := newScriptedStore(t, instanceLockResult(true), instanceLockResult(true))
	l, err := store.HoldInstanceLock(context.Background(), AggregatorInstanceLockName, func() {}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Release(context.Background()); err != nil {
		t.Fatalf("release: %v", err)
	}
	if len(conn.stmts) != 2 || !strings.Contains(conn.stmts[1].sql, "pg_advisory_unlock(hashtext(") {
		t.Fatalf("statements = %q, want try lock then unlock", conn.statements())
	}
	if conn.stmts[1].arg(t, 1) != "instance:stellarindex-aggregator" {
		t.Errorf("unlock keyed on %v", conn.stmts[1].arg(t, 1))
	}
	if l.Lost() != nil {
		t.Errorf("Lost() = %v after a clean release", l.Lost())
	}
}

// TestInstanceLockVerify_RetakesADeadSession: a dead holding session is
// not "another instance" — with the lock free, it is retaken.
func TestInstanceLockVerify_RetakesADeadSession(t *testing.T) {
	t.Parallel()
	store, conn := newScriptedStore(t, instanceLockResult(true), instancePingOK, instancePingDead, instanceLockResult(true))
	l := &InstanceLock{db: store.db, name: IndexerInstanceLockName, onLost: func() { t.Error("onLost called") }, logger: slog.New(slog.DiscardHandler)}
	c, err := l.take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	l.conn = c
	for i := range 2 {
		if err := l.verify(context.Background()); err != nil {
			t.Fatalf("verify %d: %v", i, err)
		}
	}
	if l.conn == nil {
		t.Fatal("verify left no holding session")
	}
	want := []string{"pg_try_advisory_lock", "SELECT 1", "SELECT 1", "pg_try_advisory_lock"}
	for i, s := range conn.statements() {
		if i >= len(want) || !strings.Contains(s, want[i]) {
			t.Fatalf("statements = %q, want %q", conn.statements(), want)
		}
	}
}

// TestInstanceLockWatch_LostToAnotherSession: when the session dies and
// another process takes the lock, the holder must shut itself down and
// say why — not carry on as a second writer.
func TestInstanceLockWatch_LostToAnotherSession(t *testing.T) {
	t.Parallel()
	store, _ := newScriptedStore(t, instanceLockResult(true), instancePingDead, instanceLockResult(false))
	lost := make(chan struct{})
	l := &InstanceLock{db: store.db, name: IndexerInstanceLockName, onLost: func() { close(lost) }, logger: slog.New(slog.DiscardHandler)}
	c, err := l.take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	l.conn = c
	l.startWatch(context.Background(), time.Millisecond)
	select {
	case <-lost:
	case <-time.After(5 * time.Second):
		t.Fatal("onLost never called after another session took the lock")
	}
	if !errors.Is(l.Lost(), ErrInstanceLockHeld) {
		t.Fatalf("Lost() = %v, want ErrInstanceLockHeld", l.Lost())
	}
	if err := l.Release(context.Background()); err != nil {
		t.Fatalf("release after loss: %v (nothing is held; must not unlock)", err)
	}
}

func TestInstanceLock_NilSafe(t *testing.T) {
	t.Parallel()
	var l *InstanceLock
	if l.Lost() != nil || l.Release(context.Background()) != nil {
		t.Fatal("nil lock (dry-run) must be a no-op")
	}
}
