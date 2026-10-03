// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"os"
	"strings"
	"testing"
)

// TestXLMUSDVolumeAnchorIsRobust pins the XLM/USD scalar that values
// XLM-legged volume: a median over a recent window, never the single newest
// minute, so one thin or off-market print cannot rescale every venue.
func TestXLMUSDVolumeAnchorIsRobust(t *testing.T) {
	if !strings.Contains(xlmUSDVolumeSelect, "percentile_disc(0.5)") ||
		!strings.Contains(xlmUSDVolumeSelect, "INTERVAL '15 minutes'") {
		t.Error("xlmUSDVolumeSelect must take the median vwap of a 15-minute window")
	}
	if strings.Contains(xlmUSDVolumeSelect, "LIMIT 1") {
		t.Error("xlmUSDVolumeSelect picks a single bucket")
	}
	for _, f := range []string{"markets.go", "bespoke_dex.go", "soroban_volume.go", "sources_stats.go", "mev.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		if strings.Contains(src, "xlmUSDNewest") {
			t.Errorf("%s values volume with a single newest-bucket XLM/USD pick; use xlmUSDVolumeSelect", f)
		}
		if !strings.Contains(src, "xlmUSDVolumeSelect") {
			t.Errorf("%s does not use xlmUSDVolumeSelect", f)
		}
	}
}
