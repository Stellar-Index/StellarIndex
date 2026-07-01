// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package currency

import "testing"

func hasTicker(entries []*VerifiedCurrency, ticker string) bool {
	for _, vc := range entries {
		if vc.Ticker == ticker {
			return true
		}
	}
	return false
}

// TestCatalogue_StellarExternalSplit pins the LC-001 predicate: StellarIssued()
// (→ /v1/assets) and External() (→ /v1/external/assets) partition the catalogue
// by "has a Stellar on-chain issuance". Fiat + reference-only coins are external.
func TestCatalogue_StellarExternalSplit(t *testing.T) {
	cat, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("LoadEmbedded: %v", err)
	}
	stellar := cat.StellarIssued()
	external := cat.External()

	// Exact partition of the whole catalogue.
	if got := len(stellar) + len(external); got != len(cat.All()) {
		t.Fatalf("not a partition: %d stellar + %d external = %d, want %d",
			len(stellar), len(external), got, len(cat.All()))
	}
	for _, vc := range stellar {
		if vc.StellarEntry() == nil {
			t.Errorf("StellarIssued includes %q which has no Stellar entry", vc.Ticker)
		}
	}
	for _, vc := range external {
		if vc.StellarEntry() != nil {
			t.Errorf("External includes %q which HAS a Stellar entry", vc.Ticker)
		}
	}

	// Reference-only coins are external, Stellar stablecoins are not.
	if !hasTicker(external, "BTC") {
		t.Error("BTC (reference-only) must be in External()")
	}
	if hasTicker(stellar, "BTC") {
		t.Error("BTC must NOT be in StellarIssued()")
	}
	if !hasTicker(stellar, "USDC") {
		t.Error("USDC (Stellar stablecoin) must be in StellarIssued()")
	}
	if hasTicker(external, "USDC") {
		t.Error("USDC must NOT be in External()")
	}
}
