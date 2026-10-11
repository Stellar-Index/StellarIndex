package v1_test

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// Regression suite for the scam gate's quote-leg check on /v1/price/tip (computeTip) and the
// closed-bucket SSE stream (closedStreamWithheld). `?asset=native&quote=<FLAGGED>` describes the
// same market that `?asset=<FLAGGED>&quote=native` is withheld for, so asking the gate only the
// base-only question would publish the flagged issuer's own trades unauthenticated, once per
// closed bucket for the life of an SSE connection.
//
// Driven through the REAL *pricingguard.ScamGate over a stub directory, as in the /v1/vwap
// quote-leg suite: a fake answering only the base-only question would pass without the
// pair-aware primitive. The directory records every address it is asked about, so "the QUOTE leg
// reached the gate" is proved rather than inferred from a status code.

// TestPriceTipWithholdsWhenTheQuoteLegIsScamFlagged pins the request surface. The stub reader
// holds a snapshot for the inverted orientation, so an ungated handler answers 200 with a price;
// without it the endpoint would 404 for lack of data and the test would pass vacuously.
func TestPriceTipWithholdsWhenTheQuoteLegIsScamFlagged(t *testing.T) {
	flagged, err := canonical.NewClassicAsset("RIO", scamQuoteLegIssuer)
	if err != nil {
		t.Fatalf("build flagged classic: %v", err)
	}

	prices := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/" + flagged.String(): {
				AssetID:    "native",
				Quote:      flagged.String(),
				Price:      "138.5",
				PriceType:  "last_trade",
				ObservedAt: v1.WireTime(time.Unix(1_745_000_000, 0).UTC()),
			},
		},
	}
	dir := &scamQuoteLegDirectory{flagged: map[string]bool{scamQuoteLegIssuer: true}}
	srv := v1.New(v1.Options{
		Prices: prices,
		Scam:   pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{}),
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote="+flagged.String())
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/v1/price/tip?asset=native&quote=%s status = %d, want 404 — naming the "+
			"flagged issuer as the QUOTE republishes the withheld market's price as its "+
			"exact reciprocal on the freshest surface we serve. Body: %s",
			flagged.String(), resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("body missing the price-withheld problem type: %s", body)
	}
	if strings.Contains(string(body), "138.5") {
		t.Errorf("the withheld price leaked into the refusal body: %s", body)
	}
	if len(dir.asked) == 0 {
		t.Fatal("the directory was never asked about any address — the QUOTE leg never " +
			"reached the gate, so a 404 here came from something other than the " +
			"withholding decision")
	}
	if dir.asked[0] != scamQuoteLegIssuer {
		t.Errorf("directory asked about %v, want the QUOTE leg's issuer %q first",
			dir.asked, scamQuoteLegIssuer)
	}
}

// TestPriceTipServesWhenNeitherLegIsFlagged is the blast-radius guard
// for the request surface: folding the quote leg in must not withhold
// ordinary markets.
func TestPriceTipServesWhenNeitherLegIsFlagged(t *testing.T) {
	prices := &stubPriceReader{
		snapshots: map[string]v1.PriceSnapshot{
			"native/fiat:USD": {
				AssetID:    "native",
				Quote:      "fiat:USD",
				Price:      "0.1242",
				PriceType:  "last_trade",
				ObservedAt: v1.WireTime(time.Unix(1_745_000_000, 0).UTC()),
			},
		},
	}
	dir := &scamQuoteLegDirectory{flagged: map[string]bool{}}
	srv := v1.New(v1.Options{
		Prices: prices,
		Scam:   pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{}),
	})
	ts := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, ts.URL+"/v1/price/tip?asset=native&quote=fiat:USD")
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/v1/price/tip for an unflagged pair returned %d, want 200 — the "+
			"pair-aware gate must withhold only flagged issuers. Body: %s",
			resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "0.1242") {
		t.Errorf("unflagged tip lost its price: %s", body)
	}
}

// TestPriceStreamWithholdsWhenTheQuoteLegIsScamFlagged pins the closed-bucket SSE surface at
// connect, before the response switches to SSE mode (once SSE headers are out it is too late
// to send a 404).
//
// The connect pre-flight and the per-bucket re-check in forwardClosedStream are the same
// predicate, Server.closedStreamWithheld, so pinning it pins both halves.
func TestPriceStreamWithholdsWhenTheQuoteLegIsScamFlagged(t *testing.T) {
	flagged, err := canonical.NewClassicAsset("RIO", scamQuoteLegIssuer)
	if err != nil {
		t.Fatalf("build flagged classic: %v", err)
	}

	hub := streaming.NewHub(0)
	dir := &scamQuoteLegDirectory{flagged: map[string]bool{scamQuoteLegIssuer: true}}
	srv := v1.New(v1.Options{
		Hub:  hub,
		Scam: pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{}),
	})
	ts := startHTTPTest(t, srv.Handler())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		ts.URL+"/v1/price/stream?asset=native&quote="+flagged.String(), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/v1/price/stream?asset=native&quote=%s status = %d, want 404 — the "+
			"flagged market's price, inverted, was fanned out once per closed bucket "+
			"for the life of the connection. Body: %s",
			flagged.String(), resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("body missing the price-withheld problem type: %s", body)
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "event-stream") {
		t.Errorf("response switched into SSE mode on a withheld pair: %s",
			resp.Header.Get("Content-Type"))
	}
	if len(dir.asked) == 0 {
		t.Fatal("the directory was never asked about any address — the QUOTE leg never " +
			"reached the gate on the stream surface")
	}
	if dir.asked[0] != scamQuoteLegIssuer {
		t.Errorf("directory asked about %v, want the QUOTE leg's issuer %q first",
			dir.asked, scamQuoteLegIssuer)
	}
}

// TestPriceStreamServesWhenNeitherLegIsFlagged is the blast-radius guard
// for the stream surface: the pair-aware fold must not stop an ordinary
// market's closed buckets reaching the wire.
func TestPriceStreamServesWhenNeitherLegIsFlagged(t *testing.T) {
	hub := streaming.NewHub(0)
	dir := &scamQuoteLegDirectory{flagged: map[string]bool{}}
	srv := v1.New(v1.Options{
		Hub:  hub,
		Scam: pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{}),
	})
	ts := startHTTPTest(t, srv.Handler())

	xlm, _ := canonical.ParseAsset("native")
	usd, _ := canonical.ParseAsset("fiat:USD")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		ts.URL+"/v1/price/stream?asset=native&quote=fiat:USD", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200 — neither leg is flagged", resp.StatusCode)
	}

	time.Sleep(50 * time.Millisecond)
	hub.Publish(v1.PriceStreamTopic(xlm, usd, 300), "price_update", []byte(`{"price":"0.11"}`))

	br := bufio.NewReader(resp.Body)
	if frame := readPriceStreamFrame(t, br, 3*time.Second); !strings.Contains(frame, `"price":"0.11"`) {
		t.Fatalf("unflagged pair stopped streaming after the pair-aware fold; frame = %q", frame)
	}
}
