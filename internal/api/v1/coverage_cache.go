package v1

import (
	"context"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// BackfillCoverageReader is the seam the coverage cache reads through; the query takes 2-3s, so go through CoverageCache.
type BackfillCoverageReader interface {
	BackfillCoverageStats(ctx context.Context) ([]timescale.BackfillCoverage, error)
}

// CoverageCache is a read-mostly snapshot of a [BackfillCoverageReader], refreshed in the background; Snapshot is O(1) and nil before the first refresh.
type CoverageCache struct {
	mu        sync.RWMutex
	snapshot  []timescale.BackfillCoverage
	fetchedAt time.Time
	reader    BackfillCoverageReader
	logger    Logger
}

// CoverageRefreshInterval is the background Refresh cadence, amortising the 2-3s SQL across status-page polls.
const CoverageRefreshInterval = 5 * time.Minute

// Logger is the minimal logging surface CoverageCache needs, decoupled from slog.
type Logger interface {
	Warn(msg string, args ...any)
}

// NewCoverageCache constructs an empty cache; call Refresh once at startup to populate it.
func NewCoverageCache(reader BackfillCoverageReader, logger Logger) *CoverageCache {
	return &CoverageCache{reader: reader, logger: logger}
}

// Snapshot returns the most recent successful fetch and its timestamp, both zero if Refresh never completed.
func (c *CoverageCache) Snapshot() ([]timescale.BackfillCoverage, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshot, c.fetchedAt
}

// Refresh runs the query and atomically swaps the snapshot; on error the previous snapshot is kept and the error warn-logged.
func (c *CoverageCache) Refresh(ctx context.Context) error {
	rows, err := c.reader.BackfillCoverageStats(ctx)
	if err != nil {
		if c.logger != nil {
			c.logger.Warn("coverage cache refresh", "err", err)
		}
		return err
	}
	c.mu.Lock()
	c.snapshot = rows
	c.fetchedAt = time.Now().UTC()
	c.mu.Unlock()
	return nil
}
