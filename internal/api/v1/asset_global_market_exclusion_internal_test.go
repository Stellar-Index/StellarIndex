package v1

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type globalExclTable struct {
	rows []timescale.SourceExclusion
	err  error
}

func (e globalExclTable) UnliftedSourceExclusions(context.Context) ([]timescale.SourceExclusion, error) {
	return e.rows, e.err
}

// TestGlobalMarket_SourceExclusion drives the real detail fill: a reference from an excluded source (source-wide or scoped to the
// local asset) or read while the exclusions are unreadable neither fills price_usd nor is shown, and a direct price withheld as
// source_excluded is never replaced by a clean reference.
func TestGlobalMarket_SourceExclusion(t *testing.T) {
	from := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name     string
		table    globalExclTable
		withheld PriceWithheldReason
		wantFill bool
	}{
		{name: "no exclusions", wantFill: true},
		{name: "another source excluded", table: globalExclTable{rows: []timescale.SourceExclusion{{ID: 1, Source: "kraken", From: from}}}, wantFill: true},
		{name: "reference source excluded", table: globalExclTable{rows: []timescale.SourceExclusion{{ID: 1, Source: "coingecko", From: from}}}},
		{name: "reference source excluded on the local pair", table: globalExclTable{rows: []timescale.SourceExclusion{{
			ID: 1, Source: "coingecko", Base: globalTestUSDC, Quote: "fiat:USD", From: from,
		}}}},
		{name: "exclusions unreadable", table: globalExclTable{err: errors.New("db down")}},
		{name: "direct price withheld as source_excluded", withheld: PriceWithheldSourceExcluded},
	} {
		s := globalMarketServer(t, refUpdate(t, "coingecko", "crypto:USDC", "fiat:USD", "97000000", 8, time.Now()))
		s.Exclusions = pricingguard.NewSourceExclusionGate(tc.table, pricingguard.SourceExclusionGateOptions{})
		d := AssetDetail{AssetID: globalTestUSDC, PriceWithheldReason: tc.withheld}
		s.fillDeclaredPegPrice(context.Background(), &d, nil)
		if filled := d.PriceBasis == priceBasisGlobalMarket; filled != tc.wantFill {
			t.Errorf("%s: global-market fill = %v (price_usd=%s), want %v", tc.name, filled, strPtr(d.PriceUSD), tc.wantFill)
		}
		// Only the declared peg (a configured declaration, labelled declared_peg) may stand in after the global step.
		if tc.withheld != "" && d.PriceUSD != nil && d.PriceBasis != priceBasisDeclaredPeg {
			t.Errorf("%s: price_usd=%s basis %q replaced the %q withholding", tc.name, strPtr(d.PriceUSD), d.PriceBasis, tc.withheld)
		}
		if !tc.wantFill && tc.withheld == "" && d.GlobalMarket != nil {
			t.Errorf("%s: an excluded or unjudged reference was shown: %+v", tc.name, d.GlobalMarket)
		}
	}
}
