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
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// rwaCountingListStub counts the per-issuer listing reads /v1/rwa/assets
// makes, so a test can tie the charge to the work and prove a denial
// stopped the reads.
type rwaCountingListStub struct {
	*rwaListStub
	lists atomic.Int64
}

func (l *rwaCountingListStub) ListAssetsExt(ctx context.Context, opts timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	l.lists.Add(1)
	return l.rwaListStub.ListAssetsExt(ctx, opts)
}

// newRWALimitedServer wires /v1/rwa/assets with two admitted classic
// member issuers behind the production limiter over a Redis-backed
// anonymous bucket of anonLimit tokens.
func newRWALimitedServer(t *testing.T, anonLimit int) (*testServerImpl, *rwaCountingListStub) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	rows := map[string][]timescale.AssetRow{
		rwaGoodIssuer: {rwaRow("USTRY", rwaGoodIssuer, sptr("1.0412"), 346312)},
		rwaOndoIssuer: {rwaRow("USDY", rwaOndoIssuer, sptr("1.0923"), 90211)},
	}
	reader := &rwaCountingListStub{rwaListStub: &rwaListStub{
		stubAssetsReaderExt: &stubAssetsReaderExt{},
		byIssuer:            rows,
		supply:              rwaSupplyFor(rows),
	}}
	srv := v1.New(v1.Options{
		Sep1Cache: &stubSep1BoundReader{bound: []timescale.Sep1BoundCurrency{
			rwaBound("USTRY", rwaGoodIssuer, "etherfuse.com", "bond"),
			rwaBound("USDY", rwaOndoIssuer, "etherfuse.com", "bond"),
		}},
		Directory: &stubDirectoryReader{entries: map[string]timescale.DirectoryEntry{
			rwaGoodIssuer: recognisedIssuer(rwaGoodIssuer, "Etherfuse"),
			rwaOndoIssuer: recognisedIssuer(rwaOndoIssuer, "Ondo"),
		}},
		AssetsReader: reader,
		RateLimit: middleware.RateLimitBySubject(
			ratelimit.New(rdb, anonLimit, time.Minute, pinnedWindow), nil, middleware.SkipHealthAndMetrics, nil),
	})
	return startHTTPTest(t, srv.Handler()), reader
}

// rwaRemaining reads the post-request remainder, through the no-store
// probe when the response is shared-cacheable and so carries none.
func rwaRemaining(t *testing.T, ts *testServerImpl, resp *http.Response) int {
	t.Helper()
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "" {
		n, err := strconv.Atoi(got)
		if err != nil {
			t.Fatalf("X-RateLimit-Remaining %q: %v", got, err)
		}
		return n
	}
	return remainingBeforeProbe(t, resp, ts.URL+assetsProbe)
}

// TestRWAAssets_ChargesOneTokenPerListingRead: /v1/rwa/assets runs one
// uncached /v1/assets listing read (and its pipeline) per member issuer
// on every request, so two member issuers cost two tokens, not the one
// the pre-dispatch charge takes.
func TestRWAAssets_ChargesOneTokenPerListingRead(t *testing.T) {
	ts, reader := newRWALimitedServer(t, 100)
	resp := mustGet(t, ts.URL+"/v1/rwa/assets")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := reader.lists.Load(); got != 2 {
		t.Fatalf("listing reads = %d, want 2 (one per member issuer)", got)
	}
	if got := rwaRemaining(t, ts, resp); got != 98 {
		t.Fatalf("X-RateLimit-Remaining = %d, want 98", got)
	}
}

// TestRWAAssets_ExhaustsTheBucketInLimitOverCostRequests: a 4-token
// budget buys two 2-token requests, and the third is refused with a
// truthful 429 before any listing read.
func TestRWAAssets_ExhaustsTheBucketInLimitOverCostRequests(t *testing.T) {
	ts, reader := newRWALimitedServer(t, 4)
	for i := 1; i <= 2; i++ {
		if resp := mustGet(t, ts.URL+"/v1/rwa/assets"); resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200", i, resp.StatusCode)
		}
	}
	readsBefore := reader.lists.Load()

	resp := mustGet(t, ts.URL+"/v1/rwa/assets")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("request 3: status = %d, want 429", resp.StatusCode)
	}
	if got := resp.Header.Get("X-RateLimit-Limit"); got != "4" {
		t.Fatalf("X-RateLimit-Limit = %q, want 4", got)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "0" {
		t.Fatalf("X-RateLimit-Remaining = %q, want 0", got)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
	if got := reader.lists.Load(); got != readsBefore {
		t.Fatalf("a refused request made %d listing reads, want 0", got-readsBefore)
	}
}
