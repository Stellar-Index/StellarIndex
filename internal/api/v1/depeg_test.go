// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

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
)

const (
	depegXLMUSDCPair = "native/" + pegAliasUSDCClassic
	depegUSDCUSDPair = "crypto:USDC/fiat:USD"
)

func depegServer(t *testing.T, usdcUSD string) string {
	t.Helper()
	usdc := installPegAliasRegistry(t)
	byPair := map[string]string{depegXLMUSDCPair: "0.10"}
	if usdcUSD != "" {
		byPair[depegUSDCUSDPair] = usdcUSD
	}
	srv := v1.New(v1.Options{
		PriceAt:            &recordingPriceAtReader{byPair: byPair},
		USDPeggedClassics:  []canonical.Asset{usdc},
		VerifiedCurrencies: newTestCatalogue(t),
	})
	return startHTTPTest(t, srv.Handler()).URL
}

// pegWindowBars gives each observed peg market one bar at `at` holding its observed price, so a window starting there is checked.
func pegWindowBars(observed map[string]string, at time.Time) map[string][]v1.OHLCSeriesBar {
	out := map[string][]v1.OHLCSeriesBar{}
	for pair, price := range observed {
		out[pair] = []v1.OHLCSeriesBar{{T: v1.WireTime(at), O: price, H: price, L: price, C: price}}
	}
	return out
}

// depegRangeHistory serves trades for the triangulated answer and a USDC/USD series that holds $1 at both ends of [from, to)
// but dips to 0.95 in the middle.
type depegRangeHistory struct {
	pairAwareHistoryReader
}

func newDepegRangeHistory(trades map[string][]canonical.Trade, from, to time.Time, dip bool) *depegRangeHistory {
	bars := []v1.OHLCSeriesBar{{T: v1.WireTime(from), O: "1.0", H: "1.0", L: "1.0", C: "1.0"}}
	mid := "1.0"
	if dip {
		mid = "0.95"
	}
	bars = append(bars,
		v1.OHLCSeriesBar{T: v1.WireTime(from.Add(to.Sub(from) / 2)), O: "1.0", H: "1.0", L: mid, C: "1.0"},
		v1.OHLCSeriesBar{T: v1.WireTime(to.Add(-time.Minute)), O: "1.0", H: "1.0", L: "1.0", C: "1.0"})
	h := &depegRangeHistory{pairAwareHistoryReader{tradesByPair: trades}}
	h.ohlcByPair = map[string][]v1.OHLCSeriesBar{depegUSDCUSDPair: bars}
	return h
}

