package v1_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// A scam-gate withhold and a substance-gate withhold share one problem
// type, but only the substance wording may call the market thin or send
// the client to the raw trades: for a flagged issuer those trades are
// the market the platform refused to price.

var scamWithheldForbidden = []string{
	"too thin",
	"trailing market activity",
	"apply your own judgement",
	"/v1/history",
}

func assertScamWithheldBody(t *testing.T, route string, resp *http.Response) {
	t.Helper()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("%s: status = %d, want 404: %s", route, resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "Price withheld — issuer flagged") {
		t.Errorf("%s: withheld body must name the flagged issuer as the cause: %s", route, body)
	}
	for _, wrong := range scamWithheldForbidden {
		if strings.Contains(string(body), wrong) {
			t.Errorf("%s: a flagged issuer's withheld body must not say %q: %s", route, wrong, body)
		}
	}
}

// TestScamWithheldHandlersNameTheFlag drives every handler that answers
// the scam verdict itself.
func TestScamWithheldHandlersNameTheFlag(t *testing.T) {
	base, err := canonical.ParseAsset(flaggedIssuerAsset)
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	native := canonical.NativeAsset()
	for _, route := range []string{
		"/v1/vwap?base=" + base.String() + "&quote=native",
		"/v1/twap?base=" + base.String() + "&quote=native",
		"/v1/chart?base=" + base.String() + "&quote=native&timeframe=24h",
	} {
		t.Run(route, func(t *testing.T) {
			reader := &pairAwareHistoryReader{
				tradesByPair: map[string][]canonical.Trade{
					base.String() + "/native": {scamTestTrade(t, base, native)},
				},
			}
			gate := &scamGateFor{withheld: map[string]bool{base.String(): true}}
			ts := httpTestServer(t, v1.New(v1.Options{History: reader, Scam: gate}))
			assertScamWithheldBody(t, route, mustGet(t, ts.URL+route))
		})
	}
}

// TestReaderScamWithheldNamesTheFlag covers the reader seam: a reader
// that withholds via PriceWithheldError keeps the verdict's reason on
// /v1/price.
func TestReaderScamWithheldNamesTheFlag(t *testing.T) {
	reader := &stubPriceReader{err: v1.PriceWithheldError(pricingguard.WithheldFlaggedIssuer)}
	ts := startHTTPTest(t, v1.New(v1.Options{Prices: reader}).Handler())
	route := "/v1/price?asset=" + flaggedIssuerAsset + "&quote=fiat:USD"
	assertScamWithheldBody(t, route, mustGet(t, ts.URL+route))
}

// TestSubstanceWithheldKeepsRawMarketGuidance is the other side: a thin
// market's raw trades ARE the honest fallback, so its wording keeps it.
func TestSubstanceWithheldKeepsRawMarketGuidance(t *testing.T) {
	reader := &stubPriceReader{err: v1.PriceWithheldError(pricingguard.WithheldThinMarket)}
	ts := startHTTPTest(t, v1.New(v1.Options{Prices: reader}).Handler())
	resp := mustGet(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD")
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "market too thin to aggregate") ||
		!strings.Contains(string(body), "/v1/observations") {
		t.Errorf("substance-withheld body must keep the thin-market title and raw-market guidance: %s", body)
	}
}
