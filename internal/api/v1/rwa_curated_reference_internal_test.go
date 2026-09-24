// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// A curated row's reference is a curator's own uploaded price, not a
// listing platform's aggregate — so its premium refusal must say that
// and not the listing-price wording rwaApplyListingReference sets as
// its default. Reference.Provenance already carries the curator-price
// value; Premium.Status must carry a matching one rather than the
// generic listing refusal the shared helper leaves behind.
func TestRWAApplyCuratorReference_PremiumStatusMatchesCuratorProvenance(t *testing.T) {
	now := time.Now().UTC()
	supply := "100000000000"
	a := &RWAAsset{
		AssetID:           "CURATED-ADDR",
		CirculatingSupply: &supply,
		Decimals:          7,
	}
	entry := timescale.CuratedRWAEntry{
		Address:  "CURATED-ADDR",
		Company:  "Example Fund",
		PriceUSD: "1.02",
		PricedAt: now.Add(-time.Hour),
		Source:   "curator upload",
	}

	rwaApplyCuratorReference(a, entry, now)

	if a.Reference == nil || a.Reference.Provenance != RWAReferenceCuratorPrice {
		t.Fatalf("reference = %+v, want provenance %q", a.Reference, RWAReferenceCuratorPrice)
	}
	if a.Premium.Status != RWAPremiumReferenceNotOracleCurator {
		t.Errorf("premium.status = %q, want %q (the curator-price refusal, matching reference.provenance)",
			a.Premium.Status, RWAPremiumReferenceNotOracleCurator)
	}
	if a.Premium.Status == RWAPremiumReferenceNotOracle {
		t.Errorf("premium.status = %q — the listing-price refusal, wrongly claiming this reference is a listing price", a.Premium.Status)
	}
}
