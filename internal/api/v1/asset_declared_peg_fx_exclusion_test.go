package v1_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// pegListingRows lists FOO with no market price, so the declared-peg fill prices it.
type pegListingRows struct{ stubAssetsReaderExt }

func (pegListingRows) ListAssetsExt(context.Context, timescale.ListAssetsOptions) ([]timescale.AssetRow, error) {
	return []timescale.AssetRow{{AssetID: exclFOO, Code: "FOO", IssuerGStrkey: strings.SplitN(exclFOO, "-", 2)[1], ObservationCount: 10}}, nil
}

// TestDeclaredPegFX_ExclusionJudgedOnTheWayOut: a classic declared pegged to EUR is priced from the EUR/USD rate. An exclusion
// of the vendor that published it withholds the price with its reason on an uncacheable body; lifting it serves the price on
// the very next request, so no cache holds the judged body; an unreadable set withholds; an empty set changes nothing.
func TestDeclaredPegFX_ExclusionJudgedOnTheWayOut(t *testing.T) {
	eur, err := canonical.NewFiatAsset("EUR")
	if err != nil {
		t.Fatal(err)
	}
	fx := &stubFXHistoryReader{points: []v1.FXQuotePoint{
		{Bucket: time.Now().UTC().Add(-24 * time.Hour), InverseUSDText: "1.17", Source: "ecb"},
	}}
	foo := v1.AssetDetail{AssetID: exclFOO, Type: "classic", Code: "FOO", Decimals: 7, Sep1Status: "not_applicable"}
	opts := func(g v1.PriceExclusionGate) v1.Options {
		return v1.Options{
			Assets:             &stubAssetReader{byID: map[string]v1.AssetDetail{exclFOO: foo}},
			AssetsReader:       &pegListingRows{},
			FXHistory:          fx,
			FiatPeggedClassics: map[string]canonical.Asset{exclFOO: eur},
			Exclusions:         g,
		}
	}
	ecbExcluded := assetExclGate(exclTable{rows: []timescale.SourceExclusion{{
		ID: 1, Source: "ecb", Base: "fiat:EUR", Quote: "fiat:USD", From: time.Now().Add(-72 * time.Hour),
	}}})

	for _, path := range []string{"/v1/assets", "/v1/assets/" + exclFOO} {
		t.Run(path, func(t *testing.T) {
			_, plain := exclGet(t, v1.New(opts(nil)), path)
			plain = exclAsOf.ReplaceAllString(plain, `"as_of":""`)
			if !strings.Contains(plain, `"price_usd":"1.17"`) || !strings.Contains(plain, `"price_basis":"declared_peg"`) {
				t.Fatalf("no gate: the peg fill did not run: %s", plain)
			}
			gate := &swapGate{g: ecbExcluded}
			ts := startHTTPTest(t, v1.New(opts(gate)).Handler())
			get := func() (string, string) {
				resp := mustGet(t, ts.URL+path)
				body, _ := readAll(resp)
				return exclAsOf.ReplaceAllString(body, `"as_of":""`), resp.Header.Get("Cache-Control")
			}

			body, cc := get()
			if strings.Contains(body, `"price_usd":"`) || !strings.Contains(body, `"price_withheld_reason":"source_excluded"`) ||
				!strings.Contains(body, `"fills_withheld_reason":"source_excluded"`) || cc != "no-store" {
				t.Errorf("ecb excluded: Cache-Control %q, body %s; want price withheld as source_excluded, no-store", cc, body)
			}

			gate.set(assetExclGate(exclTable{}))
			if body, _ = get(); body != plain {
				t.Errorf("exclusion lifted: the peg price is not served again:\n got %s\nwant %s", body, plain)
			}

			gate.set(assetExclGate(exclTable{err: errors.New("db down")}))
			body, cc = get()
			if strings.Contains(body, `"price_usd":"`) || !strings.Contains(body, `"price_withheld_reason":"unattributed"`) ||
				!strings.Contains(body, `"stale":true`) || cc != "no-store" {
				t.Errorf("exclusions unread: Cache-Control %q, body %s; want withheld as unattributed, stale, no-store", cc, body)
			}
		})
	}
}
