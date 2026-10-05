package v1

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

type countingPriceReader struct {
	noPriceReader
	calls atomic.Int64
}

func (c *countingPriceReader) LatestPrice(ctx context.Context, a, q canonical.Asset) (PriceSnapshot, []string, bool, error) {
	c.calls.Add(1)
	return c.noPriceReader.LatestPrice(ctx, a, q)
}

// A stream refused at the producer ceiling must cost no price reads.
func TestPriceTipStream_ProducerCeilingRefusalRunsNoComputeTip(t *testing.T) {
	prices := &countingPriceReader{}
	s := New(Options{
		Prices: prices,
		Hub:    streaming.NewHub(0),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	s.tipProducers.maxProducers = 1
	held, outcome := s.tipProducers.acquireFor(
		tipProducerKey{asset: "native", quote: "fiat:EUR", window: 5}, "other", nil,
		func(ctx context.Context) { <-ctx.Done() })
	if outcome != tipProducerAdmitted {
		t.Fatalf("setup: %s", outcome)
	}
	defer held()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/price/tip/stream?asset=native&quote=fiat:USD&window_seconds=1", nil)
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if n := prices.calls.Load(); n != 0 {
		t.Fatalf("computeTip ran %d price reads before the producer refusal", n)
	}
}
