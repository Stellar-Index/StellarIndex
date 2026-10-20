package v1

import (
	"context"
	"errors"
	"math/big"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/worker"
)

var errTokenSupplyFillPanicked = errors.New("token supply fill panicked")

const (
	// tokenSupplyTTL bounds how long one contract's lake sum is reused. Supply moves only on mint, burn and clawback, and every
	// Soroban asset without a supply snapshot runs this FINAL sum per page view, so a crawler turns it into sustained lake load.
	tokenSupplyTTL = 5 * time.Minute

	// tokenSupplyFillTimeout bounds one detached fill; the largest contracts sum millions of flows.
	tokenSupplyFillTimeout = 30 * time.Second

	// tokenSupplyCacheMax caps cached contracts so a crawl over every token cannot grow the map without bound.
	tokenSupplyCacheMax = 8192
)

type tokenSupplyEntry struct {
	sup clickhouse.TokenSupply
	at  time.Time
}

// TokenSupplyCache is a per-contract TTL cache in front of [clickhouse.SupplyReader.TokenSupply]; the embedded reader still serves
// NativeTotalCoins and the bulk TokenSupplyForContracts the classic lake-supply arm type-asserts for.
type TokenSupplyCache struct {
	*clickhouse.SupplyReader

	upstream func(ctx context.Context, contractID string) (clickhouse.TokenSupply, error)
	now      func() time.Time

	mu      sync.Mutex
	entries map[string]tokenSupplyEntry
	flight  singleflight.Group
}

// NewTokenSupplyCache wraps sr.
func NewTokenSupplyCache(sr *clickhouse.SupplyReader) *TokenSupplyCache {
	return newTokenSupplyCache(sr, sr.TokenSupply)
}

func newTokenSupplyCache(sr *clickhouse.SupplyReader, upstream func(context.Context, string) (clickhouse.TokenSupply, error)) *TokenSupplyCache {
	return &TokenSupplyCache{SupplyReader: sr, upstream: upstream, now: time.Now, entries: map[string]tokenSupplyEntry{}}
}

// TokenSupply serves a fresh entry or joins one detached fill per contract; a failed fill falls back to the last-good reading.
// Every return is a copy, so no caller can mutate another's amounts.
func (c *TokenSupplyCache) TokenSupply(ctx context.Context, contractID string) (clickhouse.TokenSupply, error) {
	c.mu.Lock()
	prev, have := c.entries[contractID]
	c.mu.Unlock()
	if have && c.now().Sub(prev.at) < tokenSupplyTTL {
		return copyTokenSupply(prev.sup), nil
	}

	//nolint:contextcheck // the fill is shared by every waiter, so no single caller's cancellation may abort it
	ch := c.flight.DoChan(contractID, func() (any, error) { return c.fill(contractID) })

	select {
	case res := <-ch:
		if res.Err != nil {
			if have {
				return copyTokenSupply(prev.sup), nil
			}
			return clickhouse.TokenSupply{}, res.Err
		}
		sup, _ := res.Val.(clickhouse.TokenSupply)
		return copyTokenSupply(sup), nil
	case <-ctx.Done():
		if have {
			return copyTokenSupply(prev.sup), nil
		}
		return clickhouse.TokenSupply{}, ctx.Err()
	}
}

// fill runs one detached upstream read; it re-checks the entry first, or a late caller would start a second flight after the first left
// the group.
func (c *TokenSupplyCache) fill(contractID string) (val any, err error) {
	c.mu.Lock()
	cur, have := c.entries[contractID]
	c.mu.Unlock()
	if have && c.now().Sub(cur.at) < tokenSupplyTTL {
		return cur.sup, nil
	}
	// singleflight re-raises a panic on a fresh goroutine nothing can
	// recover, so it must be turned into an error here.
	defer func() {
		if rec := recover(); rec != nil {
			worker.Report(nil, "api-token-supply-fill", rec)
			val, err = nil, errTokenSupplyFillPanicked
		}
	}()
	fillCtx, cancel := context.WithTimeout(context.Background(), tokenSupplyFillTimeout)
	defer cancel()
	sup, err := c.upstream(fillCtx, contractID)
	if err != nil {
		return nil, err
	}
	now := c.now()
	c.mu.Lock()
	if len(c.entries) >= tokenSupplyCacheMax {
		c.evictLocked(now)
	}
	c.entries[contractID] = tokenSupplyEntry{sup: sup, at: now}
	c.mu.Unlock()
	return sup, nil
}

// evictLocked drops expired entries, and everything if none had expired: a full map of fresh entries means a crawl, and a cold
// restart of the cache is cheaper than tracking recency.
func (c *TokenSupplyCache) evictLocked(now time.Time) {
	for k, e := range c.entries {
		if now.Sub(e.at) >= tokenSupplyTTL {
			delete(c.entries, k)
		}
	}
	if len(c.entries) >= tokenSupplyCacheMax {
		c.entries = map[string]tokenSupplyEntry{}
	}
}

func copyTokenSupply(s clickhouse.TokenSupply) clickhouse.TokenSupply {
	s.Total, s.Mint, s.Burn, s.Clawback = copyBig(s.Total), copyBig(s.Mint), copyBig(s.Burn), copyBig(s.Clawback)
	return s
}

func copyBig(v *big.Int) *big.Int {
	if v == nil {
		return nil
	}
	return new(big.Int).Set(v)
}
