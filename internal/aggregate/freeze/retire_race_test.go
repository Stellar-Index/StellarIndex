package freeze_test

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
)

// clearAfterReadCache lands an operator's freeze-unfreeze DEL between the
// retire's marker read and its write back.
type clearAfterReadCache struct {
	freeze.RedisCache
}

func (c clearAfterReadCache) Get(ctx context.Context, key string) *redis.StringCmd {
	cmd := c.RedisCache.Get(ctx, key)
	c.RedisCache.Del(ctx, key)
	return cmd
}

// TestRetireWindowLadder_DoesNotResurrectAClearedMarker: a marker cleared
// between the retire's read and its write must stay cleared. SET KEEPTTL
// on a key that no longer exists creates it with NO expiry, so the write
// back would bring the freeze back permanently, past the operator's clear.
func TestRetireWindowLadder_DoesNotResurrectAClearedMarker(t *testing.T) {
	mr, rdb := newRedis(t)
	w, err := freeze.NewWriter(rdb, time.Minute)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	asset, quote := nativeUSD(t)
	ctx := context.Background()
	fired := time.Now().UTC()
	for _, window := range []time.Duration{shortWindow, longWindow} {
		if err := w.MarkHoldForWindow(ctx, asset, quote, window, "0.124200000000",
			ladderDecision(), ladderState(fired, 0), time.Hour); err != nil {
			t.Fatalf("MarkHoldForWindow(%v): %v", window, err)
		}
	}

	racing, err := freeze.NewWriter(clearAfterReadCache{RedisCache: rdb}, time.Minute)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := racing.RetireWindowLadder(ctx, asset, quote, shortWindow); err != nil {
		t.Fatalf("RetireWindowLadder after a concurrent clear = %v, want nil (already cleared)", err)
	}
	key := cachekeys.Freeze(asset, quote).String()
	if mr.Exists(key) {
		t.Fatalf("the cleared marker was written back (TTL %v) — the operator's "+
			"unfreeze was undone", mr.TTL(key))
	}
}