// TestRangeRoutes_ProxyDeviationSeesAMidWindowDepeg: an answer over [from, to) is judged on the peg's whole window, not only at
// `to`. USDC/USD reads $1 at `to` (the point check) but dipped to 0.95 inside the window the trades were priced over.
func TestRangeRoutes_ProxyDeviationSeesAMidWindowDepeg(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	xlm, _ := canonical.ParseAsset("native")
	classicPair, _ := canonical.NewPair(xlm, usdc)
	to := time.Now().UTC().Truncate(time.Minute)
	from := to.Add(-6 * time.Hour)
	trade := canonical.Trade{
		Source: "sdex", Ledger: 1,
		TxHash:      "0000000000000000000000000000000000000000000000000000000000000001",
		Timestamp:   from.Add(3 * time.Hour),
		Pair:        classicPair,
		BaseAmount:  canonical.NewAmount(big.NewInt(100)),
		QuoteAmount: canonical.NewAmount(big.NewInt(16)),
	}
	win := "?base=native&quote=fiat:USD&from=" + from.Format(time.RFC3339) + "&to=" + to.Format(time.RFC3339)
	for _, route := range []string{"/v1/vwap", "/v1/twap", "/v1/ohlc"} {
		for _, dip := range []bool{true, false} {
			srv := v1.New(v1.Options{
				History:            newDepegRangeHistory(map[string][]canonical.Trade{depegXLMUSDCPair: {trade}}, from, to, dip),
				PriceAt:            &recordingPriceAtReader{byPair: map[string]string{depegUSDCUSDPair: "1.0"}},
				USDPeggedClassics:  []canonical.Asset{usdc},
				VerifiedCurrencies: newTestCatalogue(t),
			})
			resp := mustGet(t, startHTTPTest(t, srv.Handler()).URL+route+win)
			body, _ := readAll(resp)
			if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"triangulated":true`) {
				t.Fatalf("%s: status %d, body %s", route, resp.StatusCode, body)
			}
			if got := strings.Contains(body, `"proxy_deviation":true`); got != dip {
				t.Errorf("%s dip=%v: proxy_deviation = %v\n%s", route, dip, got, body)
			}
			if strings.Contains(body, `"proxy_deviation_unchecked":true`) {
				t.Errorf("%s dip=%v: the window's peg series was read, yet unchecked\n%s", route, dip, body)
			}
		}
	}
}

// changesPriceAt answers USDC/USD at $1 except inside [dipFrom, dipTo), where it reads 0.95.
type changesPriceAt struct {
	recordingPriceAtReader
	dipFrom, dipTo time.Time
}

func (r *changesPriceAt) PriceAt(ctx context.Context, pair canonical.Pair, ts time.Time, lb time.Duration) (string, time.Time, int, []string, error) {
	v, at, res, src, err := r.recordingPriceAtReader.PriceAt(ctx, pair, ts, lb)
	if err == nil && pair.String() == depegUSDCUSDPair && !ts.Before(r.dipFrom) && ts.Before(r.dipTo) {
		v = "0.95"
	}
	return v, at, res, src, err
}

// TestPriceChanges_ProxyDeviationAtTheReferenceInstant: each delta is priced at now and at its horizon's reference instant, so
// a peg that held now but was off its band at now-24h (when the H24 reference was priced) raises proxy_deviation.
func TestPriceChanges_ProxyDeviationAtTheReferenceInstant(t *testing.T) {
	usdc := installPegAliasRegistry(t)
	now := time.Now().UTC()
	for _, dip := range []bool{true, false} {
		reader := &changesPriceAt{recordingPriceAtReader: recordingPriceAtReader{byPair: map[string]string{
			depegXLMUSDCPair: "0.10", depegUSDCUSDPair: "1.0",
		}}}
		if dip {
			reader.dipFrom, reader.dipTo = now.Add(-25*time.Hour), now.Add(-23*time.Hour)
		}
		srv := v1.New(v1.Options{PriceAt: reader, USDPeggedClassics: []canonical.Asset{usdc}, VerifiedCurrencies: newTestCatalogue(t)})
		body, _ := readAll(mustGet(t, startHTTPTest(t, srv.Handler()).URL+"/v1/price/changes?asset=native&quote=fiat:USD"))
		if got := strings.Contains(body, `"proxy_deviation":true`); got != dip {
			t.Errorf("dip=%v: proxy_deviation = %v\n%s", dip, got, body)
		}
	}
}

func TestPriceChanges_ProxyDeviationBand(t *testing.T) {
	for _, tc := range []struct {
		usdcUSD string
		want    bool
	}{{"0.95", true}, {"1.001", false}} {
		base := depegServer(t, tc.usdcUSD)
		resp := mustGet(t, base+"/v1/price/changes?asset=native&quote=fiat:USD")
		body, _ := readAll(resp)
		if got := strings.Contains(string(body), `"proxy_deviation":true`); got != tc.want {
			t.Errorf("usdc/usd=%s: proxy_deviation present = %v, want %v\n%s", tc.usdcUSD, got, tc.want, body)
		}
	}
}

func TestPriceChanges_NonstandardDecimals_NormalizesAbsolutesNotPct(t *testing.T) {
	cache := nonstandardDecimalsCacheWith(t, flaggedAsset, 9)
	srv := v1.New(v1.Options{
		PriceAt: m2PriceAtStub{
			histPair:   flaggedAsset + "/fiat:USD",
			current:    "41.32", // now
			historical: "40.00", // every horizon
			bucketAt:   time.Now().UTC().Add(-time.Minute),
		},
		NonstandardDecimals: cache,
	})
	tsrv := startHTTPTest(t, srv.Handler())

	resp := mustGet(t, tsrv.URL+"/v1/price/changes?asset="+flaggedAsset+"&quote=fiat:USD")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := readAll(resp)
	// Absolute prices scaled by K=100 …
	if !strings.Contains(body, `"current_price":"4132.0000000000"`) {
		t.Errorf("current_price not normalized (want 4132.0000000000): %s", body)
	}
	if !strings.Contains(body, `"reference_price":"4000.0000000000"`) {
		t.Errorf("reference_price not normalized (want 4000.0000000000): %s", body)
	}
	// … but change_pct is scale-invariant: (41.32−40)/40 = +3.30%, IDENTICAL
	// to the raw computation. This is the double-application-free invariant.
	if !strings.Contains(body, `"change_pct":"+3.30"`) {
		t.Errorf("change_pct must be scale-invariant (+3.30): %s", body)
	}
}

// TestPriceChanges_DeclaredPegSACTwinSkipsItsOwnFormAndWalksOn is the same
// guard on /v1/price/changes.
func TestPriceChanges_DeclaredPegSACTwinSkipsItsOwnFormAndWalksOn(t *testing.T) {
	base, reader := pegPriceAtServer(t)

	resp := mustGet(t, base+"/v1/price/changes?asset="+pegAliasUSDCSAC+"&quote=fiat:USD")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200. Body: %s", resp.StatusCode, body)
	}
	for _, want := range []string{`"current_price":"1.0004"`, `"quote":"fiat:USD"`, `"triangulated":true`} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %s\n%s", want, body)
		}
	}
	assertSecondPegWalked(t, reader)
}
