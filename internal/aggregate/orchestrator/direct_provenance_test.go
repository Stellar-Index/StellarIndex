package orchestrator

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// orderedCache records the order of writes to the keys it watches and can
// fail the Del of one of them.
type orderedCache struct {
	*redis.Client
	watch   map[string]string // key → label
	ops     []string
	failDel string
}

func (c *orderedCache) Set(ctx context.Context, key string, value any, exp time.Duration) *redis.StatusCmd {
	if l, ok := c.watch[key]; ok {
		c.ops = append(c.ops, "set "+l)
	}
	return c.Client.Set(ctx, key, value, exp)
}

func (c *orderedCache) Del(ctx context.Context, keys ...string) *redis.IntCmd {
	for _, k := range keys {
		if l, ok := c.watch[k]; ok {
			c.ops = append(c.ops, "del "+l)
		}
		if k == c.failDel {
			cmd := redis.NewIntCmd(ctx)
			cmd.SetErr(errors.New("injected del failure"))
			return cmd
		}
	}
	return c.Client.Del(ctx, keys...)
}

// The direct refresh must clear a prior composite's "triangulated" marker
// BEFORE writing its value, and must not publish when the clear fails:
// the API serves the Redis fallback only under that marker, so a direct
// value landing beneath a stale one is served as a composite.
func TestPublishDirect_ClearsProvenanceBeforeValueAndRefusesOnFailure(t *testing.T) {
	pair := xlmUSDPair(t)
	window := 5 * time.Minute
	valueKey := cachekeys.VWAP(pair.Base, pair.Quote, window).String()
	provKey := cachekeys.VWAPProvenance(pair.Base, pair.Quote, window).String()
	now := time.Date(2026, 7, 25, 12, 0, 30, 0, time.UTC)
	trades := []canonical.Trade{
		makeXLMUSDTrade(t, "soroswap", lkgBaseAmount, lkgQuoteAmount, now.Add(-2*time.Minute)),
	}

	for _, failDel := range []bool{false, true} {
		rdb, mr := newTestRedis(t)
		if err := mr.Set(valueKey, "0.100000000000"); err != nil {
			t.Fatal(err)
		}
		if err := mr.Set(provKey, cachekeys.VWAPProvenanceTriangulated); err != nil {
			t.Fatal(err)
		}
		cache := &orderedCache{Client: rdb, watch: map[string]string{valueKey: "value", provKey: "provenance"}}
		if failDel {
			cache.failDel = provKey
		}
		o := New(&mockStore{trades: trades}, cache, Config{Pairs: []canonical.Pair{pair}, Windows: []time.Duration{window}})
		o.clock = func() time.Time { return now }
		if err := o.Tick(context.Background()); err != nil {
			t.Fatalf("Tick: %v", err)
		}
		got, _ := mr.Get(valueKey)
		if !failDel {
			if len(cache.ops) != 2 || cache.ops[0] != "del provenance" || cache.ops[1] != "set value" {
				t.Errorf("write order = %v, want [del provenance, set value]", cache.ops)
			}
			if want := formatRatFixed(big.NewRat(lkgQuoteAmount, lkgBaseAmount), 12); got != want || mr.Exists(provKey) {
				t.Errorf("value = %q, marker present = %v; want %q unmarked", got, mr.Exists(provKey), want)
			}
			continue
		}
		if got != "0.100000000000" || !mr.Exists(provKey) {
			t.Errorf("failed clear: value = %q, marker present = %v; want the prior composite untouched", got, mr.Exists(provKey))
		}
		if s := o.Stats(); s.VWAPWrites != 0 || s.Errors != 1 {
			t.Errorf("failed clear: VWAPWrites = %d, Errors = %d; want 0 and 1", s.VWAPWrites, s.Errors)
		}
	}
}
