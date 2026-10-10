package v1_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/ratelimit"
)

// batchIssuer is a well-formed (CRC-valid) issuer strkey. The ids built
// on it name no real market; the stub reader answers not-found for all
// of them, which the batch contract serves as an omitted row.
const batchIssuer = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"

// batchIDs returns n distinct, well-formed classic asset ids
// (T0000-G..., T0001-G...), so the handler's de-duplication cannot
// collapse them and the cost under test is exactly n.
func batchIDs(n int) []string {
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, fmt.Sprintf("T%04d-%s", i, batchIssuer))
	}
	return ids
}

// batchProbe is over the GET ceiling, so it is a no-store 400 that costs
// only the base token (TestPriceBatch_ChargeFollowsTheWorkDone pins it).
var batchProbe = "/v1/price/batch?asset_ids=" + strings.Join(batchIDs(101), ",")

// newBatchLimitedServer wires the price-batch routes behind the
// PRODUCTION limiter constructor — middleware.RateLimitBySubject, the
// one cmd/stellarindex-api builds — over a Redis-backed anonymous
// bucket of the given size. No auth middleware is mounted, so every
// request is the anonymous caller the findings are about.
func newBatchLimitedServer(t *testing.T, anonLimit int) (*testServerImpl, *countingPriceReader) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	// countingPriceReader (price_tip_stream_admit_internal_test.go) counts LatestPrice
	// calls, which lets a test assert the one thing a rate-limit denial
	// exists to guarantee: that the work was NOT done. A 429 written
	// after the fan-out would be a status code and nothing else.
	reader := &countingPriceReader{}
	anon := ratelimit.New(rdb, anonLimit, time.Minute, pinnedWindow)
	srv := v1.New(v1.Options{
		Prices:    reader,
		RateLimit: middleware.RateLimitBySubject(anon, nil, middleware.SkipHealthAndMetrics, nil),
	})
	return startHTTPTest(t, srv.Handler()), reader
}

func postBatch(t *testing.T, url string, ids []string) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{"asset_ids": ids})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return mustPostJSON(t, url+"/v1/price/batch", string(body))
}

// Guards the fixture itself: the ids must stay distinct at the sizes
// the tests above rely on.
func TestBatchIDs_AreDistinct(t *testing.T) {
	seen := map[string]struct{}{}
	for _, id := range batchIDs(1000) {
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate fixture id %s", id)
		}
		seen[id] = struct{}{}
	}
	if len(seen) != 1000 {
		t.Fatalf("distinct ids = %d, want 1000", len(seen))
	}
}
