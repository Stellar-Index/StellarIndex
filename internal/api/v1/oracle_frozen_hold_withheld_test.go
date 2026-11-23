package v1_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

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

// TestOraclePrices_FrozenHoldOfWithheldPairIsNotServed: XLM has its own fiat:USD market and the read of it was refused for
// substance, so prices() falls to a peg book while the fiat:USD pair is frozen with a cached VWAP. That cache is written with
// no gate consulted, so serving it would publish the very pair the read refused. No Substance gate is wired: the refusal is
// the direct read's, which is where the production reader applies it.
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

// TestFrozenHoldOfTriangulatedTokenIsServed: XLM has no fiat:USD rows (priced only through a USDC book), so the substance
// floor measured on the literal fiat:USD pair reads as thin by construction. A frozen pair must still serve its held value.
func TestFrozenHoldOfTriangulatedTokenIsServed(t *testing.T) {
	usdc, _ := oraclePegs(t)
	at := time.Now().UTC().Truncate(time.Minute)
	btc := v1.PriceSnapshot{AssetID: "crypto:BTC", Quote: "fiat:USD", Price: "101100", PriceType: "vwap", WindowSeconds: 60, ObservedAt: v1.WireTime(at)}
	cases := map[string]string{
		"/v1/price?base=crypto:XLM&quote=fiat:USD": `"price":"` + heldUSD + `"`,
		lastPriceURL: `"price":"` + heldUSD + `"`,
		pricesURL:    `"price":"` + heldUSD + `"`,
		xlmBTCURL:    `"price":"0.000001"`,
	}
	for path, want := range cases {
		t.Run(path, func(t *testing.T) {
			srv := v1.New(v1.Options{
				Prices: &stubPriceReader{
					snapshots: map[string]v1.PriceSnapshot{btcUSDLeg: btc},
					sources:   map[string][]string{btcUSDLeg: {"binance", "coinbase"}},
					recent: map[string][]v1.PriceSnapshot{
						"crypto:XLM/" + usdc.String(): pegSeries(usdc.String(), "0.1626"),
						"native/" + usdc.String():     pegSeries(usdc.String(), "0.1626"),
					},
				},
				USDPeggedClassics: []canonical.Asset{usdc},
				Substance:         pairSubstanceGate{xlmUSD: true, "native/fiat:USD": true},
				Freeze:            frozenPairs{xlmUSD: true},
				Triangulated:      lkgPairs{xlmUSD + "/300": heldUSD},
			})
			ts := startHTTPTest(t, srv.Handler())

			status, body := getBody(t, ts.URL+path)
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 with the held value: %s", status, body)
			}
			for _, w := range []string{want, `"frozen":true`} {
				if !strings.Contains(body, w) {
					t.Errorf("body missing %s: %s", w, body)
				}
			}
		})
	}
}
