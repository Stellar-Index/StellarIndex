package v1

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/currency"
)

// The catalogue loader refuses two entries sharing a code, so the
// listing's (code, issuer) keying is exercised at the projection.
func TestProjectVerifiedCurrencyList_SameCodeDistinctIssuers(t *testing.T) {
	const (
		issuerA = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
		issuerB = "GCQTGZQQ5G4PTM2GL7CDIFKUBIPEC52BROAQIAPW53XBRJVN6ZJVTG6V"
	)
	stellar := func(code, issuer, assetID string) []currency.IssuanceEntry {
		return []currency.IssuanceEntry{{Network: "stellar", Code: code, Issuer: issuer, AssetID: assetID}}
	}
	entries := []*currency.VerifiedCurrency{
		{Ticker: "USDC", Slug: "usdc-a", Name: "A", Class: currency.ClassStablecoin, Issuance: stellar("USDC", issuerA, "USDC-"+issuerA)},
		{Ticker: "USDC", Slug: "usdc-b", Name: "B", Class: currency.ClassStablecoin, Issuance: stellar("USDC", issuerB, "USDC-"+issuerB)},
		{Ticker: "XLM", Slug: "xlm", Name: "Stellar Lumens", Class: currency.ClassCrypto, Issuance: stellar("", "", "native")},
		{Ticker: "EUR", Slug: "eur", Name: "Euro", Class: currency.ClassFiat},
	}

	out := projectVerifiedCurrencyList(entries)

	want := []struct{ assetID, issuer string }{
		{"USDC-" + issuerA, issuerA},
		{"USDC-" + issuerB, issuerB},
		{"native", ""},
		{"", ""},
	}
	for i, w := range want {
		if out[i].AssetID != w.assetID || out[i].Issuer != w.issuer {
			t.Errorf("row %d (%s): asset_id=%q issuer=%q, want %q %q",
				i, out[i].Slug, out[i].AssetID, out[i].Issuer, w.assetID, w.issuer)
		}
	}
	if out[0].AssetID == out[1].AssetID {
		t.Fatalf("same-code rows collapsed to one asset_id %q", out[0].AssetID)
	}
}
