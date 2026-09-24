package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

// priceMissReader is a prices_1m with no row for the pair, so /v1/price
// falls through to the aggregator's VWAP cache.
type priceMissReader struct{}

func (priceMissReader) LatestPrice(context.Context, canonical.Asset, canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	return v1.PriceSnapshot{}, nil, false, v1.ErrPriceNotFound
}

func (priceMissReader) RecentClosedSnapshots(context.Context, canonical.Asset, canonical.Asset, int) ([]v1.PriceSnapshot, error) {
	return []v1.PriceSnapshot{}, nil
}

// TestVWAPCacheServesTheValuesObservationTime pins RLT-357 through the
// production adapter: every /v1/price surface that serves the
// aggregator's VWAP cache must stamp observed_at with when the aggregator
// observed the value, not when the API read it. A freeze keeps the held
// value alive for its whole hold without rewriting it, so "the key
// exists" says nothing about age.
//
// The stamp key is spelled out on the wire (not via its builder): the
// aggregator and the API are separate binaries, so the key is the
// contract. Against the unfixed API every case below serves observed_at
// = read time, twenty minutes after the value was observed.
func TestVWAPCacheServesTheValuesObservationTime(t *testing.T) {
	const window = 5 * time.Minute
	const held = "0.251100000000"
	observed := time.Now().UTC().Add(-20 * time.Minute).Truncate(time.Minute)

	xlm, err := canonical.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatalf("parse crypto:XLM: %v", err)
	}
	gbp, err := canonical.NewFiatAsset("GBP")
	if err != nil {
		t.Fatalf("build fiat:GBP: %v", err)
	}

	for _, tc := range []struct {
		name   string
		frozen bool
		query  string
	}{
		{"fallback", false, "/v1/price?asset=crypto:XLM&quote=fiat:GBP"},
		{"windowed", false, "/v1/price?asset=crypto:XLM&quote=fiat:GBP&window=300"},
		{"frozen held value", true, "/v1/price?asset=crypto:XLM&quote=fiat:GBP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			mr := miniredis.RunT(t)
			rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
			t.Cleanup(func() { _ = rdb.Close() })

			// The aggregator's side, as a freeze leaves it 20 minutes in:
			// value and stamp kept alive, neither rewritten.
			valKey := cachekeys.VWAP(xlm, gbp, window).String()
			if err := rdb.Set(ctx, valKey, held, 35*time.Minute).Err(); err != nil {
				t.Fatalf("seed VWAP: %v", err)
			}
			if err := rdb.Set(ctx, valKey+":observed_at", observed.Format(time.RFC3339Nano), 35*time.Minute).Err(); err != nil {
				t.Fatalf("seed observed_at: %v", err)
			}

			opts := v1.Options{Prices: priceMissReader{}, Triangulated: redisTriangulatedLooker{rdb: rdb}}
			if tc.frozen {
				opts.Freeze = markFrozen(ctx, t, rdb, xlm, gbp, window, held)
			}
			ts := httptest.NewServer(v1.New(opts).Handler())
			t.Cleanup(ts.Close)

			status, got := getObservedAt(ctx, t, ts.URL+tc.query)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200", status)
			}
			if !got.Equal(observed) {
				t.Errorf("observed_at = %s, want %s — when the aggregator observed the value, "+
					"not when the API read it (%s later)", got, observed, got.Sub(observed).Round(time.Second))
			}
		})
	}
}

func markFrozen(
	ctx context.Context, t *testing.T, rdb *redis.Client, base, quote canonical.Asset, window time.Duration, held string,
) *freeze.Looker {
	t.Helper()
	writer, err := freeze.NewWriter(rdb, cachekeys.FreezeTTL)
	if err != nil {
		t.Fatalf("freeze writer: %v", err)
	}
	now := time.Now().UTC()
	state := freeze.State{FiredAt: now.Add(-20 * time.Minute), HoldUntil: now.Add(10 * time.Minute)}
	decision := anomaly.Decision{Action: anomaly.ActionFreeze, Reason: "test: refused bucket"}
	if err := writer.MarkHoldForWindow(ctx, base, quote, window, held, decision, state, 15*time.Minute); err != nil {
		t.Fatalf("mark freeze: %v", err)
	}
	looker, err := freeze.NewLooker(rdb)
	if err != nil {
		t.Fatalf("freeze looker: %v", err)
	}
	return looker
}

func getObservedAt(ctx context.Context, t *testing.T, url string) (int, time.Time) {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var env struct {
		Data struct {
			ObservedAt time.Time `json:"observed_at"`
		} `json:"data"`
	}
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(body, &env); err != nil {
			t.Fatalf("decode %s: %v", body, err)
		}
	}
	return resp.StatusCode, env.Data.ObservedAt
}
