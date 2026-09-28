// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestUsdVolumeTotalViolations_CountsParseErrs pins #1093: a group
// ClassifyUSDVolumeTier can't parse was printed as "UNCLASSIFIABLE" but
// never added to the violation count, so a day with nothing but
// unclassifiable groups exited 0 — a mis-spelled asset id on a landed
// trade left the judged population silently.
func TestUsdVolumeTotalViolations_CountsParseErrs(t *testing.T) {
	got := usdVolumeTotalViolations(0, 0, 3)
	if got != 3 {
		t.Fatalf("usdVolumeTotalViolations(0, 0, 3) = %d, want 3 — parseErrs must count as violations (#1093)", got)
	}
	if want := 5; usdVolumeTotalViolations(2, 1, 2) != want {
		t.Errorf("usdVolumeTotalViolations(2, 1, 2) = %d, want %d", usdVolumeTotalViolations(2, 1, 2), want)
	}
}

// TestClassifyExactTierGroups_UnclassifiableCountsAsParseErr exercises the
// classify loop directly: a group whose base asset id ClassifyUSDVolumeTier
// rejects must be surfaced in parseErrs even though it never reaches the
// exact-tier judgement (TierEstimated on an error is not .Exact()).
func TestClassifyExactTierGroups_UnclassifiableCountsAsParseErr(t *testing.T) {
	spec, err := timescale.NewUSDVolumeQuoteSpec(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	groups := []timescale.TradeValuationGroup{
		{
			Source:        "sdex",
			BaseAsset:     "", // canonical.ParseAsset rejects an empty id
			QuoteAsset:    "native",
			PricedRows:    10,
			SumUSDVolume:  "100",
			SumBaseAmount: "1000",
		},
	}
	violations, judged, parseErrs, _ := classifyExactTierGroups(groups, spec, 1, 20)
	if parseErrs != 1 {
		t.Errorf("parseErrs = %d, want 1", parseErrs)
	}
	if judged != 0 {
		t.Errorf("judged = %d, want 0 — an unclassifiable group is not exact-tier", judged)
	}
	if violations != 0 {
		t.Errorf("violations = %d, want 0 from the classify pass alone — the caller folds parseErrs in (#1093)", violations)
	}
}
