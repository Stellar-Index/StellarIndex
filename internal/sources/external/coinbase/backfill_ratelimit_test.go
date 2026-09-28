package coinbase

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// scriptedCoinbaseREST answers call i with responder(i) and records the
// time of every request.
func scriptedCoinbaseREST(t *testing.T, responder func(call int, w http.ResponseWriter)) (*httptest.Server, func() []time.Time) {
	t.Helper()
	var mu sync.Mutex
	var seen []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		call := len(seen)
		seen = append(seen, time.Now())
		mu.Unlock()
		responder(call, w)
	}))
	return srv, func() []time.Time {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Time(nil), seen...)
	}
}

func coinbaseXLMUSD(t *testing.T) canonical.Pair {
	t.Helper()
	xlm, _ := canonical.NewCryptoAsset("XLM")
	usd, _ := canonical.NewFiatAsset("USD")
	p, err := canonical.NewPair(xlm, usd)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return p
}

// One 429 must not discard the walk: the window is retried.
func TestCoinbaseBackfill_RetriesRateLimitedWindow(t *testing.T) {
	const startSec = int64(1_745_002_800)
	candles := synthesiseCoinbaseCandles(5, startSec, 3600)
	srv, _ := scriptedCoinbaseREST(t, func(call int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		switch call {
		case 0:
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"Slow down"}`))
		case 1:
			_ = json.NewEncoder(w).Encode(candles)
		default:
			_ = json.NewEncoder(w).Encode([]coinbaseCandle{})
		}
	})
	defer srv.Close()

	m, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	s := NewStreamer(m)
	s.Endpoint = srv.URL
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	from := time.Unix(startSec, 0).UTC()
	trades, err := s.Backfill(ctx, coinbaseXLMUSD(t), from, from.Add(6*time.Hour), time.Hour)
	if err != nil {
		t.Fatalf("Backfill after one 429: %v", err)
	}
	if len(trades) != 5 {
		t.Fatalf("got %d trades, want 5", len(trades))
	}
}

// Requests are paced, not fired back-to-back into the venue's limit.
func TestCoinbaseBackfill_PacesRequests(t *testing.T) {
	const startSec = int64(1_745_002_800)
	srv, seen := scriptedCoinbaseREST(t, func(_ int, w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]coinbaseCandle{})
	})
	defer srv.Close()

	m, err := DefaultPairs()
	if err != nil {
		t.Fatalf("DefaultPairs: %v", err)
	}
	s := NewStreamer(m)
	s.Endpoint = srv.URL
	from := time.Unix(startSec, 0).UTC()
	// 1500 hourly candles = five 300-candle windows.
	if _, err := s.Backfill(context.Background(), coinbaseXLMUSD(t), from, from.Add(1500*time.Hour), time.Hour); err != nil {
		t.Fatalf("Backfill: %v", err)
	}
	times := seen()
	if len(times) != 5 {
		t.Fatalf("got %d requests, want 5", len(times))
	}
	span := times[len(times)-1].Sub(times[0])
	if minSpan := 4 * candlesRequestInterval * 9 / 10; span < minSpan {
		t.Fatalf("5 requests spanned %v, want >= %v (unpaced)", span, minSpan)
	}
}
