package v1_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestVWAPAndTWAPDropExcludedTrades: the arbitrary-window surfaces aggregate raw trades, so an excluded source's trades are
// dropped (the answer is what the survivors alone give), an emptied window is withheld, and an unread set withholds outright.
func TestVWAPAndTWAPDropExcludedTrades(t *testing.T) {
	native, _ := canonical.ParseAsset("native")
	usdc, _ := canonical.ParseAsset(exclUSDC)
	pair, _ := canonical.NewPair(native, usdc)
	now := time.Now().UTC()
	onBook := func(tr canonical.Trade) canonical.Trade { tr.Pair = pair; return tr }
	soroswap := onBook(mkTipTrade(now.Add(-2*time.Minute), 5, 100, "soroswap"))
	sdex := onBook(mkTipTrade(now.Add(-time.Minute), 5, 50, "sdex"))
	from := now.Add(-time.Hour)
	gate := func(e exclTable) v1.PriceExclusionGate {
		return pricingguard.NewSourceExclusionGate(e, pricingguard.SourceExclusionGateOptions{})
	}
	newSrv := func(g v1.PriceExclusionGate, trades ...canonical.Trade) *v1.Server {
		return v1.New(v1.Options{History: &stubHistoryReader{trades: trades}, Exclusions: g, USDPeggedClassics: []canonical.Asset{usdc}})
	}
	strip := func(s string) string { return exclAsOf.ReplaceAllString(s, "") }
	for _, path := range []string{"/v1/vwap?base=native&quote=" + exclUSDC, "/v1/twap?base=native&quote=" + exclUSDC, "/v1/ohlc?base=native&quote=" + exclUSDC} {
		status, baseline := exclGet(t, newSrv(nil, soroswap, sdex), path)
		if status != http.StatusOK || !strings.Contains(baseline, `"price":"15.0000000000"`) && strings.HasPrefix(path, "/v1/vwap") {
			t.Fatalf("%s: ungated %d %s, want 200 over both sources", path, status, baseline)
		}
		if _, clean := exclGet(t, newSrv(gate(exclTable{}), soroswap, sdex), path); strip(clean) != strip(baseline) {
			t.Fatalf("%s: no exclusions %s, want the ungated body %s", path, clean, baseline)
		}
		_, sdexOnly := exclGet(t, newSrv(nil, sdex), path)
		pairScoped := gate(exclTable{rows: []timescale.SourceExclusion{{ID: 1, Source: "soroswap", Base: "crypto:XLM", Quote: exclUSDC, From: from}}})
		if status, body := exclGet(t, newSrv(pairScoped, soroswap, sdex), path); status != http.StatusOK || strip(body) != strip(sdexOnly) {
			t.Fatalf("%s: soroswap excluded %d %s, want exactly the sdex-only answer %s", path, status, body, sdexOnly)
		}
		both := gate(exclTable{rows: []timescale.SourceExclusion{{ID: 1, Source: "soroswap", From: from}, {ID: 2, Source: "sdex", From: from}}})
		if status, body := exclGet(t, newSrv(both, soroswap, sdex), path); status != http.StatusNotFound || !strings.Contains(body, `"source_excluded"`) {
			t.Fatalf("%s: every source excluded %d %s, want 404 source_excluded", path, status, body)
		}
		unread := gate(exclTable{err: errors.New("db down")})
		if status, body := exclGet(t, newSrv(unread, soroswap, sdex), path); status != http.StatusNotFound || !strings.Contains(body, `"unattributed"`) {
			t.Fatalf("%s: exclusions unread %d %s, want a withheld 404 (no-store), never a price", path, status, body)
		}
	}

	// A fiat-quoted window pools the stablecoin books, so an exclusion stored under the requested pair drops the source's trades
	// on every pooled book, not only on a book literally named native/fiat:USD.
	for _, path := range []string{"/v1/vwap?base=native&quote=fiat:USD", "/v1/twap?base=native&quote=fiat:USD", "/v1/ohlc?base=native&quote=fiat:USD"} {
		status, baseline := exclGet(t, newSrv(nil, soroswap, sdex), path)
		if status != http.StatusOK {
			t.Fatalf("%s: ungated %d %s, want 200", path, status, baseline)
		}
		if _, clean := exclGet(t, newSrv(gate(exclTable{}), soroswap, sdex), path); strip(clean) != strip(baseline) {
			t.Fatalf("%s: no exclusions %s, want the ungated body %s", path, clean, baseline)
		}
		_, soroswapOnly := exclGet(t, newSrv(nil, soroswap), path)
		fiatScoped := gate(exclTable{rows: []timescale.SourceExclusion{{ID: 1, Source: "sdex", Base: "native", Quote: "fiat:USD", From: from}}})
		if status, body := exclGet(t, newSrv(fiatScoped, soroswap, sdex), path); status != http.StatusOK || strip(body) != strip(soroswapOnly) {
			t.Fatalf("%s: sdex excluded on the fiat pair %d %s, want exactly the soroswap-only answer %s", path, status, body, soroswapOnly)
		}
	}
}

// TestOHLCSeriesWithholdsExcludedBars: a CAGG bar pools its sources, so a bar an exclusion covers withholds the series
// (a dropped bar would read as a quiet bucket); a bar outside the exclusion's window serves.
func TestOHLCSeriesWithholdsExcludedBars(t *testing.T) {
	bucket := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	bars := []v1.OHLCSeriesBar{{T: v1.WireTime(bucket), O: "1", H: "1", L: "1", C: "1", VBase: "1", VQuote: "1", N: 1, Sources: []string{"soroswap"}}}
	srv := func(rows []timescale.SourceExclusion, err error) *v1.Server {
		g := pricingguard.NewSourceExclusionGate(exclTable{rows: rows, err: err}, pricingguard.SourceExclusionGateOptions{})
		return v1.New(v1.Options{History: &stubHistoryReader{ohlcBars: bars}, Exclusions: g})
	}
	path := "/v1/ohlc?base=native&quote=" + exclUSDC + "&interval=1h"
	if status, body := exclGet(t, srv(nil, nil), path); status != http.StatusOK || !strings.Contains(body, `"soroswap"`) {
		t.Fatalf("no exclusions: %d %s, want the bar served", status, body)
	}
	later := []timescale.SourceExclusion{{ID: 1, Source: "soroswap", From: bucket.Add(2 * time.Hour)}}
	if status, body := exclGet(t, srv(later, nil), path); status != http.StatusOK {
		t.Fatalf("exclusion after the bar: %d %s, want 200", status, body)
	}
	covering := []timescale.SourceExclusion{{ID: 1, Source: "soroswap", From: bucket.Add(-time.Hour)}}
	if status, body := exclGet(t, srv(covering, nil), path); status != http.StatusNotFound || !strings.Contains(body, `"source_excluded"`) {
		t.Fatalf("soroswap excluded over the bar: %d %s, want 404 source_excluded", status, body)
	}
	if status, body := exclGet(t, srv(nil, errors.New("db down")), path); status != http.StatusNotFound {
		t.Fatalf("exclusions unread: %d %s, want a withheld 404, never a bar", status, body)
	}
}
