package main

import (
	"context"
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

// closedBucketReader stands in for the prices_1m read — the one piece
// of this path that needs a live Timescale ([storePriceReader] has no
// seam below the store). Everything the freeze contract depends on is
// the REAL thing here: the marker is written by freeze.Writer in its
// production per-window shape, read by freeze.Looker, and the held value
// is read by redisTriangulatedLooker from the real cachekeys.VWAP key.
type closedBucketReader struct {
	price   string
	sources []string
}

func (c closedBucketReader) LatestPrice(_ context.Context, asset, quote canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	return v1.PriceSnapshot{
		AssetID: asset.String(), Quote: quote.String(),
		Price: c.price, PriceType: "vwap", WindowSeconds: 60,
	}, c.sources, false, nil
}

func (closedBucketReader) RecentClosedSnapshots(context.Context, canonical.Asset, canonical.Asset, int) ([]v1.PriceSnapshot, error) {
	return []v1.PriceSnapshot{}, nil
}

// TestFrozenPairServesHeldValueThroughProductionAdapters pins F013
// (MNY-22) end to end through the adapters this binary wires: with the
// aggregator's freeze marker live for XLM/GBP and its 5m value held in
// the VWAP cache, /v1/price must serve the HELD value, not the newer
// prices_1m bucket the freeze refused — and must go back to the closed
// bucket, byte for byte, the moment the freeze is released.
//
// Against the un-fixed handler the first request returns
// `"price":"0.3199"` with `"frozen":true`: the rejected bucket under a
// flag that promises the protected one.
func TestFrozenPairServesHeldValueThroughProductionAdapters(t *testing.T) {
	const held, moved = "0.251100000000", "0.3199"

	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	ctx := context.Background()

	xlm, err := canonical.ParseAsset("crypto:XLM")
	if err != nil {
		t.Fatalf("parse crypto:XLM: %v", err)
	}
	gbp, err := canonical.NewFiatAsset("GBP")
	if err != nil {
		t.Fatalf("build fiat:GBP: %v", err)
	}

	// The aggregator's side of a freeze, as it leaves Redis: the 5m
	// window's last publish still in the VWAP cache, and the pair's
	// marker carrying that window's ladder.
	const window = 5 * time.Minute
	if err := rdb.Set(ctx, cachekeys.VWAP(xlm, gbp, window).String(), held, 35*time.Minute).Err(); err != nil {
		t.Fatalf("seed held VWAP: %v", err)
	}
	writer, err := freeze.NewWriter(rdb, cachekeys.FreezeTTL)
	if err != nil {
		t.Fatalf("freeze writer: %v", err)
	}
	now := time.Now().UTC()
	state := freeze.State{FiredAt: now, HoldUntil: now.Add(30 * time.Minute)}
	decision := anomaly.Decision{Action: anomaly.ActionFreeze, Reason: "test: refused bucket"}
	if err := writer.MarkHoldForWindow(ctx, xlm, gbp, window, held, decision, state, 35*time.Minute); err != nil {
		t.Fatalf("mark freeze: %v", err)
	}

	looker, err := freeze.NewLooker(rdb)
	if err != nil {
		t.Fatalf("freeze looker: %v", err)
	}
	srv := v1.New(v1.Options{
		Prices:       closedBucketReader{price: moved, sources: []string{"kraken", "coinbase"}},
		Freeze:       looker,
		Triangulated: redisTriangulatedLooker{rdb: rdb},
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	get := func() (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/v1/price?asset=crypto:XLM&quote=fiat:GBP", nil)
		if err != nil {
			t.Fatalf("build request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /v1/price: %v", err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		return resp.StatusCode, string(body)
	}

	status, body := get()
	if status != http.StatusOK {
		t.Fatalf("frozen: status = %d, want 200: %s", status, body)
	}
	if strings.Contains(body, moved) {
		t.Fatalf("frozen pair served the refused prices_1m bucket %s: %s", moved, body)
	}
	for _, want := range []string{`"price":"` + held + `"`, `"window_seconds":300`, `"frozen":true`, `"stale":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("frozen: body missing %s: %s", want, body)
		}
	}

	// Release: the marker goes, and the closed bucket is the answer again.
	if err := writer.Clear(ctx, xlm, gbp); err != nil {
		t.Fatalf("clear freeze: %v", err)
	}
	status, body = get()
	if status != http.StatusOK {
		t.Fatalf("released: status = %d, want 200: %s", status, body)
	}
	for _, want := range []string{`"price":"` + moved + `"`, `"window_seconds":60`, `"stale":false`, `"sources":["kraken","coinbase"]`} {
		if !strings.Contains(body, want) {
			t.Errorf("released: body missing %s: %s", want, body)
		}
	}
	if strings.Contains(body, `"frozen":true`) {
		t.Errorf("released pair still flagged frozen: %s", body)
	}
}
