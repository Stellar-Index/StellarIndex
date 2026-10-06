package wiring

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// StoreMarketsReader adapts *timescale.Store to v1.MarketsReader.
// Translates timescale.Market (typed Pair) to v1.Market (string
// wire shape) so the API layer owns its own schema.
type StoreMarketsReader struct{ S *timescale.Store }

func (r StoreMarketsReader) DistinctPairsExt(ctx context.Context, cursor string, limit int, order timescale.MarketsOrder) ([]v1.Market, string, error) {
	rows, next, err := r.S.DistinctPairsExt(ctx, cursor, limit, order)
	if err != nil {
		return nil, "", err
	}
	out := make([]v1.Market, len(rows))
	for i, m := range rows {
		out[i] = v1.Market{
			Base:          m.Pair.Base.String(),
			Quote:         m.Pair.Quote.String(),
			LastTradeAt:   v1.WireTime(m.LastTradeAt),
			BucketCloseAt: v1.WireTime(m.BucketCloseAt),
			TradeCount24h: m.TradeCount24h,
			Volume24hUSD:  m.Volume24hUSD,
			LastPrice:     m.LastPrice,
		}
	}
	return out, next, nil
}

func (r StoreMarketsReader) SourceMarkets(ctx context.Context, source, cursor string, limit int, order timescale.MarketsOrder) ([]v1.Market, string, error) {
	rows, next, err := r.S.SourceMarkets(ctx, source, cursor, limit, order)
	if err != nil {
		return nil, "", err
	}
	out := make([]v1.Market, len(rows))
	for i, m := range rows {
		out[i] = v1.Market{
			Base:          m.Pair.Base.String(),
			Quote:         m.Pair.Quote.String(),
			LastTradeAt:   v1.WireTime(m.LastTradeAt),
			BucketCloseAt: v1.WireTime(m.BucketCloseAt),
			TradeCount24h: m.TradeCount24h,
			Volume24hUSD:  m.Volume24hUSD,
			LastPrice:     m.LastPrice,
		}
	}
	return out, next, nil
}

func (r StoreMarketsReader) AssetMarkets(ctx context.Context, asset, cursor string, limit int, order timescale.MarketsOrder) ([]v1.Market, string, error) {
	rows, next, err := r.S.AssetMarkets(ctx, asset, cursor, limit, order)
	if err != nil {
		return nil, "", err
	}
	out := make([]v1.Market, len(rows))
	for i, m := range rows {
		out[i] = v1.Market{
			Base:          m.Pair.Base.String(),
			Quote:         m.Pair.Quote.String(),
			LastTradeAt:   v1.WireTime(m.LastTradeAt),
			BucketCloseAt: v1.WireTime(m.BucketCloseAt),
			TradeCount24h: m.TradeCount24h,
			Volume24hUSD:  m.Volume24hUSD,
			LastPrice:     m.LastPrice,
		}
	}
	return out, next, nil
}

func (r StoreMarketsReader) AllPools(ctx context.Context, filter timescale.PoolsFilter, cursor string, limit int, order timescale.MarketsOrder) ([]v1.Pool, string, error) {
	rows, next, err := r.S.AllPools(ctx, filter, cursor, limit, order)
	if err != nil {
		return nil, "", err
	}
	out := make([]v1.Pool, len(rows))
	for i, p := range rows {
		out[i] = v1.Pool{
			Source:        p.Source,
			Base:          p.Pair.Base.String(),
			Quote:         p.Pair.Quote.String(),
			LastTradeAt:   v1.WireTime(p.LastTradeAt),
			TradeCount24h: p.TradeCount24h,
			Volume24hUSD:  p.Volume24hUSD,
			LastPrice:     p.LastPrice,
		}
	}
	return out, next, nil
}

func (r StoreMarketsReader) PairMarket(ctx context.Context, base, quote canonical.Asset) (v1.Market, bool, error) {
	m, ok, err := r.S.PairMarket(ctx, base, quote)
	if err != nil || !ok {
		return v1.Market{}, ok, err
	}
	return v1.Market{
		Base:          m.Pair.Base.String(),
		Quote:         m.Pair.Quote.String(),
		LastTradeAt:   v1.WireTime(m.LastTradeAt),
		BucketCloseAt: v1.WireTime(m.BucketCloseAt),
		TradeCount24h: m.TradeCount24h,
		Volume24hUSD:  m.Volume24hUSD,
		LastPrice:     m.LastPrice,
	}, true, nil
}

