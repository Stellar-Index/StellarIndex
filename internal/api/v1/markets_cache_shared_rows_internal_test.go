// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// Findings F014 / K038, cache half: CachedMarketsReader owns the
// immutability of what it stores. Each of fetchPairs' / fetchPools' four
// serving branches — (C) cold leader, (A) fresh hit, (A') stale-while-
// revalidate, (B) cold waiter — must hand the caller rows it can scribble
// on without the next caller seeing the scribble. The handler-level tests
// (markets_cache_shared_rows_test.go) only reach (C) and (A); (A') and (B)
// need the cache's internals.

const ownedRowsPristine = "41.32"

// ownedRowsUpstream returns a fresh slice per call. When `gate` is set,
// every call blocks until it is closed, signalling `entered` first.
type ownedRowsUpstream struct {
	calls   atomic.Int64
	gate    chan struct{}
	entered chan struct{}
}

func (u *ownedRowsUpstream) wait(ctx context.Context) error {
	u.calls.Add(1)
	if u.gate == nil {
		return nil
	}
	select {
	case u.entered <- struct{}{}:
	default:
	}
	select {
	case <-u.gate:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (u *ownedRowsUpstream) pairs(ctx context.Context) ([]Market, string, error) {
	if err := u.wait(ctx); err != nil {
		return nil, "", err
	}
	p := ownedRowsPristine
	return []Market{{Base: "native", Quote: "fiat:USD", LastPrice: &p}}, "", nil
}

func (u *ownedRowsUpstream) DistinctPairsExt(ctx context.Context, _ string, _ int, _ timescale.MarketsOrder) ([]Market, string, error) {
	return u.pairs(ctx)
}

func (u *ownedRowsUpstream) SourceMarkets(ctx context.Context, _, _ string, _ int, _ timescale.MarketsOrder) ([]Market, string, error) {
	return u.pairs(ctx)
}

func (u *ownedRowsUpstream) AssetMarkets(ctx context.Context, _, _ string, _ int, _ timescale.MarketsOrder) ([]Market, string, error) {
	return u.pairs(ctx)
}

func (u *ownedRowsUpstream) AllPools(ctx context.Context, _ timescale.PoolsFilter, _ string, _ int, _ timescale.MarketsOrder) ([]Pool, string, error) {
	if err := u.wait(ctx); err != nil {
		return nil, "", err
	}
	p := ownedRowsPristine
	return []Pool{{Source: "aquarius", Base: "native", Quote: "fiat:USD", LastPrice: &p}}, "", nil
}

func (u *ownedRowsUpstream) PairMarket(context.Context, canonical.Asset, canonical.Asset) (Market, bool, error) {
	return Market{}, false, nil
}

func (u *ownedRowsUpstream) GetPairsVolumeHistory24hBatch(context.Context, [][2]string) (map[string][]timescale.PairVolumePoint, error) {
	return map[string][]timescale.PairVolumePoint{}, nil
}

func (u *ownedRowsUpstream) FirstTradeBatch(context.Context, [][2]string) (map[string]time.Time, error) {
	return map[string]time.Time{}, nil
}

// ownedRowsRead is one cached list method reduced to what the ownership
// contract cares about: read the page, report its last_price, and then
// deface the returned rows exactly the way the handlers write them
// (element-field assignment). `addr` identifies the backing array. It
// reports failure as an error, not t.Fatal, because the waiter test calls
// it off the test goroutine.
type ownedRowsRead func(c *CachedMarketsReader) (price string, addr any, err error)

func ownedRowsPairsRead(call func(*CachedMarketsReader) ([]Market, error)) ownedRowsRead {
	return func(c *CachedMarketsReader) (string, any, error) {
		rows, err := call(c)
		if err != nil {
			return "", nil, err
		}
		if len(rows) != 1 || rows[0].LastPrice == nil {
			return "", nil, fmt.Errorf("unexpected rows: %+v", rows)
		}
		got := *rows[0].LastPrice
		defaced := "DEFACED"
		rows[0].LastPrice = &defaced
		rows[0].VolumeHistory24h = []MarketVolumeBucket{{VolumeUSD: defaced}}
		return got, &rows[0], nil
	}
}

func ownedRowsReads() map[string]ownedRowsRead {
	ctx := context.Background()
	order := timescale.MarketsOrderVolume24hDesc
	return map[string]ownedRowsRead{
		"DistinctPairsExt": ownedRowsPairsRead(func(c *CachedMarketsReader) ([]Market, error) {
			rows, _, err := c.DistinctPairsExt(ctx, "", 25, order)
			return rows, err
		}),
		"SourceMarkets": ownedRowsPairsRead(func(c *CachedMarketsReader) ([]Market, error) {
			rows, _, err := c.SourceMarkets(ctx, "aquarius", "", 25, order)
			return rows, err
		}),
		"AssetMarkets": ownedRowsPairsRead(func(c *CachedMarketsReader) ([]Market, error) {
			rows, _, err := c.AssetMarkets(ctx, "native", "", 25, order)
			return rows, err
		}),
		"AllPools": func(c *CachedMarketsReader) (string, any, error) {
			rows, _, err := c.AllPools(ctx, timescale.PoolsFilter{}, "", 25, order)
			if err != nil {
				return "", nil, err
			}
			if len(rows) != 1 || rows[0].LastPrice == nil {
				return "", nil, fmt.Errorf("unexpected rows: %+v", rows)
			}
			got := *rows[0].LastPrice
			defaced := "DEFACED"
			rows[0].LastPrice = &defaced
			return got, &rows[0], nil
		},
	}
}

// TestCachedMarketsReader_CallerOwnsReturnedRows_LeaderHitStale covers
// (C) → (A) → (A') → (A' with the refresh already in flight).
func TestCachedMarketsReader_CallerOwnsReturnedRows_LeaderHitStale(t *testing.T) {
	for name, read := range ownedRowsReads() {
		t.Run(name, func(t *testing.T) {
			up := &ownedRowsUpstream{}
			c := NewCachedMarketsReader(up, time.Hour)

			seen := map[any]string{}
			check := func(branch string) {
				t.Helper()
				price, addr, err := read(c)
				if err != nil {
					t.Fatalf("%s: %v", branch, err)
				}
				if price != ownedRowsPristine {
					t.Errorf("%s: last_price = %q, want %q — an earlier caller's write reached the cache", branch, price, ownedRowsPristine)
				}
				if prev, dup := seen[addr]; dup {
					t.Errorf("%s: shares a backing array with %s", branch, prev)
				}
				seen[addr] = branch
			}

			check("(C) cold leader")
			check("(A) fresh hit #1")
			check("(A) fresh hit #2")
			if got := up.calls.Load(); got != 1 {
				t.Fatalf("upstream calls = %d, want 1 (hits must come from the cache)", got)
			}

			// Hold the SWR refresh open so both stale reads are served
			// from the ORIGINAL entry, not from a refreshed one.
			up.gate, up.entered = make(chan struct{}), make(chan struct{}, 1)
			expireMarketsEntries(c)
			check("(A') stale, kicks refresh")
			<-up.entered
			check("(A') stale, refresh in flight")
			close(up.gate)
			waitMarketsFlightsDone(t, c)
		})
	}
}

// TestCachedMarketsReader_CallerOwnsReturnedRows_ColdWaiters covers (B):
// callers that join a cold fetch must not share rows with the leader, with
// each other, or with the entry the next hit is served from.
func TestCachedMarketsReader_CallerOwnsReturnedRows_ColdWaiters(t *testing.T) {
	for name, read := range ownedRowsReads() {
		t.Run(name, func(t *testing.T) {
			up := &ownedRowsUpstream{gate: make(chan struct{}), entered: make(chan struct{}, 1)}
			c := NewCachedMarketsReader(up, time.Hour)

			const callers = 6
			var (
				wg     sync.WaitGroup
				mu     sync.Mutex
				prices []string
				errs   []error
				addrs  = map[any]int{}
			)
			for i := 0; i < callers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					price, addr, err := read(c)
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						errs = append(errs, err)
						return
					}
					prices = append(prices, price)
					addrs[addr]++
				}()
			}
			<-up.entered
			// Give the other callers time to park on the leader's
			// flight. One that arrives late takes the fresh-hit branch
			// instead, which the assertions below hold to the same bar.
			time.Sleep(50 * time.Millisecond)
			close(up.gate)
			wg.Wait()
			if len(errs) > 0 {
				t.Fatalf("%d concurrent reads failed; first: %v", len(errs), errs[0])
			}

			price, addr, err := read(c)
			if err != nil {
				t.Fatal(err)
			}
			prices = append(prices, price)
			addrs[addr]++

			for i, p := range prices {
				if p != ownedRowsPristine {
					t.Errorf("caller %d: last_price = %q, want %q", i, p, ownedRowsPristine)
				}
			}
			if len(addrs) != callers+1 {
				t.Errorf("distinct backing arrays = %d, want %d (every caller owns its rows)", len(addrs), callers+1)
			}
			if got := up.calls.Load(); got != 1 {
				t.Fatalf("upstream calls = %d, want 1 (single-flight)", got)
			}
		})
	}
}

// waitMarketsFlightsDone blocks until no entry has a refresh in flight, so
// a test never returns with a detached refresh goroutine still running.
func waitMarketsFlightsDone(t *testing.T, c *CachedMarketsReader) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		busy := false
		for _, e := range c.entries {
			busy = busy || e.flight != nil
		}
		c.mu.Unlock()
		if !busy {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("SWR refresh never finished")
}
