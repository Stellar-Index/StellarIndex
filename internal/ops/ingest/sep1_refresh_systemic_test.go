// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"fmt"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// sep1Run builds an attempted batch: reachedOK/reachedFailed rows that had
// served a payload before, and neverOK/neverFailed rows that never had.
func sep1Run(reachedOK, reachedFailed, neverOK, neverFailed int) ([]timescale.IssuerSep1Candidate, []string) {
	var cands []timescale.IssuerSep1Candidate
	var failed []string
	add := func(n int, reached, fail bool) {
		for range n {
			key := fmt.Sprintf("G%06d", len(cands))
			cands = append(cands, timescale.IssuerSep1Candidate{GStrkey: key, HomeDomain: "example.org", Reached: reached})
			if fail {
				failed = append(failed, key)
			}
		}
	}
	add(reachedOK, true, false)
	add(reachedFailed, true, true)
	add(neverOK, false, false)
	add(neverFailed, false, true)
	return cands, failed
}

// TestSep1RunVerdict pins the systemic-outage guard: it trips on domains
// that served a stellar.toml before and now fail, never on domains that
// have never answered.
//
// The guard exists because a retry backoff cannot tell "this domain is
// dead" from "our DNS is down" — both are a failed fetch — and an
// unguarded backoff would walk the whole population to the 30-day cap
// during an outage on our side and then go quiet, with the
// data-freshness watchdog staying green throughout (it reads
// max(sep1_resolved_at), which a FAILED attempt stamps too).
func TestSep1RunVerdict(t *testing.T) {
	r := defaultSystemicFailureRate
	for _, tc := range []struct {
		name                                         string
		reachedOK, reachedFailed, neverOK, neverFail int
		rate                                         float64
		want                                         bool
	}{
		{
			// The testnet shape that failed the unit every hour: 19 real
			// domains answer, 731 junk home_domains never have.
			name:      "testnet healthy run (19 reached ok / 731 never-reached failed)",
			reachedOK: 19, neverFail: 731, rate: r, want: false,
		},
		{
			// The r1 baseline: the 291 failures were all never-reached rows.
			name:      "r1 healthy run (209 ok / 291 never-reached failed)",
			reachedOK: 209, neverFail: 291, rate: r, want: false,
		},
		{
			name:          "true outage: previously-verified domains fail en masse",
			reachedFailed: 200, neverFail: 550, rate: r, want: true,
		},
		{
			name:          "testnet outage: the 19 real domains fail, below the sample floor",
			reachedFailed: 19, neverFail: 731, rate: r, want: false,
		},
		{
			name:      "just under the threshold",
			reachedOK: 11, reachedFailed: 89, neverFail: 500, rate: r, want: false,
		},
		{
			name:      "exactly at the threshold trips it",
			reachedOK: 10, reachedFailed: 90, rate: r, want: true,
		},
		{
			name:          "reached sample below the floor never trips, even at 100%",
			reachedFailed: systemicMinAttempts - 1, neverFail: 700, rate: r, want: false,
		},
		{
			name:          "reached sample exactly at the floor is eligible",
			reachedFailed: systemicMinAttempts, rate: r, want: true,
		},
		{
			// `sep1-refresh -issuer G…` resolves one domain. A transient
			// failure there must not unwind anything or fail the unit.
			name:          "targeted single-issuer refresh",
			reachedFailed: 1, rate: r, want: false,
		},
		{
			// The documented escape hatch for a known mass outage.
			name:          "rate above 1 disables the guard",
			reachedFailed: 750, rate: 1.5, want: false,
		},
		{
			name: "empty run",
			rate: r, want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cands, failed := sep1Run(tc.reachedOK, tc.reachedFailed, tc.neverOK, tc.neverFail)
			if got := sep1RunVerdict(cands, failed, tc.rate); got != tc.want {
				t.Errorf("sep1RunVerdict(reached ok=%d failed=%d, never ok=%d failed=%d, rate=%v) = %v, want %v",
					tc.reachedOK, tc.reachedFailed, tc.neverOK, tc.neverFail, tc.rate, got, tc.want)
			}
		})
	}
}

// TestSep1RunVerdictTestnetShape guards the fixture itself: the testnet
// batch is 97% failures overall, so a verdict over raw attempts trips on
// it, and only the reached-domain population keeps it green.
func TestSep1RunVerdictTestnetShape(t *testing.T) {
	cands, failed := sep1Run(19, 0, 0, 731)
	if len(cands) != 750 || len(failed) != 731 {
		t.Fatalf("fixture = %d attempts / %d failed; want 750 / 731", len(cands), len(failed))
	}
	if raw := float64(len(failed)) / float64(len(cands)); raw < defaultSystemicFailureRate {
		t.Fatalf("raw failure rate %.3f is below %.2f; fixture no longer reproduces the testnet run", raw, defaultSystemicFailureRate)
	}
	if sep1RunVerdict(cands, failed, defaultSystemicFailureRate) {
		t.Error("testnet healthy run judged SYSTEMIC; never-reached domains must not count as regressions")
	}
}
