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
		if status, body := exclGet(t, newSrv(pairScoped, soroswap, sdex), path); status != http.StatusOK || strip(unmarked(t, body)) != strip(sdexOnly) {
			t.Fatalf("%s: soroswap excluded %d %s, want the sdex-only answer %s marked fills withheld", path, status, body, sdexOnly)
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
		if status, body := exclGet(t, newSrv(fiatScoped, soroswap, sdex), path); status != http.StatusOK || strip(unmarked(t, body)) != strip(soroswapOnly) {
			t.Fatalf("%s: sdex excluded on the fiat pair %d %s, want the soroswap-only answer %s marked fills withheld", path, status, body, soroswapOnly)
		}
	}
}

// unmarked is a recomputed answer's body less its fills-withheld marker, which it must carry: without it the caller cannot
// tell the survivors' answer from the market's.
func unmarked(t *testing.T, body string) string {
	t.Helper()
	const marker = `,"fills_withheld_reason":"source_excluded"`
	if strings.Count(body, marker) != 1 {
		t.Fatalf("recomputed body carries no fills_withheld_reason marker: %s", body)
	}
	return strings.Replace(body, marker, "", 1)
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

// TestOHLCSeriesWithholdsExcludedConstituentBars: a fiat-quoted series pools each stablecoin market's bars, so an exclusion
// stored on the constituent market that fed a bar withholds the series; one on a constituent that fed no bar does not. An alias
// spelling's bars are judged under the market that supplied them.
func TestOHLCSeriesWithholdsExcludedConstituentBars(t *testing.T) {
	const usdt = "USDT-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"
	native, _ := canonical.ParseAsset("native")
	cryptoXLM, _ := canonical.ParseAsset("crypto:XLM")
	usdc, _ := canonical.ParseAsset(exclUSDC)
	usdtA, _ := canonical.ParseAsset(usdt)
	fed, _ := canonical.NewPair(native, usdc)
	aliasFed, _ := canonical.NewPair(cryptoXLM, usdc)
	bucket := time.Now().UTC().Truncate(time.Hour).Add(-3 * time.Hour)
	bar := v1.OHLCSeriesBar{T: v1.WireTime(bucket), O: "1", H: "1", L: "1", C: "1", VBase: "1", VQuote: "1", N: 1, Sources: []string{"soroswap"}}
	srv := func(book canonical.Pair, rows ...timescale.SourceExclusion) *v1.Server {
		g := pricingguard.NewSourceExclusionGate(exclTable{rows: rows}, pricingguard.SourceExclusionGateOptions{})
		h := &stubHistoryReader{ohlcByPair: map[string][]v1.OHLCSeriesBar{book.String(): {bar}}}
		return v1.New(v1.Options{History: h, Exclusions: g, USDPeggedClassics: []canonical.Asset{usdc, usdtA}})
	}
	on := func(base, quote string) timescale.SourceExclusion {
		return timescale.SourceExclusion{ID: 1, Source: "soroswap", Base: base, Quote: quote, From: bucket.Add(-time.Hour)}
	}
	fiat := "/v1/ohlc?base=native&quote=fiat:USD&interval=1h"
	if status, body := exclGet(t, srv(fed), fiat); status != http.StatusOK || !strings.Contains(body, `"soroswap"`) {
		t.Fatalf("no exclusions: %d %s, want the pooled bar served", status, body)
	}
	if status, body := exclGet(t, srv(fed, on("native", exclUSDC)), fiat); status != http.StatusNotFound || !strings.Contains(body, `"source_excluded"`) {
		t.Fatalf("soroswap excluded on the constituent that fed the bar: %d %s, want 404 source_excluded", status, body)
	}
	if status, body := exclGet(t, srv(fed, on("native", usdt)), fiat); status != http.StatusOK {
		t.Fatalf("soroswap excluded on a constituent that fed no bar: %d %s, want 200", status, body)
	}
	alias := "/v1/ohlc?base=native&quote=" + exclUSDC + "&interval=1h"
	if status, body := exclGet(t, srv(aliasFed, on("crypto:XLM", exclUSDC)), alias); status != http.StatusNotFound || !strings.Contains(body, `"source_excluded"`) {
		t.Fatalf("soroswap excluded on the alias spelling that fed the bar: %d %s, want 404 source_excluded", status, body)
	}
}
