// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
)

// mustEmbeddedCatalogue is the verified catalogue that binds a declared peg to its observed market.
func mustEmbeddedCatalogue(t *testing.T) *currency.Catalogue {
	t.Helper()
	cat, err := currency.LoadEmbedded()
	if err != nil {
		t.Fatalf("currency.LoadEmbedded: %v", err)
	}
	return cat
}

func usdQuote(t *testing.T) canonical.Asset {
	t.Helper()
	usd, err := canonical.NewFiatAsset("USD")
	if err != nil {
		t.Fatal(err)
	}
	return usd
}
