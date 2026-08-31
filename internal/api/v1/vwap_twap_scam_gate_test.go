package v1_test

import (
	"context"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// Regression suite for wave-D MSP-02 / EXR-04: /v1/vwap and /v1/twap
// served a directory-scam-flagged issuer's aggregated price at 200,
// while /v1/price, /v1/price/tip, /v1/price/batch, the SEP-40 oracle
// and the asset headline all withheld it.
//
// The gap was DOCUMENTED AS FIXED. pricingguard/scam.go's package doc
// claimed the gate sat "at the price-reader seam so every reader-backed
// surface (/v1/price, /v1/price/batch, /v1/twap, /v1/vwap, …) is
// covered by ONE gate", and PR #182's merged body repeated it verbatim.
// Neither endpoint goes through the price reader at all — both compute
// from raw trades via their own fetch — so the claim was never true and
// no test contradicted it.
//
// Reproduced live before the fix against a flagged issuer:
// /v1/price → 404 price-withheld, /v1/price/tip → 404 price-withheld,
// but /v1/vwap → 200 with a price and /v1/twap → 200 with a price.

// scamGateFor withholds exactly the listed bases and records the surface
// label each call was made with, so a test can assert the gate was
// consulted with the right identity — not merely that a 404 appeared.
type scamGateFor struct {
	withheld map[string]bool
	surfaces []string
}

func (g *scamGateFor) Withheld(_ context.Context, base canonical.Asset, surface string) bool {
	g.surfaces = append(g.surfaces, surface)
	return g.withheld[base.String()]
}

// scamTestTrade builds one trade for the pair so the ungated path would
// otherwise serve a real 200 with a price — without it the endpoints
// would 404 for lack of data and the test would pass vacuously.
func scamTestTrade(t *testing.T, base, quote canonical.Asset) canonical.Trade {
	t.Helper()
	pair, err := canonical.NewPair(base, quote)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	return canonical.Trade{
		Source: "sdex", Ledger: 1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000002",
		Timestamp:   time.Now().UTC().Add(-time.Hour),
		Pair:        pair,
		BaseAmount:  canonical.NewAmount(big.NewInt(1_000_000)),
		QuoteAmount: canonical.NewAmount(big.NewInt(160_000)),
	}
}

// flaggedIssuerAsset is a classic asset whose issuer stands in for a
// directory-scam-flagged one. The gate stub keys on the asset id, so the
// specific strkey only has to parse.
const flaggedIssuerAsset = "RIO-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"

func TestVWAPWithholdsScamFlaggedIssuer(t *testing.T) {
	base, err := canonical.ParseAsset(flaggedIssuerAsset)
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	native, _ := canonical.ParseAsset("native")

	reader := &pairAwareHistoryReader{
		tradesByPair: map[string][]canonical.Trade{
			base.String() + "/native": {scamTestTrade(t, base, native)},
		},
	}
	gate := &scamGateFor{withheld: map[string]bool{base.String(): true}}
	srv := v1.New(v1.Options{History: reader, Scam: gate})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/vwap?base="+base.String()+"&quote=native")
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/v1/vwap status = %d, want 404 — a scam-flagged issuer's "+
			"aggregated price must be withheld here exactly as it is on "+
			"/v1/price and /v1/price/tip (MSP-02). Body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("/v1/vwap body missing the price-withheld problem type: %s", body)
	}
	if len(gate.surfaces) == 0 || gate.surfaces[0] != "vwap" {
		t.Errorf("gate surfaces = %v, want the first to be \"vwap\" — the surface "+
			"label is how an operator attributes a withholding decision", gate.surfaces)
	}
}

