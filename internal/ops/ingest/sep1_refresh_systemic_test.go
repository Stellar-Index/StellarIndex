// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ingest

import "testing"

// TestSep1RunVerdict pins the calibration of the systemic-outage
// guard against the numbers it was derived from.
//
// The guard exists because a retry backoff cannot tell "this domain is
// dead" from "our DNS is down" — both are a failed fetch — and an
// unguarded backoff would walk the whole population to the 30-day cap
// during an outage on our side and then go quiet, with the
// data-freshness watchdog staying green throughout (it reads
// max(sep1_resolved_at), which a FAILED attempt stamps too).
//
// So the threshold has to sit above what the real population can
// produce and below "everything is broken". The measured healthy
// baseline on r1 (2026-09-12) was 291 failures in 500 attempts.
func TestSep1RunVerdict(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ok, failed int
		rate       float64
		want       bool
	}{
		{
			// The r1 baseline. More than half the population genuinely
			// serves nothing; that must never read as an outage.
			name: "measured healthy run (209 ok / 291 failed)",
			ok:   209, failed: 291, rate: defaultSystemicFailureRate, want: false,
		},
		{
			name: "total outage — every domain failed",
			ok:   0, failed: 750, rate: defaultSystemicFailureRate, want: true,
		},
		{
			name: "just under the threshold",
			ok:   11, failed: 89, rate: defaultSystemicFailureRate, want: false,
		},
		{
			name: "exactly at the threshold trips it",
			ok:   10, failed: 90, rate: defaultSystemicFailureRate, want: true,
		},
		{
			// A nearly-drained queue, or a deadline-truncated batch, can
			// hand back a handful of rows that all happen to be dead.
			// That is not evidence about the run.
			name: "small sample never trips, even at 100%",
			ok:   0, failed: systemicMinAttempts - 1, rate: defaultSystemicFailureRate, want: false,
		},
		{
			name: "sample exactly at the floor is eligible",
			ok:   0, failed: systemicMinAttempts, rate: defaultSystemicFailureRate, want: true,
		},
		{
			// `sep1-refresh -issuer G…` resolves one domain. A transient
			// failure there must not unwind anything or fail the unit.
			name: "targeted single-issuer refresh",
			ok:   0, failed: 1, rate: defaultSystemicFailureRate, want: false,
		},
		{
			// The documented escape hatch: an operator working through a
			// known mass outage sets the rate above 1 to let the job run.
			name: "rate above 1 disables the guard",
			ok:   0, failed: 750, rate: 1.5, want: false,
		},
		{
			name: "empty run",
			ok:   0, failed: 0, rate: defaultSystemicFailureRate, want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sep1RunVerdict(tc.ok, tc.failed, tc.rate); got != tc.want {
				t.Errorf("sep1RunVerdict(ok=%d, failed=%d, rate=%v) = %v, want %v",
					tc.ok, tc.failed, tc.rate, got, tc.want)
			}
		})
	}
}
