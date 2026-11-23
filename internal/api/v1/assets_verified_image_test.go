package v1_test

import (
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestAssetsVerified_ImageIsHTTPSOnly: the verified listing promises https-only logos, so an issuer TOML's http:// image is
// dropped while an https:// one is served.
func TestAssetsVerified_ImageIsHTTPSOnly(t *testing.T) {
	for image, want := range map[string]string{
		"https://circle.com/usdc-logo.svg": "https://circle.com/usdc-logo.svg",
		"http://circle.com/usdc-logo.svg":  "",
	} {
		sep1 := &stubSep1Cache{byIssuer: map[string]*timescale.IssuerSep1Cached{
			testUSDCIssuer: {Currencies: []timescale.IssuerSep1Currency{{Code: "USDC", Issuer: testUSDCIssuer, Image: image}}},
		}}
		srv := v1.New(v1.Options{VerifiedCurrencies: newTestCatalogue(t), Sep1Cache: sep1})
		var rows []v1.VerifiedCurrencyListItem
		globalExclGet(t, srv, "/v1/assets/verified", &rows)
		found := false
		for _, r := range rows {
			if r.Issuer == testUSDCIssuer && r.AssetID == "USDC-"+testUSDCIssuer {
				found = true
				if r.Image != want {
					t.Errorf("TOML image %q: listing image = %q, want %q", image, r.Image, want)
				}
			}
		}
		if !found {
			t.Fatalf("verified listing has no USDC-%s row", testUSDCIssuer)
		}
	}
}
