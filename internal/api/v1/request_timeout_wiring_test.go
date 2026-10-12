package v1_test

import (
	"context"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// deadlineCapturingHistoryReader records the context deadline TradesInRange sees, proving the request-scoped deadline
// (RequestTimeout middleware + per-handler 8s wrap) reaches the DB read seam.
type deadlineCapturingHistoryReader struct {
	stubHistoryReader
	sawDeadline bool
	remaining   time.Duration
	trade       canonical.Trade
}

func (r *deadlineCapturingHistoryReader) TradesInRange(ctx context.Context, _ canonical.Pair, _, _ time.Time, _ int) ([]canonical.Trade, error) {
	if dl, ok := ctx.Deadline(); ok {
		r.sawDeadline = true
		r.remaining = time.Until(dl)
	}
	return []canonical.Trade{r.trade}, nil
}

func mkNativeUSDTrade() canonical.Trade {
	xlm, _ := canonical.ParseAsset("native")
	usd, _ := canonical.ParseAsset("fiat:USD")
	pair, _ := canonical.NewPair(xlm, usd)
	return canonical.Trade{
		Source: "soroswap", Ledger: 1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		Timestamp:   time.Unix(1_772_000_000, 0).UTC(),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(100)),
		QuoteAmount: canonical.NewAmount(big.NewInt(200)),
	}
}

// TestRequestTimeout_BoundsReachDBSeam: a request through the real Server chain delivers a
// BOUNDED context to the trades read. With RequestTimeout set below the per-handler 8s wrap, the
// middleware's deadline is what the read observes, proving it is wired, not just constructible.
func TestRequestTimeout_BoundsReachDBSeam(t *testing.T) {
	reader := &deadlineCapturingHistoryReader{trade: mkNativeUSDTrade()}
	srv := v1.New(v1.Options{History: reader, RequestTimeout: 2 * time.Second})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !reader.sawDeadline {
		t.Fatal("TradesInRange saw NO deadline — the request context reached the DB read unbounded")
	}
	// The middleware bound (2s) is tighter than the per-handler 8s wrap,
	// so the read must observe <= ~2s.
	if reader.remaining <= 0 || reader.remaining > 2*time.Second {
		t.Errorf("deadline remaining = %v, want in (0, 2s] (the middleware bound)", reader.remaining)
	}
}

// TestRequestTimeout_DefaultAlwaysOn: a Server without an explicit RequestTimeout still bounds reads (the New() default).
// It hits /v1/price/tip, not /v1/vwap: handlePriceTip passes r.Context() straight to TradesInRange, whereas vwap's own 8s
// wrap would pass even if defaultRequestTimeout were never installed.
func TestRequestTimeout_DefaultAlwaysOn(t *testing.T) {
	reader := &deadlineCapturingHistoryReader{trade: mkNativeUSDTrade()}
	srv := v1.New(v1.Options{History: reader, Prices: &stubPriceReader{}}) // no RequestTimeout set
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote=fiat:USD&window_seconds=5")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if !reader.sawDeadline {
		t.Fatal("default Server delivered an unbounded context to the DB read")
	}
	if reader.remaining <= 0 || reader.remaining > 15*time.Second {
		t.Errorf("deadline remaining = %v, want in (0, 15s]", reader.remaining)
	}
}

// TestRequestTimeout_GlobalDeadlineWritesProblem: when the middleware's own deadline fires, clientAborted (ctx.Err != nil)
// reads it as a disconnect and the handler returns silently, emitting a BODYLESS 200 that a dashboard renders as "$0 TVL". The
// deadline is a SERVER-side budget, so the still-connected client is owed an RFC 9457 problem document to retry on.
func TestRequestTimeout_GlobalDeadlineWritesProblem(t *testing.T) {
	pool := mkCStrkey(t, 7)
	srv := v1.New(v1.Options{
		Explorer: &stubExplorerReader{},
		Lending:  blockingLendingReader{},
		// Below every per-handler budget, so the middleware's deadline
		// is the one the reader observes.
		RequestTimeout: 150 * time.Millisecond,
	})
	base := httpTestServer(t, srv).URL

	resp := mustGet(t, base+"/v1/lending/pools/"+pool+"/reserves")
	body, _ := readAll(resp)
	if resp.StatusCode == http.StatusOK {
		t.Fatalf("status = 200 with body %q — a request-deadline expiry must never look like success", body)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (a request deadline is retryable capacity, not an internal fault)", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/problem+json" {
		t.Errorf("Content-Type = %q, want application/problem+json", ct)
	}
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store (a transient timeout must not be cached and replayed)", cc)
	}
	if !strings.Contains(body, "lending-timeout") {
		t.Errorf("expected `lending-timeout` problem type in body, got: %s", body)
	}
}

// blockingLendingReader stalls until the caller's context is done, then
// returns that context's error — the shape a cold contract_data scan
// takes when the request deadline beats it.
type blockingLendingReader struct{}

func (blockingLendingReader) ListBlendPools(ctx context.Context) ([]timescale.BlendPoolSummary, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (blockingLendingReader) BlendPoolAssets(ctx context.Context, _ string) ([]string, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (blockingLendingReader) BlendPoolVersion(ctx context.Context, _ string) (blend.PoolVersion, error) {
	<-ctx.Done()
	return blend.PoolVersionUnknown, ctx.Err()
}

func (blockingLendingReader) BlendReserveConfigs(ctx context.Context, _ string) (map[string]blend.ReserveConfig, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
