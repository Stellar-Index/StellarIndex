package v1_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// grainCountingHistoryReader counts the 1m reads that reach the store,
// so a test can assert a denial stopped the read.
type grainCountingHistoryReader struct {
	stubHistoryReader
	oneMinuteReads atomic.Int64
}

func (r *grainCountingHistoryReader) HistoryPoints(ctx context.Context, pair canonical.Pair, granularity string, limit int) ([]v1.HistoryPoint, error) {
	if granularity == "1m" {
		r.oneMinuteReads.Add(1)
	}
	return r.stubHistoryReader.HistoryPoints(ctx, pair, granularity, limit)
}

func (r *grainCountingHistoryReader) HistoryPointsInRange(ctx context.Context, pair canonical.Pair, granularity string, from, to time.Time, limit int) ([]v1.HistoryPoint, error) {
	if granularity == "1m" {
		r.oneMinuteReads.Add(1)
	}
	return r.stubHistoryReader.HistoryPointsInRange(ctx, pair, granularity, from, to, limit)
}

// sinceInceptionProbe is rejected (missing asset) before any read and
// answered no-store, so it reports the remainder the request above left.
const sinceInceptionProbe = "/v1/history/since-inception"

func newSinceInceptionLimitedServer(t *testing.T, anonLimit int) (*testServerImpl, *grainCountingHistoryReader) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	reader := &grainCountingHistoryReader{}
	srv := v1.New(v1.Options{
		History: reader,
		RateLimit: middleware.RateLimitBySubject(
			ratelimit.New(rdb, anonLimit, time.Minute, pinnedWindow), nil, middleware.SkipHealthAndMetrics, nil),
	})
	return startHTTPTest(t, srv.Handler()), reader
}
