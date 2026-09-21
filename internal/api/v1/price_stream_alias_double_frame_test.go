package v1_test

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestPriceStream_AliasFanOut_MergesDistinctVenuesIntoOneStream is a
// documented-defect reproduction for RLT-345, not a green regression
// guard: it records CURRENT behaviour so the fix (tracked separately —
// see the finding) has a concrete before/after to work from, rather
// than landing on top of an un-derived symptom.
//
// cmd/stellarindex-aggregator/main.go's defaultPairs() computes XLM
// under BOTH `native` and `crypto:XLM` as independent (base, quote)
// pairs — deliberately, so a future CEX connector populates the
// abstract side without a config change (see that function's own
// comment). internal/api/v1/price_stream.go's alias fan-out then
// subscribes a single client to every alias topic of its requested
// asset, so a `?asset=native` subscriber's ONE connection receives
// BOTH pairs' publishes, presented as sequential updates on what looks
// like one series. On r1 today this is latent: publishToStream in
// internal/aggregate/orchestrator only fires on a successful VWAP
// write, and crypto:XLM has no venue wired, so it never publishes.
// This test proves the client-visible merge itself, independent of
// whether the aggregator is currently driving it — the trigger the
// finding names ("a future deployment with Binance/Coinbase running")
// is a config change away, not a code change away.
//
// The fix is NOT implemented here: it requires either collapsing
// alias topics to one canonical topic per market on the subscribe
// side, or a producer id/sequence on the wire event
// (internal/api/streaming/redispub/event.go) enforced by the
// redispub Subscriber (internal/api/streaming/redispub/subscriber.go)
// — both outside this fix's file scope, and a product decision on
// which venue should win a merged tick belongs with whoever owns that
// scope, not a silent default here.
func TestPriceStream_AliasFanOut_MergesDistinctVenuesIntoOneStream(t *testing.T) {
	hub := streaming.NewHub(0)
	srv := v1.New(v1.Options{Hub: hub})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	native, _ := canonical.ParseAsset("native")
	cryptoXLM, _ := canonical.ParseAsset("crypto:XLM")
	usd, _ := canonical.ParseAsset("fiat:USD")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		ts.URL+"/v1/price/stream?asset=native&quote=fiat:USD", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	time.Sleep(50 * time.Millisecond)

	// Two independent venues' closed buckets for the SAME nominal
	// tick, published under their own (base, quote) pairs exactly as
	// defaultPairs() configures the aggregator to compute them.
	hub.Publish(v1.PriceStreamTopic(native, usd, 300), "price_update", []byte(`{"price":"0.1050","venue":"onchain"}`))
	hub.Publish(v1.PriceStreamTopic(cryptoXLM, usd, 300), "price_update", []byte(`{"price":"0.1200","venue":"cex"}`))

	br := bufio.NewReader(resp.Body)
	first := readPriceStreamFrame(t, br, 2*time.Second)
	second := readPriceStreamFrame(t, br, 2*time.Second)

	if !strings.Contains(first, `"venue":"onchain"`) {
		t.Fatalf("first frame = %q, want the native/onchain publish", first)
	}
	if !strings.Contains(second, `"venue":"cex"`) {
		t.Fatalf("second frame = %q, want the crypto:XLM/cex publish — a SINGLE "+
			"`?asset=native` subscription received a SECOND, independently-sourced "+
			"price for what it presented as one series, with nothing on the wire "+
			"(or in this handler) to tell the client the two frames came from "+
			"different venues (RLT-345)", second)
	}
}
