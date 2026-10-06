// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"strings"
	"testing"
)

// A trade with NULL usd_volume must never be valued at today's XLM price:
// a historical per-source figure would then move with XLM spot. Unpriced
// trades are excluded and reported so the API can mark a lower bound.
func TestSourceStatsQueryDoesNotValueAtSpot(t *testing.T) {
	q := sourceStatsQuery()
	for _, bad := range []string{"xlm_usd", "vwap", "base_amount /", "quote_amount /"} {
		if strings.Contains(q, bad) {
			t.Errorf("sourceStatsQuery must not value unpriced trades at spot; found %q:\n%s", bad, q)
		}
	}
	if !strings.Contains(q, "unpriced_trades") {
		t.Errorf("sourceStatsQuery must report the excluded unpriced trade count:\n%s", q)
	}
	assertDEXNumericSafe(t, "source stats", q)
}

func TestSourceVolumeHistoryQueryDoesNotValueAtSpot(t *testing.T) {
	q := sourceVolumeHistoryQuery()
	for _, bad := range []string{"xlm_usd", "vwap", "10000000"} {
		if strings.Contains(q, bad) {
			t.Errorf("sourceVolumeHistoryQuery must not value XLM legs at spot; found %q:\n%s", bad, q)
		}
	}
	for _, want := range []string{"sum_usd_priced", "sum_xlm_base", "sum_xlm_quote", "xlm_unpriced"} {
		if !strings.Contains(q, want) {
			t.Errorf("sourceVolumeHistoryQuery must read %q:\n%s", want, q)
		}
	}
}
