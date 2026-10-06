package wiring

import (
	"context"
	"database/sql"
	"errors"

	"github.com/redis/go-redis/v9"

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
