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
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// StoreChecker adapts *timescale.Store to v1.ReadyChecker; Postgres is critical, so /readyz returns 503 when it is unreachable.
type StoreChecker struct{ S *timescale.Store }

func (c StoreChecker) Name() string   { return "postgres" }
func (c StoreChecker) Critical() bool { return true }
func (c StoreChecker) Ping(ctx context.Context) error {
	return c.S.DB().PingContext(ctx)
}

// SchemaChecker adapts the golang-migrate schema_migrations row to v1.SchemaVersionReader for the head assertion.
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

// RedisChecker adapts redis.UniversalClient to v1.ReadyChecker and feeds dependency_up{redis}; non-critical, so a Redis failure
// inside api.rate_limit_dwell reads status="degraded" (200). [RedisOutageChecker] owns the 503 once the dwell has passed.
type RedisChecker struct{ RDB redis.UniversalClient }

func (c RedisChecker) Name() string   { return "redis" }
func (c RedisChecker) Critical() bool { return false }
func (c RedisChecker) Ping(ctx context.Context) error {
	return c.RDB.Ping(ctx).Err()
}

// redisOutageDetail is the public readyz text for a Redis outage past the dwell.
const redisOutageDetail = "redis unreachable longer than api.rate_limit_dwell: keyed and rate-limited routes answer 503"

// RedisOutageChecker is critical and fails only while its DwellClock is failing closed, i.e. when the Redis-backed limiters answer
// 503. Its clock is fed by readiness pings alone, so a drained instance, which gets no traffic, still sees Redis recover.
type RedisOutageChecker struct {
	RDB   redis.UniversalClient
	Clock *ratelimit.DwellClock
}

func (RedisOutageChecker) Name() string   { return "redis_outage" }
func (RedisOutageChecker) Critical() bool { return true }
func (c RedisOutageChecker) Ping(ctx context.Context) error {
	if err := c.RDB.Ping(ctx).Err(); err != nil {
		c.Clock.Failure()
		if c.Clock.FailingClosed() {
			return v1.NewReadyzPublicError(redisOutageDetail)
		}
		return nil
	}
	c.Clock.Success()
	return nil
}

// RedisOutageReadyChecks returns the outage checker only when Redis is configured and a Redis-backed limiter exists; without one, a
// Redis loss closes no route, so readiness must not drain on it.
func RedisOutageReadyChecks(rdb redis.UniversalClient, redisLimited bool, dwell time.Duration) []v1.ReadyChecker {
	if rdb == nil || !redisLimited {
		return nil
	}
	return []v1.ReadyChecker{RedisOutageChecker{RDB: rdb, Clock: ratelimit.NewDwellClock(dwell, nil)}}
}

// NonstandardDecimalsRefreshTimeout bounds each nonstandard-decimals cache load, the blocking startup one included.
const NonstandardDecimalsRefreshTimeout = 30 * time.Second

// PrimeNonstandardDecimalsCache runs the cache's first load synchronously and returns the readiness check that gates serving on it having succeeded.
func PrimeNonstandardDecimalsCache(ctx context.Context, c *v1.NonstandardDecimalsCache, logger *slog.Logger) v1.ReadyChecker {
	initCtx, cancel := context.WithTimeout(ctx, NonstandardDecimalsRefreshTimeout)
	defer cancel()
	if err := c.Refresh(initCtx); err != nil {
		logger.Warn("nonstandard-decimals cache initial refresh failed; not ready until a periodic refresh succeeds", "err", err)
	}
	return nonstandardDecimalsChecker{c: c}
}

// nonstandardDecimalsChecker reports not-ready until the nonstandard-decimals cache has loaded once. Critical: before that, every
// confirmed non-7-decimal asset resolves to 7dp and its prices serve off by a power of ten.
// A later refresh failure keeps the last-good snapshot.
type nonstandardDecimalsChecker struct{ c *v1.NonstandardDecimalsCache }

func (nonstandardDecimalsChecker) Name() string   { return "nonstandard_decimals" }
func (nonstandardDecimalsChecker) Critical() bool { return true }
func (k nonstandardDecimalsChecker) Ping(context.Context) error {
	if _, fetchedAt := k.c.Snapshot(); fetchedAt.IsZero() {
		return errors.New("nonstandard-decimals cache has not loaded yet")
	}
	return nil
}

// ClickhouseChecker adapts *clickhouse.ExplorerReader to v1.ReadyChecker; non-critical (outage reads "degraded"), and a nil reader (all boot dials failed) still reports down.
type ClickhouseChecker struct {
	R       *clickhouse.ExplorerReader
	OpenErr error
}

func (c ClickhouseChecker) Name() string   { return "clickhouse" }
func (c ClickhouseChecker) Critical() bool { return false }
func (c ClickhouseChecker) Ping(ctx context.Context) error {
	if c.R == nil {
		return fmt.Errorf("clickhouse lake reader could not be built, so every lake-backed endpoint is 503ing; fix the ClickHouse configuration and restart: %w", c.OpenErr)
	}
	_, err := c.R.LakeTipLedger(ctx)
	return err
}

// ClickhouseReadyChecks returns the readiness checkers for ClickHouse: none when no address is configured, exactly one otherwise, built or not, so a ClickHouse down at startup still reports down.
func ClickhouseReadyChecks(addr string, er *clickhouse.ExplorerReader, openErr error) []v1.ReadyChecker {
	if addr == "" {
		return nil
	}
	if openErr != nil {
		return []v1.ReadyChecker{ClickhouseChecker{OpenErr: openErr}}
	}
	return []v1.ReadyChecker{ClickhouseChecker{R: er}}
}
