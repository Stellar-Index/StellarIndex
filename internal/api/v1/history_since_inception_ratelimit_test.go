package v1_test

import (
	"context"
	"net/http"
	"strconv"
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
			ratelimit.New(rdb, anonLimit, time.Minute), nil, middleware.SkipHealthAndMetrics, nil),
	})
	return startHTTPTest(t, srv.Handler()), reader
}

// TestHistorySinceInception_ChargesByGranularity: the client's
// granularity selects how many buckets the unbounded read returns (up to
// 50,000 at 1m against ~3,650 at the 1d default), so it must select the
// price too. Each case is one request into a fresh 100-token window.
func TestHistorySinceInception_ChargesByGranularity(t *testing.T) {
	cases := []struct {
		name, query   string
		wantStatus    int
		wantRemaining int
	}{
		{"default grain", "?asset=native", 200, 99},
		{"1d", "?asset=native&granularity=1d", 200, 99},
		{"1w", "?asset=native&granularity=1w", 200, 99},
		{"1mo", "?asset=native&granularity=1mo", 200, 99},
		{"4h", "?asset=native&granularity=4h", 200, 94},
		{"1h", "?asset=native&granularity=1h", 200, 87},
		{"15m", "?asset=native&granularity=15m", 200, 87},
		{"1m", "?asset=native&granularity=1m", 200, 87},
		{"rejected before any read", "?granularity=1m", 400, 99},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, _ := newSinceInceptionLimitedServer(t, 100)
			resp := mustGet(t, ts.URL+sinceInceptionProbe+tc.query)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
			var got int
			if resp.StatusCode == http.StatusOK {
				got = remainingBeforeProbe(t, resp, ts.URL+sinceInceptionProbe)
			} else {
				n, err := strconv.Atoi(resp.Header.Get("X-RateLimit-Remaining"))
				if err != nil {
					t.Fatalf("X-RateLimit-Remaining: %v", err)
				}
				got = n
			}
			if got != tc.wantRemaining {
				t.Fatalf("remaining after request = %d, want %d", got, tc.wantRemaining)
			}
		})
	}
}

// TestHistorySinceInception_DeniedGrainDoesNoRead: the surcharge lands
// before the read. After a default request (1 token) the 13-token budget
// has 12 left; a 1m request pays its base token and cannot find the 12
// more, and the store never sees the 1m read.
func TestHistorySinceInception_DeniedGrainDoesNoRead(t *testing.T) {
	ts, reader := newSinceInceptionLimitedServer(t, 13)
	if resp := mustGet(t, ts.URL+sinceInceptionProbe+"?asset=native"); resp.StatusCode != http.StatusOK {
		t.Fatalf("default request status = %d, want 200", resp.StatusCode)
	}
	resp := mustGet(t, ts.URL+sinceInceptionProbe+"?asset=native&granularity=1m")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("1m request status = %d, want 429", resp.StatusCode)
	}
	if n := reader.oneMinuteReads.Load(); n != 0 {
		t.Fatalf("store served %d 1m read(s) for a denied request", n)
	}
}
