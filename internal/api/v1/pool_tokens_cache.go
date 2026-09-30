package v1

import (
	"context"
	"errors"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

var errPoolTokensFillPanicked = errors.New("pool tokens fill panicked")

// poolTokensTTL is how long a source's pool→tokens map is served before
// the next read refills it: a new pool, or a reserve added to an existing
// one (blend set_reserve), goes unlabelled for up to this long.
const poolTokensTTL = 10 * time.Minute

// poolTokensFillTimeout bounds one detached fill: the full-table DISTINCT
// scans behind it must not pin the single-flight if the served tier wedges.
const poolTokensFillTimeout = 30 * time.Second

type poolTokensEntry struct {
	tokens map[string][]string
	at     time.Time
}

// poolTokensCache puts a per-source TTL cache in front of PoolTokens.
// /v1/accounts/{g}/positions is keyed by address, so without it every
// address miss re-ran up to three whole-table DISTINCT scans. The
// returned map is shared across callers and must be treated as read-only.
type poolTokensCache struct {
	upstream ProtocolPoolTokensReader
	now      func() time.Time

	mu      sync.Mutex
	entries map[string]poolTokensEntry
	flight  singleflight.Group
}

func newPoolTokensCache(upstream ProtocolPoolTokensReader) *poolTokensCache {
	return &poolTokensCache{
		upstream: upstream,
		now:      time.Now,
		entries:  map[string]poolTokensEntry{},
	}
}

// PoolTokens serves a fresh entry from memory, otherwise joins one
// detached fill per source. A failed fill falls back to the last-good map,
// which can only lack pools and reserves first observed since it was built.
func (c *poolTokensCache) PoolTokens(ctx context.Context, source string) (map[string][]string, error) {
	c.mu.Lock()
	prev, have := c.entries[source]
	c.mu.Unlock()
	if have && c.now().Sub(prev.at) < poolTokensTTL {
		return prev.tokens, nil
	}

	//nolint:contextcheck // the fill is shared by every waiter, so no single caller's cancellation may abort it
	ch := c.flight.DoChan(source, func() (any, error) { return c.fill(source) })

	select {
	case res := <-ch:
		if res.Err != nil {
			if have {
				return prev.tokens, nil
			}
			return nil, res.Err
		}
		m, _ := res.Val.(map[string][]string)
		return m, nil
	case <-ctx.Done():
		if have {
			return prev.tokens, nil
		}
		return nil, ctx.Err()
	}
}

// fill runs one detached upstream read for source. It re-checks the entry
// first: a caller that missed just before another flight stored it would
// otherwise start a second flight once that one has left the group.
func (c *poolTokensCache) fill(source string) (val any, err error) {
	c.mu.Lock()
	cur, have := c.entries[source]
	c.mu.Unlock()
	if have && c.now().Sub(cur.at) < poolTokensTTL {
		return cur.tokens, nil
	}
	// singleflight re-raises a panic on a fresh goroutine nothing can
	// recover, so it must be turned into an error here.
	defer func() {
		if rec := recover(); rec != nil {
			worker.Report(nil, "api-pool-tokens-fill", rec)
			val, err = nil, errPoolTokensFillPanicked
		}
	}()
	fillCtx, cancel := context.WithTimeout(context.Background(), poolTokensFillTimeout)
	defer cancel()
	m, err := c.upstream.PoolTokens(fillCtx, source)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.entries[source] = poolTokensEntry{tokens: m, at: c.now()}
	c.mu.Unlock()
	return m, nil
}
