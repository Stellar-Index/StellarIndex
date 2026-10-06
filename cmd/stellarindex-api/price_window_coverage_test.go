package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate/anomaly"
	"github.com/Stellar-Index/StellarIndex/internal/aggregate/freeze"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/cachekeys"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestPriceServesCachedWindowCoverage: /v1/price serving the aggregator's
// rolling-window cache (here a frozen pair's held 24h value) states the
// coverage the aggregator wrote beside the value: truncated plus
// covered_from for a capped read, truncated=false for a complete one,
// and neither field when no coverage was written (unknown).
func TestPriceServesCachedWindowCoverage(t *testing.T) {
	const held = "0.251100000000"
	const window = 24 * time.Hour
	observed := time.Now().Add(-10 * time.Minute).Truncate(time.Minute).UTC()
	coveredFrom := observed.Add(-90 * time.Minute)
	wireFrom, err := json.Marshal(v1.WireTime(coveredFrom))
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		cov     *cachekeys.WindowCoverage
		want    []string
		without []string
	}{
		{
			name:    "truncated",
			cov:     &cachekeys.WindowCoverage{Truncated: true, CoveredFrom: coveredFrom},
			want:    []string{`"truncated":true`, `"covered_from":` + string(wireFrom)},
			without: nil,
		},
		{
			name:    "complete",
			cov:     &cachekeys.WindowCoverage{},
			want:    []string{`"truncated":false`},
			without: []string{`"covered_from"`},
		},
		{
			name:    "unknown",
			cov:     nil,
			without: []string{`"truncated"`, `"covered_from"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := frozenHeldPriceBody(t, held, window, observed, tc.cov)
			if !strings.Contains(body, `"price":"`+held+`"`) || !strings.Contains(body, `"window_seconds":86400`) {
				t.Fatalf("did not serve the held 24h value: %s", body)
			}
			for _, w := range tc.want {
				if !strings.Contains(body, w) {
					t.Errorf("body missing %s: %s", w, body)
				}
			}
			for _, w := range tc.without {
				if strings.Contains(body, w) {
					t.Errorf("body carries %s: %s", w, body)
				}
			}
		})
	}
}

// frozenHeldPriceBody seeds XLM/GBP's held value for window as the
// aggregator leaves it under a freeze, with cov beside it when non-nil,
// and returns the /v1/price body.
func frozenHeldPriceBody(t *testing.T, held string, window time.Duration, observed time.Time, cov *cachekeys.WindowCoverage) string {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()
	xlm, err := canonical.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatal(err)
	}
	gbp, err := canonical.NewFiatAsset("GBP")
	if err != nil {
		t.Fatal(err)
	}
	const ttl = 35 * time.Minute
	seed := map[string]string{
		cachekeys.VWAP(xlm, gbp, window).String():           held,
		cachekeys.VWAPObservedAt(xlm, gbp, window).String(): cachekeys.FormatVWAPObservedAt(observed),
	}
	if cov != nil {
		seed[cachekeys.VWAPCoverage(xlm, gbp, window).String()] = cachekeys.FormatVWAPCoverage(*cov)
	}
	for k, v := range seed {
		if err := rdb.Set(ctx, k, v, ttl).Err(); err != nil {
			t.Fatalf("seed %s: %v", k, err)
		}
	}
	writer, err := freeze.NewWriter(rdb, cachekeys.FreezeTTL)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	state := freeze.State{FiredAt: now, HoldUntil: now.Add(30 * time.Minute)}
	decision := anomaly.Decision{Action: anomaly.ActionFreeze, Reason: "test: refused bucket"}
	if err := writer.MarkHoldForWindow(ctx, xlm, gbp, window, held, decision, state, ttl); err != nil {
		t.Fatalf("mark freeze: %v", err)
	}
	looker, err := freeze.NewLooker(rdb)
	if err != nil {
		t.Fatal(err)
	}
	srv := v1.New(v1.Options{
		Prices:       closedBucketReader{price: "0.3199", sources: []string{"kraken", "coinbase"}},
		Freeze:       looker,
		Triangulated: redisTriangulatedLooker{rdb: rdb},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/price?asset=crypto:XLM&quote=fiat:GBP", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /v1/price: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	return string(body)
}