func (r StoreMarketsReader) GetPairsVolumeHistory24hBatch(ctx context.Context, pairs [][2]string) (map[string][]timescale.PairVolumePoint, error) {
	return r.S.GetPairsVolumeHistory24hBatch(ctx, pairs)
}

func (r StoreMarketsReader) FirstTradeBatch(ctx context.Context, pairs [][2]string) (map[string]time.Time, error) {
	return r.S.FirstTradeBatch(ctx, pairs)
}

type CachedMarketsReader struct {
	Inner v1.MarketsReader
	RDB   redis.UniversalClient
	Log   *slog.Logger
}

// FirstTradeBatch delegates uncached: inception timestamps are
// immutable once set, the call is already gated behind an opt-in
// include param, and the underlying MIN is index-assisted.
func (r CachedMarketsReader) FirstTradeBatch(ctx context.Context, pairs [][2]string) (map[string]time.Time, error) {
	return r.Inner.FirstTradeBatch(ctx, pairs)
}

func (r CachedMarketsReader) PairMarket(ctx context.Context, base, quote canonical.Asset) (v1.Market, bool, error) {
	return r.Inner.PairMarket(ctx, base, quote)
}

// GetPairsVolumeHistory24hBatch — pass-through. The query runs at
// page granularity (max 500 pairs) and the result depends on the
// 24h time window; not worth caching since invalidation tracks
// every minute boundary.
func (r CachedMarketsReader) GetPairsVolumeHistory24hBatch(ctx context.Context, pairs [][2]string) (map[string][]timescale.PairVolumePoint, error) {
	return r.Inner.GetPairsVolumeHistory24hBatch(ctx, pairs)
}

func (r CachedMarketsReader) AllPools(ctx context.Context, filter timescale.PoolsFilter, cursor string, limit int, order timescale.MarketsOrder) ([]v1.Pool, string, error) {
	// Pools queries are heavy (group by source × pair); cache
	// follows the same TTL as the markets list. Cache key
	// includes the filter so pools-with-DEX-filter, pools-by-pair,
	// and unfiltered pools don't collide.
	if r.RDB == nil {
		return r.Inner.AllPools(ctx, filter, cursor, limit, order)
	}
	cacheKey := cachekeys.MarketsListPools(cursor, limit, marketsOrderKey(order), filter.Sources, filter.Base, filter.Quote, filter.Asset)
	if raw, err := r.RDB.Get(ctx, cacheKey.String()).Bytes(); err == nil {
		var p listCachePayload[v1.Pool]
		if jerr := json.Unmarshal(raw, &p); jerr == nil {
			return p.Items, p.NextCursor, nil
		}
		r.Log.Warn("pools cache decode failed", "key", cacheKey)
	} else if !errors.Is(err, redis.Nil) {
		r.Log.Warn("pools cache read failed", "key", cacheKey, "err", err)
	}
	items, next, err := r.Inner.AllPools(ctx, filter, cursor, limit, order)
	if err != nil {
		return nil, "", err
	}
	if buf, jerr := json.Marshal(listCachePayload[v1.Pool]{Items: items, NextCursor: next}); jerr == nil {
		if serr := r.RDB.Set(ctx, cacheKey.String(), buf, cachekeys.CatalogueListTTL).Err(); serr != nil {
			r.Log.Warn("pools cache write failed", "key", cacheKey, "err", serr)
		}
	}
	return items, next, nil
}

