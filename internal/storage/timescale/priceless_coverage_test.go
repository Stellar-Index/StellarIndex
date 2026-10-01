// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"strings"
	"testing"
)

// TestPricelessPriced_IncludesServedSnapshot pins the served-price arm into
// the tripwire's priced set, in both the sweep and the single-asset probe,
// with the listing's own staleness bound: an asset /v1/assets serves a
// price for must never page as priceless.
func TestPricelessPriced_IncludesServedSnapshot(t *testing.T) {
	t.Parallel()

	bound := "computed_at > now() - INTERVAL '" + assetPriceSnapshotMaxAge + "'"
	if !strings.Contains(pricelessServedArm, "FROM asset_price_snapshot") ||
		!strings.Contains(pricelessServedArm, bound) {
		t.Fatalf("served arm must read asset_price_snapshot bounded by %q, got:\n%s", bound, pricelessServedArm)
	}
	pricedIdx := strings.Index(pricelessPricedCTEs, "priced AS (")
	armIdx := strings.Index(pricelessPricedCTEs, pricelessServedArm)
	if pricedIdx < 0 || armIdx < pricedIdx {
		t.Errorf("served arm must be a UNION arm of the final priced CTE")
	}
	for name, sql := range map[string]string{
		"popularPricelessCandidatesSQL": popularPricelessCandidatesSQL,
		"assetIsPricedSQL":              assetIsPricedSQL,
	} {
		if !strings.Contains(sql, pricelessServedArm) {
			t.Errorf("%s lacks the served-snapshot arm — the sweep and the probe would disagree on priced", name)
		}
	}
}
