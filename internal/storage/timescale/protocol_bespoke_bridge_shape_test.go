// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import (
	"strings"
	"testing"
)

// TestBridgeSeriesGrain guards the hourly-vs-daily bucketing rule for the
// bridge flow series: the 24h window (windowDays == 1) MUST bucket by hour —
// a daily bucket collapses it to a single useless point — and every longer
// window stays daily.
func TestBridgeSeriesGrain(t *testing.T) {
	cases := []struct {
		windowDays int
		wantTrunc  string
		wantFormat string
	}{
		{1, "hour", `YYYY-MM-DD"T"HH24:00`},
		{7, "day", "YYYY-MM-DD"},
		{30, "day", "YYYY-MM-DD"},
		{90, "day", "YYYY-MM-DD"},
	}
	for _, c := range cases {
		trunc, format := bridgeSeriesGrain(c.windowDays)
		if trunc != c.wantTrunc || format != c.wantFormat {
			t.Errorf("bridgeSeriesGrain(%d) = (%q, %q), want (%q, %q)",
				c.windowDays, trunc, format, c.wantTrunc, c.wantFormat)
		}
	}
}

// TestCCTPFlowSeriesQueryShape guards the load-bearing properties of the
// CCTP directional flow series SQL: hourly buckets at the 24h window, daily
// otherwise, the direction filter applied, and EXACT NUMERIC division at the
// canonical 6-decimal USDC scale — never a float literal (ADR-0003).
func TestCCTPFlowSeriesQueryShape(t *testing.T) {
	inFilter := `event_type IN ('mint_and_withdraw','mint_and_forward')`

	hourly := cctpFlowSeriesQuery(1, inFilter)
	if !strings.Contains(hourly, "date_trunc('hour', ts)") {
		t.Error("24h query must bucket by hour (a daily bucket gives one point)")
	}
	if !strings.Contains(hourly, `HH24:00`) {
		t.Error("24h query must format point timestamps with the hour")
	}

	daily := cctpFlowSeriesQuery(90, inFilter)
	if !strings.Contains(daily, "date_trunc('day', ts)") {
		t.Error("90d query must bucket by day")
	}
	if strings.Contains(daily, "HH24") {
		t.Error("90d query must not carry an hour component in the timestamp format")
	}

	for _, q := range []string{hourly, daily} {
		if !strings.Contains(q, "/ 1000000::numeric") {
			t.Error("cctp division must be exact NUMERIC at the 6-decimal USDC scale")
		}
		if strings.Contains(q, "1e6") || strings.Contains(q, "1e+06") {
			t.Error("cctp division must never use a float literal (ADR-0003)")
		}
		if !strings.Contains(q, inFilter) {
			t.Error("query must carry the direction filter")
		}
		if !strings.Contains(q, "ts > now() - $1::interval") {
			t.Error("query must be window-bounded (never an unbounded walk)")
		}
	}
}

// TestRozoSettledSeriesQueryShape — same guards for the Rozo settled-volume
// series: hourly at 24h, daily otherwise, payment-only rows, and exact
// NUMERIC division at the 7-decimal SAC-stroop scale (ADR-0003).
func TestRozoSettledSeriesQueryShape(t *testing.T) {
	hourly := rozoSettledSeriesQuery(1)
	if !strings.Contains(hourly, "date_trunc('hour', ts)") || !strings.Contains(hourly, "HH24:00") {
		t.Error("24h query must bucket by hour with the hour in the timestamp format")
	}

	daily := rozoSettledSeriesQuery(30)
	if !strings.Contains(daily, "date_trunc('day', ts)") || strings.Contains(daily, "HH24") {
		t.Error("30d query must bucket by day with a date-only timestamp format")
	}

	for _, q := range []string{hourly, daily} {
		if !strings.Contains(q, "/ 10000000::numeric") {
			t.Error("rozo division must be exact NUMERIC at the 7-decimal SAC scale")
		}
		if strings.Contains(q, "1e7") || strings.Contains(q, "1e+07") {
			t.Error("rozo division must never use a float literal (ADR-0003)")
		}
		if !strings.Contains(q, "event_type = 'payment'") {
			t.Error("rozo series must count payment events only (flush sweeps double-count)")
		}
		if !strings.Contains(q, "ts > now() - $1::interval") {
			t.Error("query must be window-bounded")
		}
	}
}
