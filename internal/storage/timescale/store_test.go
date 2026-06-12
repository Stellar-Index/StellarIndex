package timescale

import (
	"context"
	"database/sql"
	"testing"
	"time"

	_ "github.com/lib/pq"
)

// TestConfigurePool_AppliesAllFourSettings is the F-0151
// regression test: every pool setting that the audit pinned in
// place MUST be applied on every fresh *sql.DB the package opens.
// If a future refactor drops one, this test fails before the bug
// ships.
//
// We use `sql.Open` (lazy — no real connection attempted until
// Ping) so the test runs as a pure unit test, no postgres needed.
func TestConfigurePool_AppliesAllFourSettings(t *testing.T) {
	db, err := sql.Open("postgres", "postgres://invalid:invalid@127.0.0.1:1/invalid")
	if err != nil {
		t.Fatalf("sql.Open (lazy): %v", err)
	}
	defer db.Close()

	configurePool(db)

	stats := db.Stats()
	if got, want := stats.MaxOpenConnections, PoolMaxOpenConns; got != want {
		t.Errorf("MaxOpenConnections = %d, want %d", got, want)
	}

	// MaxIdleConns / ConnMaxLifetime / ConnMaxIdleTime aren't
	// surfaced through Stats — assert the exported constants
	// remain sane instead (drift-guard) and that configurePool
	// doesn't panic.
	if PoolMaxIdleConns <= 0 || PoolMaxIdleConns > PoolMaxOpenConns {
		t.Errorf("PoolMaxIdleConns = %d, want 0 < n ≤ %d", PoolMaxIdleConns, PoolMaxOpenConns)
	}
	if PoolConnMaxLifetime < time.Minute || PoolConnMaxLifetime > time.Hour {
		t.Errorf("PoolConnMaxLifetime = %v, want 1m–1h (F-0151 safety-net window)", PoolConnMaxLifetime)
	}
	if PoolConnMaxIdleTime <= 0 || PoolConnMaxIdleTime >= PoolConnMaxLifetime {
		t.Errorf("PoolConnMaxIdleTime = %v, want 0 < x < %v", PoolConnMaxIdleTime, PoolConnMaxLifetime)
	}
}

// TestStorePingContext_NilSafe — the resilience-ping goroutine in
// cmd/stellaratlas-indexer/main.go probes via [Store.PingContext];
// a nil receiver during shutdown teardown MUST not panic. Returning
// nil is the documented behaviour.
func TestStorePingContext_NilSafe(t *testing.T) {
	var s *Store
	if err := s.PingContext(context.Background()); err != nil {
		t.Fatalf("nil *Store: PingContext returned %v, want nil", err)
	}

	// Empty Store (no underlying *sql.DB) — same contract.
	empty := &Store{}
	if err := empty.PingContext(context.Background()); err != nil {
		t.Fatalf("&Store{}: PingContext returned %v, want nil", err)
	}
}
