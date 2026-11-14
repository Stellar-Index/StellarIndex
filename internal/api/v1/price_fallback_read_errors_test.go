package v1_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// failingVWAPLooker is a VWAP cache that cannot be read.
type failingVWAPLooker struct{}

func (failingVWAPLooker) LookupTriangulatedVWAP(context.Context, canonical.Asset, canonical.Asset, time.Duration) (v1.CachedVWAP, bool, error) {
	return v1.CachedVWAP{}, false, errors.New("redis: connection refused")
}

// TestPriceFallback_ReadFailureIsNotAMiss: a fallback layer that could not be read is a retryable
// error on every price surface, never "no trades" and never the next peg's market.
func TestPriceFallback_ReadFailureIsNotAMiss(t *testing.T) {
	usdc, usdt := oraclePegsForFallback(t)
	foo := "FOO-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
	broken := errors.New("pool acquire timeout")
	secondPeg := map[string]v1.PriceSnapshot{foo + "/" + usdt.String(): {Price: "0.50", PriceType: "vwap"}}

	for name, tc := range map[string]struct {
		opts v1.Options
		path string
	}{
		"first peg read fails, second peg has a price": {
			v1.Options{Prices: &stubPriceReader{snapshots: secondPeg, errByPair: map[string]error{foo + "/" + usdc.String(): broken}}},
			"/v1/price?asset=" + foo + "&quote=fiat:USD",
		},
		"tip: first peg read fails": {
			v1.Options{Prices: &stubPriceReader{snapshots: secondPeg, errByPair: map[string]error{foo + "/" + usdc.String(): broken}}},
			"/v1/price/tip?asset=" + foo + "&quote=fiat:USD",
		},
		"crypto cross: the quote's USD leg read fails": {
			v1.Options{Prices: &stubPriceReader{
				snapshots: map[string]v1.PriceSnapshot{foo + "/fiat:USD": {Price: "0.50", PriceType: "vwap"}},
				errByPair: map[string]error{"crypto:BTC/fiat:USD": broken},
			}},
			"/v1/price?asset=" + foo + "&quote=crypto:BTC",
		},
		"cache unread and nothing below answers": {
			v1.Options{Prices: &stubPriceReader{}, Triangulated: failingVWAPLooker{}},
			"/v1/price?asset=" + foo + "&quote=fiat:USD",
		},
		"tip: cache unread and nothing below answers": {
			v1.Options{Prices: &stubPriceReader{}, Triangulated: failingVWAPLooker{}},
			"/v1/price/tip?asset=" + foo + "&quote=fiat:USD",
		},
	} {
		tc.opts.USDPeggedClassics = []canonical.Asset{usdc, usdt}
		ts := startHTTPTest(t, v1.New(tc.opts).Handler())
		resp := mustGet(t, ts.URL+tc.path)
		body, _ := readAll(resp)
		if resp.StatusCode < 500 {
			t.Errorf("%s: status = %d, want 5xx. Body: %s", name, resp.StatusCode, body)
		}
	}
}

// TestPriceFallback_CacheFailureDoesNotHideALowerLayer: an unread cache only matters when nothing
// below it answered.
func TestPriceFallback_CacheFailureDoesNotHideALowerLayer(t *testing.T) {
	usdc, usdt := oraclePegsForFallback(t)
	foo := "FOO-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
	reader := &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{foo + "/" + usdc.String(): {Price: "0.50", PriceType: "vwap"}}}
	ts := startHTTPTest(t, v1.New(v1.Options{
		Prices: reader, Triangulated: failingVWAPLooker{}, USDPeggedClassics: []canonical.Asset{usdc, usdt},
	}).Handler())
	resp := mustGet(t, ts.URL+"/v1/price?asset="+foo+"&quote=fiat:USD")
	body, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"0.50`) {
		t.Errorf("status = %d, want 200 with the peg price. Body: %s", resp.StatusCode, body)
	}
}

// TestPriceBatch_FallbackReadFailureIsDegraded: the row is omitted and the response is not cacheable.
func TestPriceBatch_FallbackReadFailureIsDegraded(t *testing.T) {
	usdc, usdt := oraclePegsForFallback(t)
	foo := "FOO-GA7QYNF7SOWQ3GLR2BGMZEHXAVIRZA4KVWLTJJFC7MGXUA74P7UJVSGZ"
	reader := &stubPriceReader{errByPair: map[string]error{foo + "/" + usdc.String(): errors.New("pool acquire timeout")}}
	ts := startHTTPTest(t, v1.New(v1.Options{Prices: reader, USDPeggedClassics: []canonical.Asset{usdc, usdt}}).Handler())
	resp := mustGet(t, ts.URL+"/v1/price/batch?asset_ids="+foo+"&quote=fiat:USD")
	body, _ := readAll(resp)
	if cc := resp.Header.Get("Cache-Control"); resp.StatusCode != http.StatusOK || !strings.Contains(cc, "no-store") {
		t.Errorf("status = %d Cache-Control = %q, want a 200 marked no-store. Body: %s", resp.StatusCode, cc, body)
	}
}

func oraclePegsForFallback(t *testing.T) (usdc, usdt canonical.Asset) {
	t.Helper()
	var err error
	if usdc, err = canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"); err != nil {
		t.Fatal(err)
	}
	if usdt, err = canonical.ParseAsset("USDT-GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"); err != nil {
		t.Fatal(err)
	}
	return usdc, usdt
}
