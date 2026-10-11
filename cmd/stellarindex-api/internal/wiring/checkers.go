package wiring

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// StoreChecker adapts *timescale.Store to the v1.ReadyChecker
// interface so /readyz can include it in the dependency poll.
//
// Postgres is critical: every trade, aggregate and supply read hits Timescale with no fallback, so
// Critical()==true and /readyz returns 503 when it is unreachable.
type StoreChecker struct{ S *timescale.Store }

func (c StoreChecker) Name() string   { return "postgres" }
func (c StoreChecker) Critical() bool { return true }
func (c StoreChecker) Ping(ctx context.Context) error {
	return c.S.DB().PingContext(ctx)
}

// SchemaChecker adapts the golang-migrate schema_migrations row to v1.SchemaVersionReader for the
// head assertion. It reads over the store's *sql.DB (stellarindex-migrate owns writes) and keeps
// the raw SQL in this binary layer, like StoreChecker.
type SchemaChecker struct{ DB *sql.DB }

func (c SchemaChecker) SchemaMigrationVersion(ctx context.Context) (uint, bool, error) {
	var version uint
	var dirty bool
	// schema_migrations holds a single row (golang-migrate). No row =
	// no migrations applied = version 0.
	err := c.DB.QueryRowContext(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&version, &dirty)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return version, dirty, nil
}

// RedisChecker adapts redis.UniversalClient to v1.ReadyChecker. Redis is non-critical: cache misses
// fall back to Timescale per ADR-0007, so an outage costs latency, not correctness.
// UniversalClient lets one adapter serve both dev single-node and the Sentinel FailoverClient.
//
// Critical()==false makes an outage a 200 with status="degraded" from /v1/readyz instead of a 503,
// so HAProxy keeps the backend in service while operators see the degradation in the body.
type RedisChecker struct{ RDB redis.UniversalClient }

func (c RedisChecker) Name() string   { return "redis" }
func (c RedisChecker) Critical() bool { return false }
func (c RedisChecker) Ping(ctx context.Context) error {
	return c.RDB.Ping(ctx).Err()
}

// NonstandardDecimalsRefreshTimeout bounds each nonstandard-decimals cache
// load, the blocking startup one included.
const NonstandardDecimalsRefreshTimeout = 30 * time.Second

// PrimeNonstandardDecimalsCache runs the cache's first load synchronously and
// returns the readiness check that gates serving on it having succeeded.
func PrimeNonstandardDecimalsCache(ctx context.Context, c *v1.NonstandardDecimalsCache, logger *slog.Logger) v1.ReadyChecker {
	initCtx, cancel := context.WithTimeout(ctx, NonstandardDecimalsRefreshTimeout)
	defer cancel()
	if err := c.Refresh(initCtx); err != nil {
		logger.Warn("nonstandard-decimals cache initial refresh failed; not ready until a periodic refresh succeeds", "err", err)
	}
	return nonstandardDecimalsChecker{c: c}
}

// nonstandardDecimalsChecker reports not-ready until the nonstandard-decimals
// cache has loaded once. Critical: before that load every confirmed
// non-7-decimal asset resolves to 7dp, so its prices serve off by a power of
// ten. A later refresh failure keeps the last-good snapshot and stays ready.
type nonstandardDecimalsChecker struct{ c *v1.NonstandardDecimalsCache }

func (nonstandardDecimalsChecker) Name() string   { return "nonstandard_decimals" }
func (nonstandardDecimalsChecker) Critical() bool { return true }
func (k nonstandardDecimalsChecker) Ping(context.Context) error {
	if _, fetchedAt := k.c.Snapshot(); fetchedAt.IsZero() {
		return errors.New("nonstandard-decimals cache has not loaded yet")
	}
	return nil
}

// ClickhouseChecker adapts *clickhouse.ExplorerReader to v1.ReadyChecker. It is non-critical: the
// explorer/supply readers 503 on their own endpoints (/v1/ledgers, /v1/tx, /v1/assets/{id}/supply,
// ...) when ClickHouse is down while Postgres-backed reads keep serving, so an outage should read
// status="degraded", not drop the backend from load-balancer rotation. LakeTipLedger is the Ping
// probe: a cheap query against the small `stellar.ledgers` table.
//
// `r` is nil when every boot dial in the retry window failed; the checker then still reports down
// (see ClickhouseReadyChecks).
type ClickhouseChecker struct {
	R          *clickhouse.ExplorerReader
	DialErr    error
	DialBudget time.Duration
}

func (c ClickhouseChecker) Name() string   { return "clickhouse" }
func (c ClickhouseChecker) Critical() bool { return false }
func (c ClickhouseChecker) Ping(ctx context.Context) error {
	if c.R == nil {
		return fmt.Errorf("clickhouse stayed unreachable for this process's whole boot retry window (%s) and is not re-dialled after it; every lake-backed endpoint is 503ing and a restart is required to re-wire them: %w", c.DialBudget, c.DialErr)
	}
	_, err := c.R.LakeTipLedger(ctx)
	return err
}

// ClickhouseReadyChecks returns the readiness checkers for ClickHouse: none when
// no address is configured, exactly one otherwise, wired or not.
//
// A checker registered only on boot-dial success would leave a ClickHouse that was down at startup
// with no stellarindex_dependency_up series, so the alert (== 0, deliberately not absent()) could
// not match while endpoints 503. One still unreachable when dialLakeReadersAtBoot's window ends
// therefore reports down for the process's lifetime: the lake seams are never re-dialled, so a
// Ping that went green would hide endpoints still 503ing. Only a ClickHouse that answers before
// the last attempt (clickhouseBootDialMinAttempt) avoids this.
//
// No address means no lake and no dependency, so nothing is published.
func ClickhouseReadyChecks(addr string, er *clickhouse.ExplorerReader, dialErr error, dialBudget time.Duration) []v1.ReadyChecker {
	if addr == "" {
		return nil
	}
	if dialErr != nil {
		return []v1.ReadyChecker{ClickhouseChecker{DialErr: dialErr, DialBudget: dialBudget}}
	}
	return []v1.ReadyChecker{ClickhouseChecker{R: er, DialBudget: dialBudget}}
}
