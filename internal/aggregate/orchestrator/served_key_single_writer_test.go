package orchestrator

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// valueLogCache records every value written to one key, whether by a plain
// SET or inside a transaction.
type valueLogCache struct {
	*redis.Client
	key    string
	values []string
}

func (c *valueLogCache) Set(ctx context.Context, key string, value any, exp time.Duration) *redis.StatusCmd {
	if key == c.key {
		c.values = append(c.values, fmt.Sprint(value))
	}
	return c.Client.Set(ctx, key, value, exp)
}

type valueLogPipe struct {
	redis.Pipeliner
	c *valueLogCache
}

func (p valueLogPipe) Set(ctx context.Context, key string, value any, exp time.Duration) *redis.StatusCmd {
	if key == p.c.key {
		p.c.values = append(p.c.values, fmt.Sprint(value))
	}
	return p.Pipeliner.Set(ctx, key, value, exp)
}

func (c *valueLogCache) TxPipelined(ctx context.Context, fn func(redis.Pipeliner) error) ([]redis.Cmder, error) {
	return c.Client.TxPipelined(ctx, func(p redis.Pipeliner) error { return fn(valueLogPipe{p, c}) })
}

type streamEvent struct {
	pair       string
	value      string
	observedAt time.Time
}

type recordingStream struct{ events []streamEvent }

func (s *recordingStream) PublishClosedBucket(
	_ context.Context, pair canonical.Pair, _ time.Duration, value string, observedAt time.Time,
) error {
	s.events = append(s.events, streamEvent{pair.String(), value, observedAt})
	return nil
}

// A pair that is both a direct pair and a triangulation target has one
// writer per tick on its served key. Two ticks inside one closed bucket —
// the deciding tick and a replaying one — must never write the direct
// print to the key the composite serves from, and the stream must carry
// the composite, once, stamped with the bucket end.
func TestTriangulationTarget_ServedKeyNeverHoldsTheDirectPrintUnderTheComposite(t *testing.T) {
	ctx := context.Background()
	window := 5 * time.Minute
	leg1 := xlmUsdtPair(t)
	leg2 := mkPair(t, "crypto", "USDT", "fiat", "EUR")
	target := mkPair(t, "crypto", "XLM", "fiat", "EUR")
	t0 := time.Date(2026, 9, 24, 12, 0, 10, 0, time.UTC)
	bucketEnd := t0.Truncate(closedBucket)
	ts := t0.Add(-2 * time.Minute)

	rdb, mr := newTestRedis(t)
	targetKey := cachekeys.VWAP(target.Base, target.Quote, window).String()
	cache := &valueLogCache{Client: rdb, key: targetKey}
	stream := &recordingStream{}
	o := New(&mockStore{perPair: map[string][]canonical.Trade{
		leg1.String():   {buildTrade(t, big.NewInt(100_000_000), big.NewInt(100_000_000), ts)},
		target.String(): {buildTrade(t, big.NewInt(100_000_000), big.NewInt(200_000_000), ts)},
	}}, cache, Config{
		Pairs:           []canonical.Pair{leg1, target},
		Windows:         []time.Duration{window},
		StreamPublisher: stream,
		Triangulations:  []TriangulationChain{{Target: target, Legs: []canonical.Pair{leg1, leg2}}},
	})
	if err := mr.Set(cachekeys.VWAP(leg2.Base, leg2.Quote, window).String(), "0.900000000000"); err != nil {
		t.Fatal(err)
	}

	direct := formatRatFixed(big.NewRat(2, 1), 12)
	composite := formatRatFixed(big.NewRat(9, 10), 12)
	for i, now := range []time.Time{t0, t0.Add(20 * time.Second)} {
		o.clock = func() time.Time { return now }
		if err := o.Tick(ctx); err != nil {
			t.Fatalf("tick %d: %v", i+1, err)
		}
		if got, _ := mr.Get(targetKey); got != composite {
			t.Fatalf("tick %d: served %q, want the composite %q", i+1, got, composite)
		}
	}
	for _, v := range cache.values {
		if v == direct {
			t.Errorf("the direct print %s was written to the served key under the composite (writes %v)", direct, cache.values)
		}
	}

	var targetEvents []streamEvent
	for _, e := range stream.events {
		if e.pair == target.String() {
			targetEvents = append(targetEvents, e)
		}
	}
	if len(targetEvents) != 1 || targetEvents[0].value != composite || !targetEvents[0].observedAt.Equal(bucketEnd) {
		t.Errorf("target stream events = %+v, want one composite %s stamped %s", targetEvents, composite, bucketEnd)
	}
}
