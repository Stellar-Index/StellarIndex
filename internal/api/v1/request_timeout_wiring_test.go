package v1_test

import (
	"context"
	"math/big"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// deadlineCapturingHistoryReader records the context deadline its
// TradesInRange sees, so a test can prove the request-scoped deadline
// (RequestTimeout middleware + the per-handler 8s wrap) actually reaches
// the DB read seam.
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

// TestRequestTimeout_BoundsReachDBSeam proves the durable chokepoint fix
// end-to-end (C3-1/C3-2/P1, audit-2026-07-16): a request through the real
// Server chain delivers a BOUNDED context to the trades read. With
// RequestTimeout set below the per-handler 8s wrap, the middleware's
// deadline is what the read observes — proving the middleware is wired,
// not just constructible.
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

// TestRequestTimeout_DefaultAlwaysOn confirms a Server built without an
// explicit RequestTimeout still bounds the read (the New() default), so
// the protection is on by default rather than opt-in. Here the observed
// deadline comes from the per-handler 8s wrap (tighter than the 15s
// default), which is itself derived from the middleware-bounded context.
func TestRequestTimeout_DefaultAlwaysOn(t *testing.T) {
	reader := &deadlineCapturingHistoryReader{trade: mkNativeUSDTrade()}
	srv := v1.New(v1.Options{History: reader}) // no RequestTimeout set
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/vwap?base=native&quote=fiat:USD")
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
