package v1

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/aggregate"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestCatalogueListing_CarriesTheGlobalWithholding: a catalogue row whose global tier was refused for an excluded source says
// so, as /v1/assets/{slug} does, instead of reading as "no price exists"; the twin's change pills do not sit beside it.
func TestCatalogueListing_CarriesTheGlobalWithholding(t *testing.T) {
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatal(err)
	}
	listing := "0.4321"
	s := New(Options{
		VerifiedCurrencies: cat,
		GlobalPrice:        newWithheldTierReader(&aggregate.TierWithheldError{}),
		AssetsReader:       onChainPriceReader{row: timescale.AssetRow{PriceUSD: &listing}},
	})
	page := []AssetDetail{{Slug: "usdc", Code: "USDC", AssetID: "usdc"}}
	s.fillCataloguePricesForPage(context.Background(), page)
	if page[0].PriceUSD != nil || page[0].PriceWithheldReason != PriceWithheldSourceExcluded {
		t.Fatalf("price_usd = %s reason = %q, want null with source_excluded", strOrEmpty(page[0].PriceUSD), page[0].PriceWithheldReason)
	}
	pct := "4.2"
	mergeTwinStats(&page[0], AssetDetail{Change1hPct: &pct, Change24hPct: &pct, Change7dPct: &pct})
	if page[0].Change1hPct != nil || page[0].Change24hPct != nil || page[0].Change7dPct != nil {
		t.Error("a change pill derived from the twin's price was served beside a withheld row price")
	}
}