func TestTWAPWithholdsScamFlaggedIssuer(t *testing.T) {
	base, err := canonical.ParseAsset(flaggedIssuerAsset)
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	native, _ := canonical.ParseAsset("native")

	reader := &pairAwareHistoryReader{
		tradesByPair: map[string][]canonical.Trade{
			base.String() + "/native": {scamTestTrade(t, base, native)},
		},
	}
	gate := &scamGateFor{withheld: map[string]bool{base.String(): true}}
	srv := v1.New(v1.Options{History: reader, Scam: gate})
	ts := httpTestServer(t, srv)

	resp := mustGet(t, ts.URL+"/v1/twap?base="+base.String()+"&quote=native")
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/v1/twap status = %d, want 404 (MSP-02). Body: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "errors/price-withheld") {
		t.Errorf("/v1/twap body missing the price-withheld problem type: %s", body)
	}
	if len(gate.surfaces) == 0 || gate.surfaces[0] != "twap" {
		t.Errorf("gate surfaces = %v, want the first to be \"twap\"", gate.surfaces)
	}
}

// TestVWAPTWAPGateKeysOnBaseNotQuote — the gate must key on the BASE
// alone, so it survives the frontend's XLM triangulation. Keying on the
// pair would let /v1/vwap?base=<flagged>&quote=native through whenever
// the flagged asset appeared on the other side of a different quote.
func TestVWAPTWAPGateKeysOnBaseNotQuote(t *testing.T) {
	base, err := canonical.ParseAsset(flaggedIssuerAsset)
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	usdc, err := canonical.ParseAsset(w2t2USDC)
	if err != nil {
		t.Fatalf("parse usdc: %v", err)
	}

	reader := &pairAwareHistoryReader{
		tradesByPair: map[string][]canonical.Trade{
			base.String() + "/" + usdc.String(): {scamTestTrade(t, base, usdc)},
		},
	}
	gate := &scamGateFor{withheld: map[string]bool{base.String(): true}}
	srv := v1.New(v1.Options{History: reader, Scam: gate})
	ts := httpTestServer(t, srv)

	for _, path := range []string{"/v1/vwap", "/v1/twap"} {
		resp := mustGet(t, ts.URL+path+"?base="+base.String()+"&quote="+usdc.String())
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s with a different quote returned %d, want 404 — the gate "+
				"keys on the BASE so it covers every quote", path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}

// TestVWAPTWAPServeUnflaggedIssuer is the blast-radius guard. The gate
// must withhold ONLY flagged bases; if it 404s an ordinary pair the fix
// has broken two public endpoints for everyone.
func TestVWAPTWAPServeUnflaggedIssuer(t *testing.T) {
	usdc, err := canonical.ParseAsset(w2t2USDC)
	if err != nil {
		t.Fatalf("parse usdc: %v", err)
	}
	native, _ := canonical.ParseAsset("native")

	reader := &pairAwareHistoryReader{
		tradesByPair: map[string][]canonical.Trade{
			"native/" + usdc.String(): {scamTestTrade(t, native, usdc)},
		},
	}
	// Gate is wired but flags nothing.
	gate := &scamGateFor{withheld: map[string]bool{}}
	srv := v1.New(v1.Options{History: reader, Scam: gate})
	ts := httpTestServer(t, srv)

	for _, path := range []string{"/v1/vwap", "/v1/twap"} {
		resp := mustGet(t, ts.URL+path+"?base=native&quote="+usdc.String())
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s for an UNFLAGGED pair returned %d, want 200 — the scam gate "+
				"must not withhold ordinary markets. Body: %s", path, resp.StatusCode, body)
		}
	}
}

// TestVWAPTWAPNilGateServes — a deployment with no gate wired must keep
// serving. Every other gate in this package is nil-safe and these two
// call sites must not be the exception.
func TestVWAPTWAPNilGateServes(t *testing.T) {
	usdc, err := canonical.ParseAsset(w2t2USDC)
	if err != nil {
		t.Fatalf("parse usdc: %v", err)
	}
	native, _ := canonical.ParseAsset("native")

	reader := &pairAwareHistoryReader{
		tradesByPair: map[string][]canonical.Trade{
			"native/" + usdc.String(): {scamTestTrade(t, native, usdc)},
		},
	}
	srv := v1.New(v1.Options{History: reader}) // no Scam gate
	ts := httpTestServer(t, srv)

	for _, path := range []string{"/v1/vwap", "/v1/twap"} {
		resp := mustGet(t, ts.URL+path+"?base=native&quote="+usdc.String())
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s with a nil scam gate returned %d, want 200", path, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
}
