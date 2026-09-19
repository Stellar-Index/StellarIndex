package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/forex"
)

// fxUpstream fakes the two massive REST endpoints the forex worker reads.
// `current` answers today's grouped-daily bar (what the worker treats as
// the live rate); `dated` answers every earlier date (the trailing-7d
// series). todayHits counts live-rate fetches so the test can wait for
// whole refresh cycles instead of sleeping.
type fxUpstream struct {
	mu        sync.Mutex
	current   map[string]float64
	dated     map[string]float64
	todayHits int
}

func (u *fxUpstream) set(rates map[string]float64) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.current = rates
	return u.todayHits
}

func (u *fxUpstream) hits() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.todayHits
}

func (u *fxUpstream) handler(w http.ResponseWriter, r *http.Request) {
	const groupedPrefix = "/v2/aggs/grouped/locale/global/market/fx/"
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case strings.HasPrefix(r.URL.Path, groupedPrefix):
		rates := u.dated
		if strings.TrimPrefix(r.URL.Path, groupedPrefix) == time.Now().UTC().Format("2006-01-02") {
			rates = u.current
			u.todayHits++
		}
		rows := make([]map[string]any, 0, len(rates))
		for code, rate := range rates {
			rows = append(rows, map[string]any{"T": "C:USD" + code, "c": rate})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"resultsCount": len(rows), "results": rows})
	case r.URL.Path == "/v3/reference/tickers":
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]string{
			{
				"ticker": "C:USDUZS", "base_currency_symbol": "USD", "base_currency_name": "United States Dollar",
				"currency_symbol": "UZS", "currency_name": "Uzbekistan Som",
			},
			{
				"ticker": "C:USDEUR", "base_currency_symbol": "USD", "base_currency_name": "United States Dollar",
				"currency_symbol": "EUR", "currency_name": "Euro",
			},
		}})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// xlmUSDOnlyReader prices native/fiat:USD and nothing else, so any
// fiat:UZS answer the handler gives was DERIVED through the forex
// snapshot — the path under test.
type xlmUSDOnlyReader struct{}

func (xlmUSDOnlyReader) LatestPrice(_ context.Context, a, q canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	if a.String() == "native" && q.String() == "fiat:USD" {
		return v1.PriceSnapshot{
			AssetID: a.String(), Quote: q.String(), Price: "0.25", PriceType: "vwap",
		}, []string{"sdex"}, false, nil
	}
	return v1.PriceSnapshot{}, nil, false, v1.ErrPriceNotFound
}

func (xlmUSDOnlyReader) RecentClosedSnapshots(context.Context, canonical.Asset, canonical.Asset, int) ([]v1.PriceSnapshot, error) {
	return []v1.PriceSnapshot{}, nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func getBody(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url) //nolint:gosec,noctx // httptest URL
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestPriceFiatCrossNeverServesABandRefusedRate pins F004 / F026 / K032
// end to end through everything this binary wires on the fiat path: the
// real forex.Worker running its production Run loop against a fake
// upstream, the forex.Cache it installs into, newForexAdapter, and the
// production /v1/price handler.
//
// It replays the 2026-08-24 Massive UZS incident. After one healthy
// refresh (UZS = 11,800) the live bar turns into 1820 and stays there,
// while the ticker's dated bars keep saying ~11,790. The C2-030 band
// refuses 1820 on every refresh (deviation, then the history-majority
// confirm veto). Un-fixed, the worker installed the RAW snapshot before
// the band ran, so /v1/price served XLM/UZS = 0.25 × 1820 = 455 — 6.5×
// too low — for as long as the upstream stayed broken, while fx_quotes
// correctly refused the same bar.
func TestPriceFiatCrossNeverServesABandRefusedRate(t *testing.T) {
	up := &fxUpstream{
		current: map[string]float64{"UZS": 11800, "EUR": 0.92},
		dated:   map[string]float64{"UZS": 11790, "EUR": 0.92},
	}
	massive := httptest.NewServer(http.HandlerFunc(up.handler))
	t.Cleanup(massive.Close)

	cache := forex.NewCache()
	worker := forex.NewWorker(
		forex.NewClient("test").WithBase(massive.URL),
		cache,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		5*time.Millisecond,
	)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = worker.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	srv := v1.New(v1.Options{Prices: xlmUSDOnlyReader{}, Currencies: newForexAdapter(cache)})
	api := httptest.NewServer(srv.Handler())
	t.Cleanup(api.Close)

	waitFor(t, "the first healthy snapshot", func() bool { return cache.Latest() != nil })
	status, body := getBody(t, api.URL+"/v1/price?asset=native&quote=fiat:UZS")
	if status != http.StatusOK || !strings.Contains(body, `"price":"2950"`) {
		t.Fatalf("healthy: status %d, want XLM/UZS = 0.25 x 11800 = 2950. Body: %s", status, body)
	}

	// The live bar breaks. Wait for four MORE live-rate fetches: the
	// worker's loop is sequential, so by then at least three complete
	// refreshes have scored 1820 — the deviation refusal AND the vetoed
	// confirm that follows it.
	flippedAt := up.set(map[string]float64{"UZS": 1820, "EUR": 0.92})
	waitFor(t, "three refreshes of the broken bar", func() bool { return up.hits() >= flippedAt+4 })

	status, body = getBody(t, api.URL+"/v1/price?asset=native&quote=fiat:UZS")
	if strings.Contains(body, `"price":"455"`) {
		t.Fatalf("/v1/price served XLM/UZS = 455 = 0.25 x 1820: the rate the sanity "+
			"band REFUSED reached a customer through the in-memory snapshot. Body: %s", body)
	}
	if status != http.StatusOK || !strings.Contains(body, `"price":"2950"`) {
		t.Fatalf("status %d, want the last GUARDED rate still served (2950). Body: %s", status, body)
	}

	// Same snapshot, the fiat-vs-fiat path: 1/11800 = 0.0000847…,
	// against 1/1820 = 0.000549….
	status, body = getBody(t, api.URL+"/v1/price?asset=fiat:UZS&quote=fiat:USD")
	if status != http.StatusOK || !strings.Contains(body, `"price":"0.0000847`) {
		t.Fatalf("fiat cross: status %d, want UZS/USD = 1/11800 (0.0000847…), never "+
			"1/1820 (0.000549…). Body: %s", status, body)
	}
}
