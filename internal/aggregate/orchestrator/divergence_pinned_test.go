package orchestrator

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/divergence"
)

// fixedReference quotes one price for every pair.
type fixedReference struct {
	name  string
	price float64
}

func (r fixedReference) Name() string { return r.name }

func (r fixedReference) LookupQuote(_ context.Context, _ canonical.Pair, at time.Time) (divergence.Quote, error) {
	return divergence.Quote{Price: r.price, AsOf: at}, nil
}

const (
	pinnedLKG   = 0.100 // the last-known-good a freeze keeps serving
	marketPrice = 0.108 // where the market settled: +8%, past the 5% threshold
)

// pinnedDivergenceRig is an orchestrator driving a real divergence
// service whose references all quote marketPrice, with pinnedLKG cached
// as the pair's shortest-window VWAP.
func pinnedDivergenceRig(t *testing.T) (*Orchestrator, *redis.Client, canonical.Pair, *atomic.Int32) {
	t.Helper()
	rdb, _ := newTestRedis(t)
	pair := pairXLMUSD(t)
	window := 5 * time.Minute
	if err := rdb.Set(context.Background(), cachekeys.VWAP(pair.Base, pair.Quote, window).String(),
		"0.1", time.Hour).Err(); err != nil {
		t.Fatalf("seed vwap: %v", err)
	}
	var hooks atomic.Int32
	svc, err := divergence.NewService(divergence.ServiceOptions{
		References: []divergence.Reference{
			fixedReference{"coingecko", marketPrice},
			fixedReference{"chainlink", marketPrice},
			fixedReference{"coinmarketcap", marketPrice},
		},
		Cache:                rdb,
		Threshold:            5,
		MinSourcesForWarning: 2,
		WarningPersistence:   5 * time.Minute,
		Logger:               silentLogger(),
		OnWarningFired: func(context.Context, canonical.Pair, divergence.CachedResult) error {
			hooks.Add(1)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	o := New(nil, rdb, Config{
		Pairs:               []canonical.Pair{pair},
		Windows:             []time.Duration{window},
		DivergenceRefresher: svc,
		Logger:              silentLogger(),
	})
	return o, rdb, pair, &hooks
}

func readDivergenceEntry(t *testing.T, rdb *redis.Client, pair canonical.Pair) divergence.CachedResult {
	t.Helper()
	raw, err := rdb.Get(context.Background(), cachekeys.Divergence(pair).String()).Bytes()
	if err != nil {
		t.Fatalf("read div entry: %v", err)
	}
	var cached divergence.CachedResult
	if err := json.Unmarshal(raw, &cached); err != nil {
		t.Fatalf("decode div entry: %v", err)
	}
	return cached
}
