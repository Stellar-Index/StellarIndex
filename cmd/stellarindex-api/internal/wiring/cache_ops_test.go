package wiring

import (
	"context"
	"log/slog"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type stubAssets struct{ v1.AssetReader }

func (stubAssets) ListAssets(context.Context, string, int) ([]v1.AssetDetail, string, error) {
	return []v1.AssetDetail{{AssetID: "native"}}, "", nil
}

type stubMarkets struct{ v1.MarketsReader }

func (stubMarkets) DistinctPairsExt(context.Context, string, int, timescale.MarketsOrder) ([]v1.Market, string, error) {
	return []v1.Market{}, "", nil
}

func cacheOps(cache, op, result string) float64 {
	return testutil.ToFloat64(obs.APICacheOpsTotal.WithLabelValues(cache, op, result))
}

func TestRedisListCachesEmitCacheOps(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	log := slog.New(slog.DiscardHandler)
	ctx := context.Background()

	cases := []struct {
		name, cache, op string
		call            func() error
	}{
		{"assets", "assets_redis", "list_assets", func() error {
			_, _, err := CachedAssetReader{Inner: stubAssets{}, RDB: rdb, Log: log}.ListAssets(ctx, "", 10)
			return err
		}},
		{"markets", "markets_redis", "distinct_pairs_ext", func() error {
			_, _, err := CachedMarketsReader{Inner: stubMarkets{}, RDB: rdb, Log: log}.DistinctPairsExt(ctx, "", 10, timescale.MarketsOrderVolume24hDesc)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			miss, hit, errs := cacheOps(tc.cache, tc.op, "miss"), cacheOps(tc.cache, tc.op, "hit"), cacheOps(tc.cache, tc.op, "error")
			if err := tc.call(); err != nil {
				t.Fatal(err)
			}
			if got := cacheOps(tc.cache, tc.op, "miss"); got != miss+1 {
				t.Fatalf("miss = %v, want %v", got, miss+1)
			}
			if err := tc.call(); err != nil {
				t.Fatal(err)
			}
			if got := cacheOps(tc.cache, tc.op, "hit"); got != hit+1 {
				t.Fatalf("hit = %v, want %v", got, hit+1)
			}
			mr.SetError("boom")
			defer mr.SetError("")
			if err := tc.call(); err != nil {
				t.Fatal(err)
			}
			if got := cacheOps(tc.cache, tc.op, "error"); got != errs+1 {
				t.Fatalf("error = %v, want %v", got, errs+1)
			}
		})
	}
}
