// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// declaredPegOnlyServer prices a declared USD peg with no market anywhere, so the only USD price
// is the operator's 1:1 declaration.
func declaredPegOnlyServer(t *testing.T) (*Server, canonical.Asset) {
	t.Helper()
	peg := mustAsset(t, withheldTestPegIssuer)
	return &Server{Options: Options{
		Prices:            withheldUSDReader{},
		USDPeggedClassics: []canonical.Asset{peg},
		Change24h:         stubChange24h{then: "0.90"},
	}}, peg
}

// TestPopulatePriceUSD_DeclarationCarriesItsBasis: the declaration is served labelled, so every
// guard keyed on price_basis withholds the cap, FDV and history a market price would earn.
func TestPopulatePriceUSD_DeclarationCarriesItsBasis(t *testing.T) {
	s, peg := declaredPegOnlyServer(t)
	var detail AssetDetail
	s.populatePriceUSD(context.Background(), &detail, peg)
	if detail.PriceUSD == nil || *detail.PriceUSD != declaredPegPrice {
		t.Fatalf("price_usd = %v, want the declaration %s", detail.PriceUSD, declaredPegPrice)
	}
	if detail.PriceBasis != priceBasisDeclaredPeg {
		t.Fatalf("price_basis = %q, want %q", detail.PriceBasis, priceBasisDeclaredPeg)
	}
	if r := capRefusedOnPriceBasis(&detail); r != MarketCapWithheldPriceBasis {
		t.Fatalf("cap refusal = %q, want %q", r, MarketCapWithheldPriceBasis)
	}
}

// TestPopulateChange24h_DeclarationHasNoChange: a declared 1.0 compared with a market bucket
// would report a move nobody observed.
func TestPopulateChange24h_DeclarationHasNoChange(t *testing.T) {
	s, peg := declaredPegOnlyServer(t)
	var detail AssetDetail
	if failed := s.populateChange24h(context.Background(), &detail, peg); failed {
		t.Fatal("change read reported failed")
	}
	if detail.Change24hPct != nil {
		t.Fatalf("change_24h_pct = %q on a declared price, want absent", *detail.Change24hPct)
	}
}

// TestReservePriceUSD_TakesNoDeclaration: the lending page values reserves on observed prices
// only, so a declared peg with no market leaves the reserve unpriced.
func TestReservePriceUSD_TakesNoDeclaration(t *testing.T) {
	s, peg := declaredPegOnlyServer(t)
	if got := s.reservePriceUSD(context.Background(), peg.String()); got.price != "" {
		t.Fatalf("reserve price = %q (basis %q), want none from a declaration", got.price, got.basis)
	}
}
