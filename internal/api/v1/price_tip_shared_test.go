package v1_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// countingPriceReader wraps stubPriceReader and counts LatestPrice
// calls — the proxy for "how many tip computations ran".
type countingPriceReader struct {
	stubPriceReader
	calls atomic.Int32
}

func (r *countingPriceReader) LatestPrice(ctx context.Context, a, q canonical.Asset) (v1.PriceSnapshot, []string, bool, error) {
	r.calls.Add(1)
	return r.stubPriceReader.LatestPrice(ctx, a, q)
}

// TestPriceTipStream_HubSharesOneProducerAcrossConnections is the RT-1
// regression pin (audit 2026-08-04: "tip stream = 6 DB queries/s PER
// CONNECTION"). With a Hub wired, N viewers of the same pair must
// share ONE compute loop: every connection still receives events
// (fan-out works), but total compute calls stay near
// (N pre-flights + ticks), far below the legacy N×ticks shape.
func TestPriceTipStream_HubSharesOneProducerAcrossConnections(t *testing.T) {
	prices := &countingPriceReader{
		stubPriceReader: stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{
				"crypto:BTC/fiat:USD": {Price: "65000", PriceType: "last_trade"},
			},
		},
	}
	hub := streaming.NewHub(0)
	srv := v1.New(v1.Options{Prices: prices, Hub: hub})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	const viewers = 5
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	perConnEvents := make([]int, viewers)
	for i := 0; i < viewers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
				ts.URL+"/v1/price/tip/stream?asset=crypto:BTC&quote=fiat:USD&window_seconds=1", nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("viewer %d GET: %v", i, err)
				return
			}
			defer resp.Body.Close()
			br := bufio.NewReader(resp.Body)
			for perConnEvents[i] < 2 {
				if ev := readTipStreamEvent(t, br, 2500*time.Millisecond); ev == "" {
					return
				}
				perConnEvents[i]++
			}
		}(i)
	}
	wg.Wait()

	for i, n := range perConnEvents {
		if n < 2 {
			t.Errorf("viewer %d received %d events, want >= 2 (hub fan-out must reach every subscriber)", i, n)
		}
	}

	// Compute budget: viewers pre-flights + one shared tick loop.
	// Each viewer holds the stream ~1-2.5s at window_seconds=1, so the
	// shared producer runs ~1 + ceil(elapsed) computes. The legacy
	// per-connection shape would burn viewers×(1+ticks) ≈ 15-20.
	// Note LatestPrice may be called more than once per computeTip for
	// alias fan-in — using a no-alias pair (crypto:BTC) keeps it 1:1.
	calls := int(prices.calls.Load())
	maxWant := viewers + 6
	if calls > maxWant {
		t.Errorf("LatestPrice called %d times for %d same-pair viewers, want <= %d — producer not shared",
			calls, viewers, maxWant)
	}
}
