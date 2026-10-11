package v1

import (
	"context"
	"sync"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// BackfillCoverageReader is the seam the per-source coverage cache
// reads through. That query takes 2–3s on a populated trades hypertable, so
// callers go through CoverageCache rather than the reader directly.
type BackfillCoverageReader interface {
	BackfillCoverageStats(ctx context.Context) ([]timescale.BackfillCoverage, error)
}

// CoverageCache wraps a [BackfillCoverageReader] with a read-mostly
// snapshot. A background refresher calls Refresh every CoverageRefreshInterval;
// handlers read Snapshot, O(1) under an RLock.
//
// Before the first refresh Snapshot returns nil and a zero time, which the
// handler renders as "coverage data not yet computed".
type CoverageCache struct {
	mu        sync.RWMutex
	snapshot  []timescale.BackfillCoverage
	fetchedAt time.Time
	reader    BackfillCoverageReader
	logger    Logger
}

// CoverageRefreshInterval is the background Refresh cadence. 5 minutes
// amortises the 2-3s SQL across many status-page polls while a stalled backfill
// still shows up within one window.
const CoverageRefreshInterval = 5 * time.Minute

// Logger is the minimal logging surface CoverageCache needs.
// Decoupled from slog so the cache stays import-free of the
// rest of the v1 package.
type Logger interface {
	Warn(msg string, args ...any)
}

// NewCoverageCache constructs an empty cache. Call Refresh once at
// startup to populate it before serving requests; subsequent
// refreshes happen on the goroutine schedule.
func NewCoverageCache(reader BackfillCoverageReader, logger Logger) *CoverageCache {
	return &CoverageCache{reader: reader, logger: logger}
}

// Snapshot returns the most recent successful fetch + its
// timestamp. Both fields are zero values if Refresh has never
// completed.
func (c *CoverageCache) Snapshot() ([]timescale.BackfillCoverage, time.Time) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.snapshot, c.fetchedAt
}

// Refresh runs the underlying query and atomically swaps the
// cached snapshot on success. On error the previous snapshot is
// kept (a transient DB hiccup shouldn't blank the diagnostics
// surface). Errors are warn-logged for ops visibility.
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
