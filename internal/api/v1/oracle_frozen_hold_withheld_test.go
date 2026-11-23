package v1_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
)

// pairSubstanceGate refuses exactly the pairs it names, so one market of an asset can be withheld while another serves.
type pairSubstanceGate map[string]bool

func (g pairSubstanceGate) Allowed(_ context.Context, base, quote canonical.Asset, _ string) bool {
	return !g[base.String()+"/"+quote.String()]
}

func (g pairSubstanceGate) Probe(ctx context.Context, base, quote canonical.Asset) (allowed, measured bool, floor pricingguard.SubstanceFloor) {
	return g.Allowed(ctx, base, quote, "probe"), true, pricingguard.FloorNone
}

// TestOraclePrices_FrozenHoldOfWithheldPairIsNotServed: the fiat:USD market is refused by the substance gate, prices()
// falls to a peg book, and the fiat:USD pair is frozen with a cached VWAP. That cache is written with no gate consulted,
// so serving it would publish the very pair the gate refused.
func TestOraclePrices_FrozenHoldOfWithheldPairIsNotServed(t *testing.T) {
	usdc, _ := oraclePegs(t)
	srv := v1.New(v1.Options{
		Prices: &stubPriceReader{
			errByPair: map[string]error{
				"crypto:XLM/fiat:USD": v1.PriceWithheldError(pricingguard.WithheldThinMarket),
				"native/fiat:USD":     v1.PriceWithheldError(pricingguard.WithheldThinMarket),
			},
			recent: map[string][]v1.PriceSnapshot{
				"crypto:XLM/" + usdc.String(): pegSeries(usdc.String(), "0.1626"),
				"native/" + usdc.String():     pegSeries(usdc.String(), "0.1626"),
			},
		},
		USDPeggedClassics: []canonical.Asset{usdc},
		Substance:         pairSubstanceGate{xlmUSD: true, "native/fiat:USD": true},
		Freeze:            frozenPairs{xlmUSD: true, "native/fiat:USD": true},
		Triangulated:      lkgPairs{xlmUSD + "/300": heldUSD, "native/fiat:USD/300": heldUSD},
	})
	ts := startHTTPTest(t, srv.Handler())

	status, body := getBody(t, ts.URL+pricesURL)
	if strings.Contains(body, heldUSD) {
		t.Fatalf("served the withheld pair's cached VWAP %s: %d %s", heldUSD, status, body)
	}
	if status != http.StatusNotFound || !strings.Contains(body, "errors/price-withheld") {
		t.Fatalf("status = %d, want 404 price-withheld: %s", status, body)
	}
}
