//go:build integration

package integration_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestInstanceLock_OneHolderPerDatabase drives the instance lock against
// real Postgres, two stores standing in for two processes: the second is
// refused while the first holds it, a different name is independent,
// and both a release and the holder's session dying (a crash) free it.
func TestInstanceLock_OneHolderPerDatabase(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	dsn := startTimescale(t, ctx)
	open := func() *timescale.Store {
		s, err := timescale.Open(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	first, second := open(), open()
	logger := slog.New(slog.DiscardHandler)
	hold := func(s *timescale.Store, name string) (*timescale.InstanceLock, error) {
		return s.HoldInstanceLock(ctx, name, func() {}, logger)
	}

	held, err := hold(first, timescale.IndexerInstanceLockName)
	if err != nil {
		t.Fatalf("first indexer: %v", err)
	}
	if _, err := hold(second, timescale.IndexerInstanceLockName); !errors.Is(err, timescale.ErrInstanceLockHeld) {
		t.Fatalf("second indexer while the first runs: err = %v, want ErrInstanceLockHeld", err)
	}
	agg, err := hold(second, timescale.AggregatorInstanceLockName)
	if err != nil {
		t.Fatalf("aggregator beside the indexer: %v (names must be independent)", err)
	}
	if err := agg.Release(ctx); err != nil {
		t.Fatalf("aggregator release: %v", err)
	}

	if err := held.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	held, err = hold(second, timescale.IndexerInstanceLockName)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}

	// The holder crashes: its session ends and the lock must go with it.
	var terminated int
	if err := first.DB().QueryRowContext(ctx, `
		SELECT count(pg_terminate_backend(pid)) FROM pg_locks
		WHERE locktype = 'advisory' AND granted AND pid <> pg_backend_pid()
		  AND pid IN (SELECT pid FROM pg_stat_activity WHERE backend_type = 'client backend')`).Scan(&terminated); err != nil || terminated != 1 {
		t.Fatalf("fixture: terminate the holding session: terminated=%d err=%v", terminated, err)
	}
	var took *timescale.InstanceLock
	deadline := time.Now().Add(10 * time.Second)
	for {
		took, err = hold(first, timescale.IndexerInstanceLockName)
		if err == nil || !errors.Is(err, timescale.ErrInstanceLockHeld) || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("after the holder's session died: %v", err)
	}
	if err := took.Release(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	_ = held.Release(ctx) // its session is gone; the unlock error is expected
}
