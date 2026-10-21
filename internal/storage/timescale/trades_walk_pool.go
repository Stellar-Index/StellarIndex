package timescale

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// tradeWalkConns sizes the long-range walk pool: enough that a burst of dormant-pair price reads queues for ~40ms warm plans, few
// enough that the cold catalog load stays on a handful of backends.
const tradeWalkConns = 4

// configureTradeWalkPool is [configurePool] for walkDB minus the idle reap, which would hand the walk a cold backend after a quiet spell.
func configureTradeWalkPool(db *sql.DB) {
	db.SetMaxOpenConns(tradeWalkConns)
	db.SetMaxIdleConns(tradeWalkConns)
	db.SetConnMaxLifetime(PoolConnMaxLifetime)
}

func (s *Store) tradeWalkDB() *sql.DB {
	if s.walkDB != nil {
		return s.walkDB
	}
	return s.db
}

// WarmTradeWalkPool fills the long-range walk pool and runs today's fallback statement once on each backend, so the first burst of
// dormant-pair price reads after a restart finds warm catalogs; a no-op without that pool.
func (s *Store) WarmTradeWalkPool(ctx context.Context) error {
	if s.walkDB == nil {
		return nil
	}
	since := time.Now().Add(-LatestVWAPWindow).UTC().Truncate(24 * time.Hour)
	warm := canonical.Pair{Base: canonical.NativeAsset(), Quote: canonical.NativeAsset()}
	conns := make([]*sql.Conn, 0, tradeWalkConns)
	defer func() {
		for _, c := range conns {
			_ = c.Close()
		}
	}()
	for range tradeWalkConns {
		c, err := s.walkDB.Conn(ctx)
		if err != nil {
			return fmt.Errorf("timescale: warm trade walk pool: %w", err)
		}
		conns = append(conns, c)
	}
	var g errgroup.Group
	for _, c := range conns {
		g.Go(func() error {
			_, err := s.latestTradesForPair(ctx, c, warm, 1, since)
			return err
		})
	}
	return g.Wait()
}

// tradeQuerier is a pool or one pinned connection.
type tradeQuerier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}
