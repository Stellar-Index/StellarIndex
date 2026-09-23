package v1_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/api/v1/middleware"
	"github.com/Stellar-Index/StellarIndex/internal/auth"
	"github.com/Stellar-Index/StellarIndex/internal/usage"
)

// TestPriceBatch_MetersOneUsageUnitPerID pins GH-1275: the monthly meter
// counted HTTP requests, so a 1000-id POST /v1/price/batch cost one unit
// of a quota sold in price lookups. Each batch must advance BOTH the
// billable total MonthlyQuota reads and the per-endpoint detail counter
// by its de-duplicated id count, and a single-price call stays at one.
func TestPriceBatch_MetersOneUsageUnitPerID(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	counter := usage.New(rdb, usage.WithClock(func() time.Time { return now }))

	subject := auth.Subject{
		Identifier: auth.AccountIdentifier("acme"),
		KeyID:      "kid_units",
		Tier:       auth.TierAPIKey,
	}
	attach := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithSubject(r.Context(), subject)))
		})
	}
	srv := v1.New(v1.Options{
		Prices:       &countingPriceReader{},
		Auth:         attach,
		UsageTracker: middleware.UsageTracker(counter, nil),
	})
	ts := startHTTPTest(t, srv.Handler())

	post := postBatch(t, ts.URL, batchIDs(250))
	_ = post.Body.Close()
	if post.StatusCode != http.StatusOK {
		t.Fatalf("POST batch status = %d, want 200", post.StatusCode)
	}
	// 40 distinct ids plus a duplicate: the unit count is the work done.
	ids := batchIDs(40)
	ids = append(ids, batchIDs(1)...)
	get := mustGet(t, ts.URL+"/v1/price/batch?asset_ids="+strings.Join(ids, ","))
	_ = get.Body.Close()
	if get.StatusCode != http.StatusOK {
		t.Fatalf("GET batch status = %d, want 200", get.StatusCode)
	}

	key := middleware.UsageKeyForSubject(subject)
	got, err := counter.MonthToDate(context.Background(), key)
	if err != nil {
		t.Fatalf("MonthToDate: %v", err)
	}
	if got != 290 {
		t.Fatalf("billable month-to-date = %d, want 290 (250 + 40 price lookups)", got)
	}

	rows, err := counter.ScanDetail(context.Background(), []string{"2026-09-15"})
	if err != nil {
		t.Fatalf("ScanDetail: %v", err)
	}
	var detail int64
	for _, r := range rows {
		if r.Subject == key && r.Endpoint == "/v1/price/batch" && r.Class == usage.ClassOK {
			detail += r.Count
		}
	}
	if detail != 290 {
		t.Fatalf("detail ok units for /v1/price/batch = %d, want 290 — the rollup's billable sum must equal the quota counter", detail)
	}
}
