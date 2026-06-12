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

	// usdVolumeQuoteSpec, when non-nil, lets [InsertTrade] populate
	// `trades.usd_volume` for on-chain trades whose quote asset is
	// on the operator's USD-pegged list. Set via
	// [SetUSDVolumeQuoteSpec] after [Open] — keeps the no-config
	// path (tests, ops binary) on the existing off-chain-only
	// behaviour.
	usdVolumeQuoteSpec *USDVolumeQuoteSpec

	// usdVolumeFXResolver, when non-nil, is consulted by
	// [InsertTrade] AFTER [usdVolumeQuoteSpec] has rejected the
	// trade — i.e. the on-chain quote isn't on the operator's
	// USD-pegged list, so Phase 1 returns NULL. The resolver returns
	// a USD rate for the quote asset (typically sourced from the
	// aggregator's `<asset>/<USD>` VWAP) which [InsertTrade]
	// multiplies through quote_amount to land a non-NULL
	// `usd_volume` per L2.2 Phase 2.
	//
	// Nil keeps the L2.2 Phase 1 behaviour exactly: only off-chain
	// CEX/FX + operator-allow-listed on-chain DEX trades get a
	// non-NULL `usd_volume`. Set via [SetUSDVolumeFXResolver] after
	// [Open]; safe to leave unset for tests, ops binary, and any
	// deployment that hasn't enabled Phase 2.
	usdVolumeFXResolver USDVolumeFXResolver
}

// SetUSDVolumeQuoteSpec installs the operator-configured quote-asset
// spec used by [InsertTrade] to populate `trades.usd_volume` for
// on-chain trades. Safe to call once at startup; not safe to call
// concurrently with InsertTrade.
//
// nil clears the spec — InsertTrade reverts to off-chain-only
// behaviour (the L2.2 pre-Phase-1 default).
func (s *Store) SetUSDVolumeQuoteSpec(spec *USDVolumeQuoteSpec) {
	s.usdVolumeQuoteSpec = spec
}

// SetUSDVolumeFXResolver installs the FX-resolver path for
// L2.2 Phase 2 on-chain USD-volume coverage. nil clears it.
//
// Safe to call once at startup; not safe to call concurrently with
// InsertTrade. The resolver is consulted only when Phase 1
// (USDVolumeQuoteSpec) declines the trade — see [tradeUSDVolume].
func (s *Store) SetUSDVolumeFXResolver(r USDVolumeFXResolver) {
	s.usdVolumeFXResolver = r
}

// Pool-tuning constants. Exposed so [store_test.go] can assert
// configurePool actually set them, and so operators reading the
// audit register (F-0151) can see the live values without grepping
// the function body.
//
// See [configurePool] for the rationale on each value.
const (
	// PoolMaxOpenConns caps total conns held by one indexer/api/
	// aggregator binary. 25 is conservative; tune per deployment.
	PoolMaxOpenConns = 25
	// PoolMaxIdleConns caps idle conns kept in the pool between
	// uses. Keeping a small idle floor avoids the connect-storm
	// pattern on a cold cache.
	PoolMaxIdleConns = 5
	// PoolConnMaxLifetime is the full re-dial ceiling — every conn
	// is retired this often regardless of liveness. This is the
	// resilience net behind F-0151: the 2026-05-26 cascade left
	// dead conns in the pool for ~14 h after the underlying
	// postgres@15-main crashed and recovered, because nothing
	// forced them to refresh. 30 min beats Patroni's typical
	// rolling-restart interval AND bounds the longest cascade-gap
	// to that interval.
	PoolConnMaxLifetime = 30 * time.Minute
	// PoolConnMaxIdleTime bounds the window where an idle conn the
	// DB-side has already killed (pg_terminate_backend, firewall
	// tcp-timeout, Patroni failover) might still be handed out.
	// Without this, an idle conn can live until ConnMaxLifetime,
	// forcing a retry at serve-time.
	PoolConnMaxIdleTime = 5 * time.Minute
)

// configurePool applies the standard pool tunings to a freshly-
// opened *sql.DB. Extracted so [store_test.go] can verify the
// settings without booting a real postgres.
//
// F-0151 (2026-05-27 audit) drove the explicit constant naming +
// extraction: the previous inline magic-numbers shipped correct
// values but were invisible to anything except a reader of this
// file, so a future refactor could silently drop them and the
// connection-pool resilience would regress unnoticed.
func configurePool(db *sql.DB) {
	db.SetMaxOpenConns(PoolMaxOpenConns)
	db.SetMaxIdleConns(PoolMaxIdleConns)
	db.SetConnMaxLifetime(PoolConnMaxLifetime)
	db.SetConnMaxIdleTime(PoolConnMaxIdleTime)
}

// Open initialises a connection pool. Ping'd before returning so a
// bad DSN fails fast.
//
// Pool tuning is applied via [configurePool] — see those constants
// for the per-setting rationale. Net effect: every conn is retired
// at most every [PoolConnMaxLifetime], which is the resilience
// safety-net behind F-0151 (the 2026-05-26 cascade left dead
// conns in the pool for ~14 h after postgres@15-main recovered).
func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("timescale: sql.Open: %w", err)
	}
	configurePool(db)

	pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("timescale: ping: %w", err)
	}
	return &Store{db: db}, nil
}

// PingContext exercises the underlying *sql.DB pool. Used by the
// indexer's periodic resilience probe (see watchPostgresPing in
// cmd/stellaratlas-indexer/main.go) to surface dead-pool conditions
// as a metric / alert signal. The actual reconnect path is handled
// automatically by database/sql + ConnMaxLifetime — this method is
// the OBSERVABILITY hook, not the reconnect mechanism.
//
// Returns nil on a nil receiver so callers can poll a Store that
// hasn't been wired yet during shutdown / test teardown without
// special-casing.
func (s *Store) PingContext(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.PingContext(ctx)
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
