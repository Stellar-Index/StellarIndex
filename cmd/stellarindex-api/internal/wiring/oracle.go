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

// StoreOracleReader adapts *timescale.Store to v1.OracleReader.
type StoreOracleReader struct{ S *timescale.Store }

func (r StoreOracleReader) LatestOracleUpdatesForAsset(ctx context.Context, asset canonical.Asset, sourceFilter string) ([]canonical.OracleUpdate, error) {
	return r.S.LatestOracleUpdatesForAsset(ctx, asset, sourceFilter)
}

func (r StoreOracleReader) LatestOracleUpdatesForAssets(ctx context.Context, assets []canonical.Asset, sourceFilter string) ([]canonical.OracleUpdate, error) {
	return r.S.LatestOracleUpdatesForAssets(ctx, assets, sourceFilter)
}

func (r StoreOracleReader) LatestOracleStreams(ctx context.Context) ([]canonical.OracleUpdate, error) {
	return r.S.LatestOracleStreams(ctx)
}

// CachedOracleReader wraps an inner OracleReader with a Redis
// read-through cache. The inner DISTINCT ON (source) sort is
// expensive (~580 ms p95 on R1's oracle_updates volume); the
// reading only refreshes every 1–5 minutes, so a 30 s Redis
// entry absorbs the polling fan-out without delaying customer-
// facing freshness in any meaningful way.
//
// Cache miss: hit the inner reader, then SET. Cache hit: deserialise
// and skip the DB. Errors on either side fall through to the inner
// reader — never fail open.
type CachedOracleReader struct {
	Inner v1.OracleReader
	RDB   redis.UniversalClient
	Log   *slog.Logger
}

func (r CachedOracleReader) LatestOracleUpdatesForAsset(ctx context.Context, asset canonical.Asset, sourceFilter string) ([]canonical.OracleUpdate, error) {
	return r.LatestOracleUpdatesForAssets(ctx, []canonical.Asset{asset}, sourceFilter)
}

func (r CachedOracleReader) LatestOracleUpdatesForAssets(ctx context.Context, assets []canonical.Asset, sourceFilter string) ([]canonical.OracleUpdate, error) {
	rows, _, err := r.LatestOracleUpdatesForAssetsAt(ctx, assets, sourceFilter)
	return rows, err
}

// oracleLatestEntry is the Redis value: the rows plus when the DB query that
// produced them started, so a reader stamps as_of with the data's time, not
// the time it re-read Redis.
type oracleLatestEntry struct {
	ComputedAt time.Time                `json:"computed_at"`
	Updates    []canonical.OracleUpdate `json:"updates"`
}

// LatestOracleUpdatesForAssetsAt is LatestOracleUpdatesForAssets plus the
// time the served rows were computed. A legacy bare-array entry carries no
// time, so it is read as a miss rather than stamped.
func (r CachedOracleReader) LatestOracleUpdatesForAssetsAt(ctx context.Context, assets []canonical.Asset, sourceFilter string) ([]canonical.OracleUpdate, time.Time, error) {
	if r.RDB == nil {
		computedAt := time.Now()
		rows, err := r.Inner.LatestOracleUpdatesForAssets(ctx, assets, sourceFilter)
		return rows, computedAt, err
	}

	keys := make([]string, len(assets))
	for i, a := range assets {
		keys[i] = a.String()
	}
	cacheKey := cachekeys.OracleLatest(keys, sourceFilter)

	raw, err := r.RDB.Get(ctx, cacheKey.String()).Bytes()
	switch {
	case err == nil:
		if entry, ok := r.decodeEntry(cacheKey, raw); ok {
			return entry.Updates, entry.ComputedAt, nil
		}
	case errors.Is(err, redis.Nil):
		// miss — proceed to DB
	default:
		r.Log.Warn("oracle cache read failed; falling through to DB",
			"key", cacheKey, "err", err)
	}

	computedAt := time.Now()
	updates, err := r.Inner.LatestOracleUpdatesForAssets(ctx, assets, sourceFilter)
	if err != nil {
		return nil, time.Time{}, err
	}
	if buf, jerr := json.Marshal(oracleLatestEntry{ComputedAt: computedAt, Updates: updates}); jerr == nil {
		if serr := r.RDB.Set(ctx, cacheKey.String(), buf, cachekeys.OracleLatestTTL).Err(); serr != nil {
			r.Log.Warn("oracle cache write failed", "key", cacheKey, "err", serr)
		}
	}
	return updates, computedAt, nil
}

// decodeEntry reports false for anything that cannot supply a computed time:
// a legacy bare-array entry, a zero time, or a payload that fails to decode.
func (r CachedOracleReader) decodeEntry(key cachekeys.OracleLatestKey, raw []byte) (oracleLatestEntry, bool) {
	var entry oracleLatestEntry
	if len(raw) > 0 && raw[0] == '[' {
		return entry, false
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		// Bad payload — log and re-read; don't fail the request on a
		// cache deserialisation glitch.
		r.Log.Warn("oracle cache decode failed; falling through to DB", "key", key, "err", err)
		return entry, false
	}
	return entry, !entry.ComputedAt.IsZero()
}

// LatestOracleStreams pass-through — the underlying scan is one
// query against oracle_updates with DISTINCT ON. Cheap enough to
// skip the cache layer at this volume; revisit if the page becomes
// a hot endpoint.
func (r CachedOracleReader) LatestOracleStreams(ctx context.Context) ([]canonical.OracleUpdate, error) {
	return r.Inner.LatestOracleStreams(ctx)
}
