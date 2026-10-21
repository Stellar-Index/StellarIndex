package main

import (
	"context"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
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
