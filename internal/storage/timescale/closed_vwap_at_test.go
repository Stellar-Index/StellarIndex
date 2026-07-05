// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestPriceAtResolutionLadder pins the finest-first CAGG ladder used
// by ClosedVWAPAtOrBefore: recent instants probe prices_1m first;
// week-old instants start at prices_1h; year-old instants read
// prices_1d only. Every ladder is ordered finest→coarsest so the
// first in-window hit is the finest resolution that has the data.
func TestPriceAtResolutionLadder(t *testing.T) {
	cases := []struct {
		name string
		age  time.Duration
		want []HistoryGranularity
	}{
		{"now", 0, []HistoryGranularity{Granularity1m, Granularity15m, Granularity1h, Granularity4h, Granularity1d}},
		{"1h-ago", time.Hour, []HistoryGranularity{Granularity1m, Granularity15m, Granularity1h, Granularity4h, Granularity1d}},
		{"36h-ago", 36 * time.Hour, []HistoryGranularity{Granularity1m, Granularity15m, Granularity1h, Granularity4h, Granularity1d}},
		{"7d-ago", 7 * 24 * time.Hour, []HistoryGranularity{Granularity1h, Granularity4h, Granularity1d}},
		{"30d-ago", 30 * 24 * time.Hour, []HistoryGranularity{Granularity1h, Granularity4h, Granularity1d}},
		{"1y-ago", 365 * 24 * time.Hour, []HistoryGranularity{Granularity1d}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := priceAtResolutionLadder(tc.age)
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("priceAtResolutionLadder(%v) = %v, want %v", tc.age, got, tc.want)
			}
			// Ladder MUST be strictly finest→coarsest (monotone
			// non-decreasing bucket width) so the first in-window hit
			// is the finest available resolution.
			for i := 1; i < len(got); i++ {
				if got[i].BucketDuration() <= got[i-1].BucketDuration() {
					t.Fatalf("ladder not finest-first: %v", got)
				}
			}
		})
	}
}

// TestHistoryGranularityBucketDuration pins the wall-clock width used
// for staleness math + window_seconds labeling.
func TestHistoryGranularityBucketDuration(t *testing.T) {
	cases := []struct {
		g        HistoryGranularity
		wantDur  time.Duration
		wantSecs int
	}{
		{Granularity1m, time.Minute, 60},
		{Granularity15m, 15 * time.Minute, 900},
		{Granularity1h, time.Hour, 3600},
		{Granularity4h, 4 * time.Hour, 14400},
		{Granularity1d, 24 * time.Hour, 86400},
		{Granularity1w, 7 * 24 * time.Hour, 604800},
		{Granularity1mo, 30 * 24 * time.Hour, 2592000},
	}
	for _, tc := range cases {
		if got := tc.g.BucketDuration(); got != tc.wantDur {
			t.Errorf("%s.BucketDuration() = %v, want %v", tc.g, got, tc.wantDur)
		}
		if got := tc.g.Seconds(); got != tc.wantSecs {
			t.Errorf("%s.Seconds() = %d, want %d", tc.g, got, tc.wantSecs)
		}
	}
}

// TestClosedVWAPAtOrBeforeQueryShape guards the sargability + both-
// directions invariants of the point-in-time query WITHOUT a database.
// A regression to the non-sargable `bucket + INTERVAL <= …` form (the
// 2026-06-20 latency-burn shape) or dropping a stored direction would
// silently degrade the endpoint; this turns either into a test
// failure.
func TestClosedVWAPAtOrBeforeQueryShape(t *testing.T) {
	q := fmt.Sprintf(closedVWAPAtOrBeforeQueryTemplate,
		"prices_1m", "2024-01-01 00:00:00+00", "2023-12-01 00:00:00+00")

	// Sargable closed-bucket guard: the interval lives on the RHS as a
	// literal bound, NEVER as a function on the indexed bucket column.
	if !strings.Contains(q, "bucket <= TIMESTAMPTZ") {
		t.Error("query missing sargable `bucket <= TIMESTAMPTZ` upper bound")
	}
	if strings.Contains(q, "bucket + INTERVAL") {
		t.Error("query uses non-sargable `bucket + INTERVAL` form (function on indexed column)")
	}
	// Both stored directions of the market are read.
	if !strings.Contains(q, "base_asset = $1 AND quote_asset = $2") ||
		!strings.Contains(q, "base_asset = $2 AND quote_asset = $1") {
		t.Error("query does not combine both stored directions of the pair")
	}
	// Flipped rows invert the vwap so the answer expresses base-in-quote.
	if !strings.Contains(q, "1.0 / NULLIF(vwap, 0)") {
		t.Error("query missing flipped-direction vwap inversion")
	}
}
