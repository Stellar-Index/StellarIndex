// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package external_test

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/sources/external"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/binance"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/bitstamp"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/coinbase"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/ecb"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/exchangeratesapi"
	"github.com/Stellar-Index/StellarIndex/internal/sources/external/kraken"
)

// The registry's amount scale must match the scale each connector
// actually stamps: the USD-volume gate reads AmountScaleDecimals, so a
// drift mis-scales that source's volume by 10^Δ.
func TestAmountScaleDecimals_MatchesConnectorScale(t *testing.T) {
	const cexScale = 8 // every CEX parser's externalAmountDecimals
	for _, tc := range []struct {
		source string
		want   int
	}{
		{binance.SourceName, cexScale},
		{coinbase.SourceName, cexScale},
		{kraken.SourceName, cexScale},
		{bitstamp.SourceName, cexScale},
		{ecb.SourceName, int(ecb.DefaultDecimals)},
		{exchangeratesapi.SourceName, int(exchangeratesapi.DefaultDecimals)},
	} {
		if got := external.Lookup(tc.source).AmountScaleDecimals(); got != tc.want {
			t.Errorf("Lookup(%q).AmountScaleDecimals() = %d, want %d", tc.source, got, tc.want)
		}
	}
	if got := (external.Metadata{}).AmountScaleDecimals(); got != cexScale {
		t.Errorf("unset AmountDecimals defaults to %d, want %d", got, cexScale)
	}
}
