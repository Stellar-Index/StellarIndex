// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A contradicted scale withholds the price on the contract arm whichever read settles it: when the listing's batch read fails,
// the arm's own single read finds the contradiction after the listing's price gate ran, and must withhold the price itself.
func TestRWAContractListingRows_LateScaleContradictionWithholdsPrice(t *testing.T) {
	const id = "CC2RBGYNCFBCVENIDL5BFBWPH4OUZM2UA3OD2K2N54GLMWCC4KWPVAGO"
	price, volume := "1.0000000000", "250000.00"
	sources := 3
	row := timescale.AssetRow{AssetID: id, PriceUSD: &price, Volume24hUSD: &volume, SourceCount: &sources}
	for _, tc := range []struct {
		name     string
		batchErr error
	}{
		{"batch read answers", nil},
		{"batch read fails, single read answers", context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{
				Options: Options{
					ContractCatalogue: capDecimalsContractCatalogue{rows: map[string]timescale.AssetRow{id: row}},
					TokenSupply:       capDecimalsTokenSupply{byID: map[string]string{id: "10000000000"}},
					TokenDecimals:     &countingScaleReader{declared: map[string]uint32{id: 6}, batchErr: tc.batchErr},
				},
				logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
			}
			out, _, err := s.rwaContractListingRows(t.Context(), []rwaContractMember{{contractID: id}})
			if err != nil {
				t.Fatal(err)
			}
			got := out[id]
			if got.Decimals != 6 {
				t.Fatalf("decimals = %d, want the read 6 (fixture no longer reaches the contradiction)", got.Decimals)
			}
			if got.PriceUSD != nil || got.PriceWithheldReason != PriceWithheldDecimalsMismatch {
				t.Errorf("price_usd = %v withheld = %q, want withheld %q", ptrStr(got.PriceUSD), got.PriceWithheldReason, PriceWithheldDecimalsMismatch)
			}
			if v := rwaValuationOf(got); v.PriceUSD != nil || v.MarketCapUSD != nil {
				t.Errorf("valuation = %+v, want no price and no cap", v)
			}
		})
	}
}

func ptrStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}
