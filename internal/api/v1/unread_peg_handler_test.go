package v1_test

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

type failingPriceAt struct{}

func (failingPriceAt) PriceAt(context.Context, canonical.Pair, time.Time, time.Duration) (string, time.Time, int, error) {
	return "", time.Time{}, 0, errors.New("pool timeout")
}

// TestTriangulatedPrice_UnreadPegIsNotCached: a triangulated answer whose peg check failed must not reach a shared cache as "peg held".
func TestTriangulatedPrice_UnreadPegIsNotCached(t *testing.T) {
	peg, err := canonical.ParseAsset("USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/price?asset=crypto:XLM&quote=fiat:EUR", "/v1/price/tip?asset=crypto:XLM&quote=fiat:EUR"} {
		for _, peggedReadFails := range []bool{true, false} {
			opts := v1.Options{
				Prices:            &stubPriceReader{err: v1.ErrPriceNotFound},
				Triangulated:      &stubTriangulatedPriceLooker{value: "0.5500", isTriangulated: true, found: true},
				USDPeggedClassics: []canonical.Asset{peg},
			}
			if peggedReadFails {
				opts.PriceAt = failingPriceAt{}
			}
			resp := mustGet(t, startHTTPTest(t, v1.New(opts).Handler()).URL+path)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s: status = %d, want 200", path, resp.StatusCode)
			}
			if got := resp.Header.Get("Cache-Control") == "no-store"; got != peggedReadFails {
				t.Errorf("%s peg read failed=%v: Cache-Control = %q", path, peggedReadFails, resp.Header.Get("Cache-Control"))
			}
		}
	}
}
