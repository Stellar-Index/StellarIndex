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

// The withheld body names the gate that fired. Only the thin-market
// verdict invites the reader to price the market from its raw trades;
// a flagged issuer's trades are not a price signal to recompute, and a
// verdict whose cause is unknown must not claim to be the thin one.

const (
	thinMarketTitle = "market too thin to aggregate"
	thinMarketFloor = "trailing market activity is below the serve floor"
	rawSurfaceHint  = "/v1/observations"
	judgementHint   = "apply your own judgement"
)

func withheldBody(t *testing.T, url string) string {
	t.Helper()
	resp := mustGet(t, url)
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusNotFound || !strings.Contains(string(body), "errors/price-withheld") {
		t.Fatalf("want 404 price-withheld, got %d: %s", resp.StatusCode, body)
	}
	return string(body)
}

func assertFlaggedIssuerBody(t *testing.T, body string) {
	t.Helper()
	if !strings.Contains(body, "issuer flagged") || !strings.Contains(body, "directory-flagged issuer") {
		t.Errorf("body must name the flagged issuer as the cause: %s", body)
	}
	for _, s := range []string{thinMarketTitle, thinMarketFloor, rawSurfaceHint, judgementHint} {
		if strings.Contains(body, s) {
			t.Errorf("flagged-issuer body contains %q — it must neither call the market thin nor "+
				"point the reader at the raw trades to price it themselves: %s", s, body)
		}
	}
}

func TestVWAPScamWithheldNamesFlaggedIssuer(t *testing.T) {
	base, err := canonical.ParseAsset(flaggedIssuerAsset)
	if err != nil {
		t.Fatalf("parse base: %v", err)
	}
	native := canonical.NativeAsset()
	reader := &pairAwareHistoryReader{tradesByPair: map[string][]canonical.Trade{
		base.String() + "/native": {scamTestTrade(t, base, native)},
	}}
	srv := v1.New(v1.Options{History: reader, Scam: &scamGateFor{withheld: map[string]bool{base.String(): true}}})
	ts := httpTestServer(t, srv)

	assertFlaggedIssuerBody(t, withheldBody(t, ts.URL+"/v1/vwap?base="+base.String()+"&quote=native"))
	assertFlaggedIssuerBody(t, withheldBody(t, ts.URL+"/v1/twap?base="+base.String()+"&quote=native"))
}

// Both gates refuse on /v1/price/tip: the substance gate used to be asked
// first and short-circuit, so a flagged issuer's thin market was
// described as merely thin.
func TestPriceTipThinAndFlaggedReportsFlaggedIssuer(t *testing.T) {
	flagged, err := canonical.NewClassicAsset("RIO", scamQuoteLegIssuer)
	if err != nil {
		t.Fatalf("build flagged classic: %v", err)
	}
	reader := &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{
		"native/" + flagged.String(): {Price: "0.12", PriceType: "vwap"},
	}}
	substance := &stubSubstanceGate{allow: false}
	dir := &scamQuoteLegDirectory{flagged: map[string]bool{scamQuoteLegIssuer: true}}
	srv := v1.New(v1.Options{
		Prices:    reader,
		Substance: substance,
		Scam:      pricingguard.NewScamGate(dir, pricingguard.ScamGateOptions{}),
	})
	ts := startHTTPTest(t, srv.Handler())

	assertFlaggedIssuerBody(t, withheldBody(t, ts.URL+"/v1/price/tip?asset=native&quote="+flagged.String()))
	if len(substance.surfaces) == 0 {
		t.Error("the substance gate must still be consulted (its withheld metric is unchanged)")
	}
}

// A reader-backed verdict (cmd/stellarindex-api's storePriceReader)
// carries its cause through PriceWithheldError to the body.
func TestPriceReaderVerdictCarriesItsCause(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{err: v1.PriceWithheldError(pricingguard.WithheldFlaggedIssuer)}})
	ts := startHTTPTest(t, srv.Handler())
	assertFlaggedIssuerBody(t, withheldBody(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD"))

	srv = v1.New(v1.Options{Prices: &stubPriceReader{err: v1.PriceWithheldError(pricingguard.WithheldThinMarket)}})
	ts = startHTTPTest(t, srv.Handler())
	body := withheldBody(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD")
	for _, s := range []string{thinMarketTitle, thinMarketFloor, rawSurfaceHint, judgementHint} {
		if !strings.Contains(body, s) {
			t.Errorf("thin-market body lost %q — the raw-surface guidance belongs to this verdict: %s", s, body)
		}
	}
}

// A bare ErrPriceWithheld carries no cause, so the body must not assert one.
func TestPriceUnattributedVerdictClaimsNoCause(t *testing.T) {
	srv := v1.New(v1.Options{Prices: &stubPriceReader{err: v1.ErrPriceWithheld}})
	ts := startHTTPTest(t, srv.Handler())
	body := withheldBody(t, ts.URL+"/v1/price?asset=native&quote=fiat:USD")
	for _, s := range []string{thinMarketTitle, thinMarketFloor, judgementHint} {
		if strings.Contains(body, s) {
			t.Errorf("unattributed body contains %q — it may be a flagged issuer's market: %s", s, body)
		}
	}
}
