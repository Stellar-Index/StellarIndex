package timescale

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/lib/pq"
)

// Store is the handle on our TimescaleDB connection pool.
// Safe for concurrent use.
type Store struct {
	db *sql.DB
}

// Open initialises a connection pool. Ping'd before returning so a
// bad DSN fails fast.
//
// Configuration:
//   - max open conns: 25 (conservative; tune per deployment).
//   - max idle conns: 5.
//   - conn max lifetime: 30 min (re-dial periodically to survive
//     rolling Patroni restarts).
func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("timescale: sql.Open: %w", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(30 * time.Minute)

	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("timescale: ping: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the connection pool. Safe to call more than once.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DB returns the underlying *sql.DB for packages that need raw
// access (e.g. the migrate binary). Prefer the typed methods.
func (s *Store) DB() *sql.DB { return s.db }

// ─── Error helpers ─────────────────────────────────────────────────

// ErrNotFound indicates a row we expected to exist did not.
var ErrNotFound = errors.New("timescale: not found")

// ErrAlreadyExists wraps a Postgres unique-violation. Callers
// typically treat this as idempotent-success for insert paths.
var ErrAlreadyExists = errors.New("timescale: already exists")

// isUniqueViolation returns true for Postgres SQLSTATE 23505.
// lib/pq exposes this via *pq.Error; avoiding the type assertion
// makes this robust to driver changes.
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	// Both lib/pq and pgx expose SQLSTATE; sniff via string.
	// "pq: duplicate key value violates unique constraint" is the
	// standard English; SQLSTATE 23505 is the robust marker.
	return containsSQLState(err.Error(), "23505") ||
		containsSubstring(err.Error(), "duplicate key value violates unique constraint")
}

func containsSQLState(msg, code string) bool {
	// Naive but adequate: both drivers embed the SQLSTATE as
	// "SQLSTATE <code>" or "23505" on a line.
	return len(msg) > 0 && (stringsContains(msg, "SQLSTATE "+code) || stringsContains(msg, code))
}

func containsSubstring(haystack, needle string) bool {
	return stringsContains(haystack, needle)
}

// Tiny strings.Contains inlined to keep this file import-light.
func stringsContains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
