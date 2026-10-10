// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

const (
	devUSDC  = "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	devToken = "AQUA-GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"
)

// devPriceAtStub answers PriceAt for the listed pairs and misses on the rest.
type devPriceAtStub map[string]string

func (s devPriceAtStub) PriceAt(_ context.Context, pair canonical.Pair, ts time.Time, _ time.Duration) (string, time.Time, int, error) {
	if v, ok := s[pair.Base.String()+"/"+pair.Quote.String()]; ok {
		return v, ts.Add(-time.Minute), 60, nil
	}
	return "", time.Time{}, 0, v1.ErrPriceAtUnavailable
}

// devDeviationServer serves devToken/fiat:USD only through the declared USDC
// peg (closed buckets for /v1/price*, raw trades for /v1/twap and /v1/ohlc);
// usdcUSD is the peg's own observed dollar price ("" = unobserved).
func devDeviationServer(t *testing.T, usdcUSD string) *testServerImpl {
	t.Helper()
	usdc, err := canonical.ParseAsset(devUSDC)
	if err != nil {
		t.Fatalf("parse USDC: %v", err)
	}
	token, err := canonical.ParseAsset(devToken)
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	pair, err := canonical.NewPair(token, usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}
	t0 := sacReachDay(0)
	at := v1.WireTime(time.Unix(1745000000, 0).UTC())
	snap := func(base, quote, price string) v1.PriceSnapshot {
		return v1.PriceSnapshot{AssetID: base, Quote: quote, Price: price, PriceType: "vwap", ObservedAt: at, WindowSeconds: 60}
	}
	byPair := map[string]string{}
	if usdcUSD != "" {
		byPair["crypto:USDC/fiat:USD"] = usdcUSD
	}
	srv := v1.New(v1.Options{
		Prices: &stubPriceReader{
			snapshots: map[string]v1.PriceSnapshot{
				devToken + "/" + devUSDC: snap(devToken, devUSDC, "2.0000"),
				"native/fiat:USD":        snap("native", "fiat:USD", "0.10"),
			},
			sources: map[string][]string{
				devToken + "/" + devUSDC: {"sdex"},
				"native/fiat:USD":        {"binance"},
			},
		},
		PriceAt: devPriceAtStub(byPair),
		History: &fiatConstituentReader{tradesByPair: map[string][]canonical.Trade{
			fiatParityPairKey(pair): {
				fiatParityTrade(pair, 1, t0.Add(time.Minute), 1000, 2000),
				fiatParityTrade(pair, 2, t0.Add(2*time.Minute), 1000, 2000),
			},
		}},
		USDPeggedClassics: []canonical.Asset{usdc},
	})
	return startHTTPTest(t, srv.Handler())
}

// Each primary price surface, served triangulated through the declared peg,
// flags proxy_deviation only when the peg's own dollar price is off band.
func TestPrimarySurfaces_ProxyDeviation(t *testing.T) {
	t0 := sacReachDay(0)
	win := "&from=" + t0.Format(time.RFC3339) + "&to=" + t0.Add(time.Hour).Format(time.RFC3339)
	paths := map[string]string{
		"price":        "/v1/price?asset=" + devToken + "&quote=fiat:USD",
		"price_batch":  "/v1/price/batch?asset_ids=" + devToken + "&quote=fiat:USD",
		"price_tip":    "/v1/price/tip?asset=" + devToken + "&quote=fiat:USD",
		"twap":         "/v1/twap?base=" + devToken + "&quote=fiat:USD" + win,
		"ohlc":         "/v1/ohlc?base=" + devToken + "&quote=fiat:USD" + win,
		"lastprice":    "/v1/oracle/lastprice?asset=" + devToken,
		"x_last_price": "/v1/oracle/x_last_price?base=" + devToken + "&quote=fiat:USD",
	}
	for name, path := range paths {
		for _, tc := range []struct {
			label, usdcUSD string
			want           bool
		}{
			{"depegged below", "0.95", true},
			{"depegged above", "1.03", true},
			{"on peg", "1.001", false},
			{"no observation", "", false},
		} {
			t.Run(name+"/"+tc.label, func(t *testing.T) {
				ts := devDeviationServer(t, tc.usdcUSD)
				resp := mustGet(t, ts.URL+path)
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("status = %d, want 200", resp.StatusCode)
				}
				body, _ := readAll(resp)
				if name != "price_tip" && !strings.Contains(body, `"triangulated":true`) {
					t.Fatalf("fixture must be served through the peg: %s", body)
				}
				if got := strings.Contains(body, `"proxy_deviation":true`); got != tc.want {
					t.Errorf("proxy_deviation present = %v, want %v: %s", got, tc.want, body)
				}
			})
		}
	}
}
