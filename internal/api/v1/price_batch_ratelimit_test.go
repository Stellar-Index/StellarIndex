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

	// countingPriceReader (price_tip_shared_test.go) counts LatestPrice
	// calls, which lets a test assert the one thing a rate-limit denial
	// exists to guarantee: that the work was NOT done. A 429 written
	// after the fan-out would be a status code and nothing else.
	reader := &countingPriceReader{}
	anon := ratelimit.New(rdb, anonLimit, time.Minute)
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

// TestPriceBatch_ChargesOneTokenPerID is the F035 / F046 regression.
// One rate-limit token used to buy a whole batch: a 40-id GET left 99
// of 100 tokens, so the per-minute ceiling bounded HTTP requests while
// the work behind them was the caller's to choose. A batch must cost
// its id count.
func TestPriceBatch_ChargesOneTokenPerID(t *testing.T) {
	ts, reader := newBatchLimitedServer(t, 100)
	url := ts.URL + "/v1/price/batch?asset_ids=" + strings.Join(batchIDs(40), ",")

	for i, wantRemaining := range []string{"60", "20"} {
		resp := mustGet(t, url)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("batch %d: status = %d, want 200", i+1, resp.StatusCode)
		}
		if got := resp.Header.Get("X-RateLimit-Remaining"); got != wantRemaining {
			t.Fatalf("batch %d: X-RateLimit-Remaining = %q, want %q (40 ids must cost 40 tokens)",
				i+1, got, wantRemaining)
		}
	}

	// 80 spent; a third 40-id batch does not fit in the remaining 20.
	served := reader.calls.Load()
	resp := mustGet(t, url)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third batch: status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("429 must carry Retry-After")
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("429 Content-Type = %q, want application/problem+json", ct)
	}
	if got := reader.calls.Load(); got != served {
		t.Fatalf("a denied batch still resolved %d price(s); the charge must land BEFORE the fan-out", got-served)
	}
}

// TestPriceBatch_POSTChargesPerID pins the variant the findings name:
// the JSON-body route whose 1000-id ceiling is what made one token
// worth a thousand resolutions. Against the deployed 6000/min anonymous
// budget a full batch must leave 5000, not 5999.
func TestPriceBatch_POSTChargesPerID(t *testing.T) {
	ts, _ := newBatchLimitedServer(t, 6000)

	resp := postBatch(t, ts.URL, batchIDs(1000))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "5000" {
		t.Fatalf("X-RateLimit-Remaining after a 1000-id POST = %q, want 5000", got)
	}
}

// TestPriceBatch_OverCeilingBatchSpendsTheWindow pins the decision for
// a batch priced above the caller's whole budget (1000 ids against the
// 60/min default). It is served into an untouched window — refusing it
// in every window would make the documented 1000-id ceiling unusable
// behind a Retry-After that never comes true — and it takes the entire
// window with it, so the next request of any size is denied.
func TestPriceBatch_OverCeilingBatchSpendsTheWindow(t *testing.T) {
	ts, _ := newBatchLimitedServer(t, 60)

	resp := postBatch(t, ts.URL, batchIDs(1000))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first batch in a fresh window: status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "0" {
		t.Fatalf("X-RateLimit-Remaining = %q, want 0 (the batch must spend the whole window)", got)
	}

	resp = mustGet(t, ts.URL+"/v1/price/batch?asset_ids=fiat:EUR")
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("request after an over-ceiling batch: status = %d, want 429", resp.StatusCode)
	}
}

// TestPriceBatch_ChargeFollowsTheWorkDone pins what the cost is a count
// OF: the de-duplicated ids the handler will resolve. Forty copies of
// one id are one resolution and cost one token; a request rejected as
// malformed does no resolution and costs only the base token every
// request pays.
func TestPriceBatch_ChargeFollowsTheWorkDone(t *testing.T) {
	ts, reader := newBatchLimitedServer(t, 100)

	dupes := strings.TrimSuffix(strings.Repeat("fiat:EUR,", 40), ",")
	resp := mustGet(t, ts.URL+"/v1/price/batch?asset_ids="+dupes)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "99" {
		t.Fatalf("40 duplicates of one id: X-RateLimit-Remaining = %q, want 99", got)
	}

	// 101 ids on the GET route is a 400 (ceiling 100): base token only.
	before := reader.calls.Load()
	resp = mustGet(t, ts.URL+"/v1/price/batch?asset_ids="+strings.Join(batchIDs(101), ","))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "98" {
		t.Fatalf("a rejected batch: X-RateLimit-Remaining = %q, want 98 (base token only)", got)
	}
	if got := reader.calls.Load(); got != before {
		t.Fatalf("a 400 resolved %d price(s)", got-before)
	}
}

// TestPriceBatch_UnlimitedDeploymentIsUncharged: with no limiter wired
// there is no account to charge, and the batch is served as before.
func TestPriceBatch_UnlimitedDeploymentIsUncharged(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{}})
	ts := startHTTPTest(t, srv.Handler())

	resp := postBatch(t, ts.URL, batchIDs(1000))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("X-RateLimit-Remaining"); got != "" {
		t.Fatalf("X-RateLimit-Remaining = %q on a deployment with no limiter", got)
	}
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
