// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"strings"
	"testing"
)

// TestDex24hBucketWindowMatchesSourceVolumeHistory pins that the readers of source_volume_1h's 24h window
// select the SAME hourly buckets, or the served parity note is false. Flooring with date_trunc gives a
// >= 25-bucket window while `bucket > now() - $2::interval` gives 24.
func TestDex24hBucketWindowMatchesSourceVolumeHistory(t *testing.T) {
	kpi := dexWindowKPIQuery(1)
	series := dexActivitySeriesQuery(1)
	history := sourceVolumeHistoryQuery()

	for _, bad := range []string{"date_trunc(", ">= NOW() - $", ">= now() - $"} {
		if strings.Contains(history, bad) {
			t.Errorf("sourceVolumeHistoryQuery must not widen its window with %q — it must select the same buckets as the bespoke 24h readers:\n%s", bad, history)
		}
	}

	wantKPI := "bucket > now() - $2::interval"
	wantHistory := "bucket > now() - $1::interval"
	if !strings.Contains(kpi, wantKPI) {
		t.Errorf("dexWindowKPIQuery(1) must bind the shared strict bucket predicate %q:\n%s", wantKPI, kpi)
	}
	if !strings.Contains(series, wantKPI) {
		t.Errorf("dexActivitySeriesQuery(1) must bind the shared strict bucket predicate %q:\n%s", wantKPI, series)
	}
	if !strings.Contains(history, wantHistory) {
		t.Errorf("sourceVolumeHistoryQuery must bind the shared strict bucket predicate %q — got a different window than the bespoke 24h readers:\n%s", wantHistory, history)
	}
}
