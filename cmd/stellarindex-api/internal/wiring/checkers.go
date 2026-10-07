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
// Postgres is critical — every request that returns trade /
// aggregate / supply data reads from Timescale. There's no
// fallback path; a Postgres outage really does mean the API
// can't serve. Critical()==true so /readyz returns 503 when
// Postgres is unreachable.
type StoreChecker struct{ S *timescale.Store }

func (c StoreChecker) Name() string   { return "postgres" }
func (c StoreChecker) Critical() bool { return true }
func (c StoreChecker) Ping(ctx context.Context) error {
	return c.S.DB().PingContext(ctx)
}

// SchemaChecker adapts the golang-migrate schema_migrations
// bookkeeping row to v1.SchemaVersionReader for the head
// assertion. It reads over the store's *sql.DB —
// the stellarindex-migrate binary owns writes; the API only reads —
// keeping the raw SQL in this binary layer, mirroring StoreChecker.
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

// RedisChecker adapts redis.UniversalClient to the v1.ReadyChecker
// interface. Redis is non-critical at API layer — cache misses
// fall back to Timescale per ADR-0007, so a Redis outage degrades
// latency (every read becomes a Timescale query instead of a
// Redis read) but does NOT break correctness. UniversalClient
// (vs typed Client) lets the same adapter work against both the
// dev single-node and production Sentinel-backed FailoverClient.
//
// Critical()==false so a Redis
// outage produces a 200 with status="degraded" from /v1/readyz
// instead of a 503; HAProxy keeps the backend in service while
// operators see the degradation in the response body.
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

// ClickhouseChecker adapts *clickhouse.ExplorerReader to the
// v1.ReadyChecker interface. Non-critical
// for the same reason as redisChecker: the explorer/supply readers
// this wraps degrade to 503 on their own OWNING endpoints
// (/v1/ledgers, /v1/tx, /v1/assets/{id}/supply, …) when ClickHouse
// is unreachable — the rest of the API (Postgres-backed reads) keeps
// serving correctly — so a CH outage should read status="degraded",
// not take the whole backend out of load-balancer rotation. Reuses
// LakeTipLedger (already exported for the protocol-analytics window
// cutoff) as the Ping probe: a cheap query against the small
// `stellar.ledgers` table, no new ClickHouse-side surface needed.
//
// `r` is nil when every boot dial in the retry window failed. The checker
// still exists in that state and still reports down — see
// ClickhouseReadyChecks.
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

// ClickhouseReadyChecks returns the readiness checkers for ClickHouse:
// none when no address is configured, and exactly one when there is —
// wired or not.
//
// The "or not" is the whole point. A checker
// appended inside the success branch of the boot dial means a ClickHouse
// that was already down when the API started published NO
// `stellarindex_dependency_up{dependency="clickhouse"}` series at all.
// The alert over it is `stellarindex_dependency_up == 0`, with an
// in-file rationale deliberately rejecting absent() — so it would have no
// series to match, and the one state the annotation calls "the only
// signal that it is gone" would be the state with no signal. Endpoints
// would 503 and nothing would page.
//
// A ClickHouse still unreachable when dialLakeReadersAtBoot's window ends
// therefore registers a checker that reports down for the process's
// lifetime. That is the truth: none of the lake-backed seams is
// re-dialled after that, so a Ping that re-dialled and went green would
// hide endpoints that are still 503ing until the process restarts. Only a
// ClickHouse that answers before the last attempt (at least
// clickhouseBootDialMinAttempt before the window ends) avoids this state.
//
// No address configured is the one case that publishes nothing, and
// that is correct: a deployment without a lake has no such dependency,
// and a 0 there would page for a component it does not run.
func ClickhouseReadyChecks(addr string, er *clickhouse.ExplorerReader, dialErr error, dialBudget time.Duration) []v1.ReadyChecker {
	if addr == "" {
		return nil
	}
	if dialErr != nil {
		return []v1.ReadyChecker{ClickhouseChecker{DialErr: dialErr, DialBudget: dialBudget}}
	}
	return []v1.ReadyChecker{ClickhouseChecker{R: er, DialBudget: dialBudget}}
}
