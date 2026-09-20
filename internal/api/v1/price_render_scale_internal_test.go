// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

func tinyRat(num int64, places int64) *big.Rat {
	return new(big.Rat).SetFrac(big.NewInt(num), new(big.Int).Exp(big.NewInt(10), big.NewInt(places), nil))
}

// The wire price renderers floor at ohlcPriceDigits. A strictly positive
// price whose first significant digit lies beyond that place must NOT
// render as an all-zero string (which reparses as price 0); the scale is
// extended magnitude-relatively, exactly as the aggregator's own
// formatRatFixed does. Normal-magnitude prices stay byte-identical.
func TestRatToDecimal_extendsScaleForSubDigitPrices(t *testing.T) {
	cases := []struct {
		name string
		r    *big.Rat
		want string
	}{
		{"normal magnitude unchanged", big.NewRat(1, 3), "0.3333333333"},
		{"last rendered place unchanged", tinyRat(1, 10), "0.0000000001"},
		{"zero unchanged", new(big.Rat), "0.0000000000"},
		{"3e-12 keeps its digits", tinyRat(3, 12), "0.000000000003000000000000"},
		{"negative tiny keeps sign and digits", tinyRat(-3, 12), "-0.000000000003000000000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ratToDecimal(tc.r, ohlcPriceDigits)
			if got != tc.want {
				t.Fatalf("ratToDecimal(%s, %d) = %q, want %q", tc.r.RatString(), ohlcPriceDigits, got, tc.want)
			}
			back, ok := new(big.Rat).SetString(got)
			if !ok || back.Sign() != tc.r.Sign() {
				t.Fatalf("%q reparses with sign %d, want %d", got, back.Sign(), tc.r.Sign())
			}
		})
	}
}

// /v1/history renders price via priceRatioDecimal: same floor, same fix.
func TestPriceRatioDecimal_extendsScaleForSubDigitPrices(t *testing.T) {
	tr := canonical.Trade{
		BaseAmount:  canonical.NewAmount(new(big.Int).Exp(big.NewInt(10), big.NewInt(15), nil)),
		QuoteAmount: canonical.NewAmount(big.NewInt(3)), // 3e-15 quote per base
	}
	if got, want := priceRatioDecimal(tr, ohlcPriceDigits), "0.000000000000003000000000000"; got != want {
		t.Fatalf("priceRatioDecimal = %q, want %q", got, want)
	}
}

// The derived fiat-cross chart leg: a tiny cross rate must survive.
func TestCrossFiatChartPoints_tinyCrossRateSurvives(t *testing.T) {
	d := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	base := []FXQuotePoint{{Bucket: d, RateUSD: 4e12, InverseUSD: 2.5e-13}}
	quote := []FXQuotePoint{{Bucket: d, RateUSD: 2, InverseUSD: 0.5}}
	got := crossFiatChartPoints(base, quote)
	if len(got) != 1 {
		t.Fatalf("got %d points, want 1", len(got))
	}
	if want := "0.0000000000005000000000000"; got[0].P != want {
		t.Fatalf("cross P = %q, want %q", got[0].P, want)
	}
}

// The cross leg must recover the decimal the NUMERIC column held, not
// the float's binary expansion: 0.3/0.1 is exactly 3, not 2.9999….
func TestCrossFiatChartPoints_recoversDecimalRates(t *testing.T) {
	d := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	base := []FXQuotePoint{{Bucket: d, RateUSD: 0.1, InverseUSD: 10}}
	quote := []FXQuotePoint{{Bucket: d, RateUSD: 0.3, InverseUSD: 1 / 0.3}}
	got := crossFiatChartPoints(base, quote)
	if len(got) != 1 {
		t.Fatalf("got %d points, want 1", len(got))
	}
	if want := "3.0000000000"; got[0].P != want {
		t.Fatalf("cross P = %q, want %q", got[0].P, want)
	}
}
