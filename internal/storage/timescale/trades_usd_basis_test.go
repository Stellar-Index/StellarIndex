package timescale

import (
	"context"
	"testing"
)

// TestTradeUSDVolumeCredited_BasisNamesTheStoredLeg: every route records the leg and rate it stored, including the bounded routes
// where the cross-checked leg wins, and the basis is empty exactly when the value is NULL.
func TestTradeUSDVolumeCredited_BasisNamesTheStoredLeg(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	spec := fiatPegSpec(t)
	eur := map[string]string{"fiat:EUR": "1.08", eurcKey: "1.075"}
	withAqua := func(m map[string]string) map[string]string {
		out := map[string]string{aquaID: "0.1"}
		for k, v := range m {
			out[k] = v
		}
		return out
	}
	cases := []struct {
		name   string
		source string
		base   string
		quote  string
		amts   [2]int64
		prices map[string]string
		want   USDBasis
	}{
		{"pegged quote", "sdex", aquaID, circleKey, [2]int64{1e7, 2e7}, nil, BasisQuotePegged},
		{"pegged base", "sdex", circleKey, aquaID, [2]int64{2e7, 1e7}, nil, BasisBasePegged},
		{"XLM base", "soroswap", "native", aquaID, [2]int64{10e7, 5e7}, map[string]string{"native": "0.1"}, BasisXLMBase},
		{"XLM quote", "soroswap", aquaID, "native", [2]int64{5e7, 10e7}, map[string]string{"native": "0.1"}, BasisXLMQuote},
		{"fiat-pegged quote", "sdex", aquaID, eurcKey, [2]int64{10e7, 25e7}, eur, BasisFiatPeg},
		{"fiat peg bounded to the base leg", "sdex", aquaID, eurcKey, [2]int64{10e7, 25e7}, withAqua(eur), BasisFXBase},
		{"fiat peg on the base bounded to the quote leg", "sdex", eurcKey, aquaID, [2]int64{25e7, 10e7}, withAqua(eur), BasisFXQuote},
		{"DEX quote at its market", "soroswap", sep41AID, aquaID, [2]int64{1e7, 100e7}, withAqua(nil), BasisFXQuote},
		{"DEX quote bounded to the base leg", "soroswap", sep41AID, aquaID, [2]int64{1e7, 100e7}, withAqua(map[string]string{sep41AID: "0.00001"}), BasisFXBase},
		{"off-chain fiat quote", "binance", "crypto:XLM", "fiat:EUR", [2]int64{1e8, 1e8}, eur, BasisFXQuote},
		{"tier 4 base anchor", "soroswap", aquaID, sep41BID, [2]int64{100e7, 1e7}, withAqua(nil), BasisFXBase},
		{"unvaluable", "soroswap", aquaID, sep41BID, [2]int64{100e7, 1e7}, map[string]string{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := volTrade(t, tc.source, mustAsset(t, tc.base), mustAsset(t, tc.quote), tc.amts[0], tc.amts[1])
			var fx USDVolumeFXResolver
			if tc.prices != nil {
				fx = stubFXResolver{prices: tc.prices}
			}
			v, basis, _, err := tradeUSDVolumeCredited(ctx, tr, spec, fx, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			if basis != tc.want {
				t.Fatalf("basis = %q, want %q (value %v)", basis, tc.want, v)
			}
			if (v == nil) != (basis == "") {
				t.Fatalf("value %v with basis %q: NULL must pair with an empty basis", v, basis)
			}
		})
	}
}