func (r CachedMarketsReader) SourceMarkets(ctx context.Context, source, cursor string, limit int, order timescale.MarketsOrder) ([]v1.Market, string, error) {
	// Per-source markets share the same cache shape as
	// DistinctPairsExt but partition by source so a source's pool
	// list isn't aliased with the global one.
	if r.RDB == nil {
		return r.Inner.SourceMarkets(ctx, source, cursor, limit, order)
	}
	cacheKey := cachekeys.MarketsListBySource(cursor, limit, marketsOrderKey(order), source)
	if raw, err := r.RDB.Get(ctx, cacheKey.String()).Bytes(); err == nil {
		var p listCachePayload[v1.Market]
		if jerr := json.Unmarshal(raw, &p); jerr == nil {
			return p.Items, p.NextCursor, nil
		}
		r.Log.Warn("source-markets cache decode failed", "key", cacheKey)
	} else if !errors.Is(err, redis.Nil) {
		r.Log.Warn("source-markets cache read failed", "key", cacheKey, "err", err)
	}

	items, next, err := r.Inner.SourceMarkets(ctx, source, cursor, limit, order)
	if err != nil {
		return nil, "", err
	}
	if buf, jerr := json.Marshal(listCachePayload[v1.Market]{Items: items, NextCursor: next}); jerr == nil {
		if serr := r.RDB.Set(ctx, cacheKey.String(), buf, cachekeys.CatalogueListTTL).Err(); serr != nil {
			r.Log.Warn("source-markets cache write failed", "key", cacheKey, "err", serr)
		}
	}
	return items, next, nil
}

func (r CachedMarketsReader) AssetMarkets(ctx context.Context, asset, cursor string, limit int, order timescale.MarketsOrder) ([]v1.Market, string, error) {
	// Per-asset markets share the same cache shape as
	// DistinctPairsExt but partition by asset so an asset's
	// involvement list isn't aliased with the global one.
	if r.RDB == nil {
		return r.Inner.AssetMarkets(ctx, asset, cursor, limit, order)
	}
	cacheKey := cachekeys.MarketsListByAsset(cursor, limit, marketsOrderKey(order), asset)
	if raw, err := r.RDB.Get(ctx, cacheKey.String()).Bytes(); err == nil {
		var p listCachePayload[v1.Market]
		if jerr := json.Unmarshal(raw, &p); jerr == nil {
			return p.Items, p.NextCursor, nil
		}
		r.Log.Warn("asset-markets cache decode failed", "key", cacheKey)
	} else if !errors.Is(err, redis.Nil) {
		r.Log.Warn("asset-markets cache read failed", "key", cacheKey, "err", err)
	}

	items, next, err := r.Inner.AssetMarkets(ctx, asset, cursor, limit, order)
	if err != nil {
		return nil, "", err
	}
	if buf, jerr := json.Marshal(listCachePayload[v1.Market]{Items: items, NextCursor: next}); jerr == nil {
		if serr := r.RDB.Set(ctx, cacheKey.String(), buf, cachekeys.CatalogueListTTL).Err(); serr != nil {
			r.Log.Warn("asset-markets cache write failed", "key", cacheKey, "err", serr)
		}
	}
	return items, next, nil
}

func (r CachedMarketsReader) DistinctPairsExt(ctx context.Context, cursor string, limit int, order timescale.MarketsOrder) ([]v1.Market, string, error) {
	if r.RDB == nil {
		return r.Inner.DistinctPairsExt(ctx, cursor, limit, order)
	}
	cacheKey := cachekeys.MarketsListOrdered(cursor, limit, marketsOrderKey(order))
	if raw, err := r.RDB.Get(ctx, cacheKey.String()).Bytes(); err == nil {
		var p listCachePayload[v1.Market]
		if jerr := json.Unmarshal(raw, &p); jerr == nil {
			return p.Items, p.NextCursor, nil
		}
		r.Log.Warn("markets cache decode failed", "key", cacheKey)
	} else if !errors.Is(err, redis.Nil) {
		r.Log.Warn("markets cache read failed", "key", cacheKey, "err", err)
	}

	items, next, err := r.Inner.DistinctPairsExt(ctx, cursor, limit, order)
	if err != nil {
		return nil, "", err
	}
	if buf, jerr := json.Marshal(listCachePayload[v1.Market]{Items: items, NextCursor: next}); jerr == nil {
		if serr := r.RDB.Set(ctx, cacheKey.String(), buf, cachekeys.CatalogueListTTL).Err(); serr != nil {
			r.Log.Warn("markets cache write failed", "key", cacheKey, "err", serr)
		}
	}
	return items, next, nil
}

func marketsOrderKey(o timescale.MarketsOrder) string {
	switch o {
	case timescale.MarketsOrderVolume24hDesc:
		return "vol_desc"
	default:
		return "pair"
	}
}
