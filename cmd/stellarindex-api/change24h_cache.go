package main

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// change24hFillBudget bounds one shared anchor read; it runs detached from the request that started it.
const change24hFillBudget = 5 * time.Second

// minuteChange24hReader shares one 24h anchor per asset per minute. The anchor is the newest bucket at or before
// now-24h-1m and buckets are minute-aligned, so every read inside one minute returns the same row; caching it changes
// no answer, it only spares the explorer build's identical batch rows a hypertable read each on fresh connections.
type minuteChange24hReader struct {
	next v1.Change24hReader
	now  func() time.Time

	flight singleflight.Group
	mu     sync.Mutex
	minute time.Time
	vals   map[string]string
}

func newMinuteChange24hReader(next v1.Change24hReader) *minuteChange24hReader {
	return &minuteChange24hReader{next: next, now: time.Now}
}

// USDPrice24hAgo caches successes only, so a transient error or an unavailable anchor is re-read on the next call.
func (r *minuteChange24hReader) USDPrice24hAgo(ctx context.Context, asset canonical.Asset) (string, error) {
	minute := r.now().Truncate(time.Minute)
	key := asset.String()
	r.mu.Lock()
	if !r.minute.Equal(minute) {
		r.minute, r.vals = minute, make(map[string]string)
	}
	v, ok := r.vals[key]
	r.mu.Unlock()
	if ok {
		return v, nil
	}
	ch := r.flight.DoChan(minute.Format(time.RFC3339)+"|"+key, func() (any, error) {
		fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), change24hFillBudget)
		defer cancel()
		got, err := r.next.USDPrice24hAgo(fctx, asset)
		if err == nil {
			r.mu.Lock()
			if r.minute.Equal(minute) {
				r.vals[key] = got
			}
			r.mu.Unlock()
		}
		return got, err
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return "", res.Err
		}
		return res.Val.(string), nil //nolint:forcetypeassert // the flight returns only strings
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// change24hPrefetcher is the batch-prefetch capability of a [v1.Change24hReader].
type change24hPrefetcher interface {
	PrefetchUSDPrice24hAgo(ctx context.Context, assets []canonical.Asset)
}

// PrefetchUSDPrice24hAgo prefetches beneath the cache, for the assets it holds no anchor for this minute: the cache
// is filled only by USDPrice24hAgo, at its own minute, so a prefetched read reaches it only as that call's read.
// A flight follower may get a memo-derived anchor: flight and cache are keyed minute+asset, the cache already serves a
// same-minute anchor to every caller, and a memo entry for another minute misses by key.
func (r *minuteChange24hReader) PrefetchUSDPrice24hAgo(ctx context.Context, assets []canonical.Asset) {
	pf, ok := r.next.(change24hPrefetcher)
	if !ok {
		return
	}
	minute := r.now().Truncate(time.Minute)
	cold := make([]canonical.Asset, 0, len(assets))
	r.mu.Lock()
	for _, a := range assets {
		if _, held := r.vals[a.String()]; !held || !r.minute.Equal(minute) {
			cold = append(cold, a)
		}
	}
	r.mu.Unlock()
	pf.PrefetchUSDPrice24hAgo(ctx, cold)
}

// change24hAnchorPrefetcher is the optional capability of [timescale.PrefetchAnchors].
type change24hAnchorPrefetcher interface {
	PrefetchChange24hAnchors(ctx context.Context, pairs []canonical.Pair, at, notBefore time.Time, limit int)
}

// PrefetchUSDPrice24hAgo prefetches, when the store can, the reads [storeChange24hReader.USDPrice24hAgo] may make for
// each asset: the anchor in its literal USD pair and in each peg pair it may walk, and the guard's baseline behind each.
// The withholding gates are not prefetched: they are asked as of each read's own instant, after its anchor.
func (r storeChange24hReader) PrefetchUSDPrice24hAgo(ctx context.Context, assets []canonical.Asset) {
	pf, ok := r.s.(change24hAnchorPrefetcher)
	if !ok || len(assets) == 0 {
		return
	}
	at := time.Now().Add(-24 * time.Hour)
	pairs := make([]canonical.Pair, 0, len(assets)*(1+len(r.pegs)))
	for _, a := range assets {
		pairs = append(pairs, canonical.Pair{Base: a, Quote: usdQuoteAsset})
		for _, peg := range r.pegs {
			if !canonical.CanonicalAsset(peg).Equal(canonical.CanonicalAsset(a)) {
				pairs = append(pairs, canonical.Pair{Base: a, Quote: peg})
			}
		}
	}
	pf.PrefetchChange24hAnchors(ctx, pairs, at, at.Add(-timescale.Change24hAnchorTolerance), pricingguard.SampleFetch)
}
