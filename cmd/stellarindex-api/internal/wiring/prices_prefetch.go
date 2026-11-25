package wiring

import (
	"context"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricing/readset"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

var (
	triangulatedSeam = readset.NewSeam[readset.PairKey, v1.CachedVWAP]("triangulated", cloneCachedVWAP)
	freezeSeam       = readset.NewSeam[readset.PairKey, struct{}]("freeze", nil)
)

// cloneCachedVWAP copies Coverage so a memoised value is never shared between readers.
func cloneCachedVWAP(v v1.CachedVWAP) v1.CachedVWAP {
	if v.Coverage != nil {
		c := *v.Coverage
		v.Coverage = &c
	}
	return v
}

// PrefetchTriangulatedLooker is [RedisTriangulatedLooker] answering a key the request's batch prefetch read from that
// read, and every other key exactly as before.
type PrefetchTriangulatedLooker struct{ RedisTriangulatedLooker }

// LookupTriangulatedVWAP implements [v1.TriangulatedPriceLooker].
func (p PrefetchTriangulatedLooker) LookupTriangulatedVWAP(
	ctx context.Context, base, quote canonical.Asset, window time.Duration,
) (v1.CachedVWAP, bool, error) {
	if readset.From(ctx) == nil { // the single /v1/price path: no key or closure to build
		return p.RedisTriangulatedLooker.LookupTriangulatedVWAP(ctx, base, quote, window)
	}
	return triangulatedSeam.Read(ctx, readset.Pair(base, quote, window), func(ctx context.Context) (v1.CachedVWAP, bool, error) {
		return p.RedisTriangulatedLooker.LookupTriangulatedVWAP(ctx, base, quote, window)
	})
}

// PrefetchTriangulatedVWAP makes each read's four-key MGET, intact, in one pipeline and fills ctx's memo with the
// answers; a failed MGET leaves its key unfilled.
func (p PrefetchTriangulatedLooker) PrefetchTriangulatedVWAP(ctx context.Context, reads []readset.PairRead) {
	if p.RDB == nil || readset.From(ctx) == nil || len(reads) == 0 {
		return
	}
	pipe := p.RDB.Pipeline()
	cmds := make([]*redis.SliceCmd, len(reads))
	for i, r := range reads {
		cmds[i] = pipe.MGet(ctx, vwapMGetKeys(r.Base, r.Quote, r.Window)...)
	}
	_, _ = pipe.Exec(ctx) // per-command errors are read below
	for i, r := range reads {
		got, err := cmds[i].Result()
		if err != nil {
			triangulatedSeam.Failed()
			continue
		}
		v, found := cachedVWAPFrom(got)
		triangulatedSeam.Fill(ctx, r.Key(), v, found)
	}
}

// latestPrefetcher is the optional [PriceStore] capability [timescale.PrefetchStore] has.
type latestPrefetcher interface {
	PrefetchLatest(ctx context.Context, pairs []canonical.Pair, limit int)
}

// PrefetchLatestPrices prefetches, when S can, the store reads [StorePriceReader.LatestPrice] makes for each read's
// pair: its latest closed bucket and the serving guard's trailing buckets. A read that is not a pair is skipped, as
// LatestPrice reads nothing for it.
func (r StorePriceReader) PrefetchLatestPrices(ctx context.Context, reads []readset.PairRead) {
	pf, ok := r.S.(latestPrefetcher)
	if !ok {
		return
	}
	pairs := make([]canonical.Pair, 0, len(reads))
	for _, read := range reads {
		if pair, err := canonical.NewPair(read.Base, read.Quote); err == nil {
			pairs = append(pairs, pair)
		}
	}
	pf.PrefetchLatest(ctx, pairs, pricingguard.SampleFetch)
}

// PrefetchFreezeLooker is a freeze-marker looker answering a pair the request's batch prefetch read from that read,
// and every other pair through Looker exactly as before.
type PrefetchFreezeLooker struct {
	Looker v1.FrozenLooker
	RDB    redis.UniversalClient
}

// FrozenForPair implements [v1.FrozenLooker].
func (p PrefetchFreezeLooker) FrozenForPair(ctx context.Context, asset, quote canonical.Asset) (bool, error) {
	if readset.From(ctx) == nil {
		return p.Looker.FrozenForPair(ctx, asset, quote)
	}
	_, frozen, err := freezeSeam.Read(ctx, readset.Pair(asset, quote, 0), func(ctx context.Context) (struct{}, bool, error) {
		frozen, err := p.Looker.FrozenForPair(ctx, asset, quote)
		return struct{}{}, frozen, err
	})
	return frozen, err
}

// PrefetchFrozen GETs each pair's freeze marker in one pipeline and fills ctx's memo: a marker is frozen, redis.Nil
// is not, and any other error leaves the pair unfilled. Windows on reads are ignored.
func (p PrefetchFreezeLooker) PrefetchFrozen(ctx context.Context, reads []readset.PairRead) {
	if p.RDB == nil || readset.From(ctx) == nil || len(reads) == 0 {
		return
	}
	pipe := p.RDB.Pipeline()
	cmds := make([]*redis.StringCmd, len(reads))
	for i, r := range reads {
		cmds[i] = pipe.Get(ctx, cachekeys.Freeze(r.Base, r.Quote).String())
	}
	_, _ = pipe.Exec(ctx) // per-command errors are read below
	for i, r := range reads {
		err := cmds[i].Err()
		switch {
		case err == nil:
			freezeSeam.Fill(ctx, readset.Pair(r.Base, r.Quote, 0), struct{}{}, true)
		case errors.Is(err, redis.Nil):
			freezeSeam.Fill(ctx, readset.Pair(r.Base, r.Quote, 0), struct{}{}, false)
		default:
			freezeSeam.Failed()
		}
	}
}
