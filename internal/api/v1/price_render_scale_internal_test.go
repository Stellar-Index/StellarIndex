// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"math/big"
	"testing"
	"time"
)

func tinyRat(num int64, places int64) *big.Rat {
	return new(big.Rat).SetFrac(big.NewInt(num), new(big.Int).Exp(big.NewInt(10), big.NewInt(places), nil))
}

// The derived fiat-cross chart leg: a tiny cross rate must survive.
func TestCrossFiatChartPoints_tinyCrossRateSurvives(t *testing.T) {
	d := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	base := []FXQuotePoint{{Bucket: d, RateUSDText: "4000000000000", InverseUSDText: "0.00000000000025"}}
	quote := []FXQuotePoint{{Bucket: d, RateUSDText: "2", InverseUSDText: "0.5"}}
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
	base := []FXQuotePoint{{Bucket: d, RateUSDText: "0.1", InverseUSDText: "10"}}
	quote := []FXQuotePoint{{Bucket: d, RateUSDText: "0.3", InverseUSDText: "3.33333333333333333333"}}
	got := crossFiatChartPoints(base, quote)
	if len(got) != 1 {
		t.Fatalf("got %d points, want 1", len(got))
	}
	if want := "3.0000000000"; got[0].P != want {
		t.Fatalf("cross P = %q, want %q", got[0].P, want)
	}
}
