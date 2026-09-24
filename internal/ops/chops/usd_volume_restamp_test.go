// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"strings"
	"testing"
	"time"
)

// TestUSDVolumeRestamp_IsDispatchable pins that the W5.3 tool exists under
// its runbook name and lives in the WRITING half of the dispatcher: the
// v1-launch-plan carried "no usd-volume-restamp tool exists yet" as an open
// item, and the verifier half must never resolve a verb that mutates rows.
func TestUSDVolumeRestamp_IsDispatchable(t *testing.T) {
	if _, ok := lakeMutatorVerb("usd-volume-restamp"); !ok {
		t.Fatal(`lakeMutatorVerb("usd-volume-restamp") = not found — the W5.3 restamp tool is not dispatchable`)
	}
	if _, ok := verifierVerb("usd-volume-restamp"); ok {
		t.Fatal("usd-volume-restamp resolved as a VERIFIER verb, but it writes trades.usd_volume")
	}
}

// TestUSDVolumeRestamp_RefusesUnboundedOrLiveWindows: a money-column
// writer gets no "whole history" default and never touches today's
// still-being-written chunk. The window check runs before any DB is
// opened, so this runs without one.
func TestUSDVolumeRestamp_RefusesUnboundedOrLiveWindows(t *testing.T) {
	now := time.Date(2026, 8, 28, 13, 0, 0, 0, time.UTC)
	cases := []struct {
		name     string
		from, to string
		wantErr  string
	}{
		{"no window", "", "", "-from and -to are required"},
		{"half window", "2026-05-12", "", "-from and -to are required"},
		{"reversed", "2026-07-22", "2026-05-12", "is before -from"},
		{"reaches today", "2026-08-01", "2026-08-28", "reaches today"},
		{"future", "2026-09-01", "2026-09-02", "reaches today"},
		{"bad format", "12/05/2026", "2026-07-22", "want YYYY-MM-DD"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := resolveRestampWindow(tc.from, tc.to, now)
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
			}
		})
	}

	from, to, err := resolveRestampWindow("2026-05-12", "2026-07-22", now)
	if err != nil {
		t.Fatal(err)
	}
	if from.Format(time.DateOnly) != "2026-05-12" || to.Format(time.DateOnly) != "2026-07-22" {
		t.Errorf("window = [%s, %s]", from, to)
	}
	if from.Location() != time.UTC || to.Location() != time.UTC {
		t.Error("window days are not UTC")
	}
	// Yesterday is the newest closed day and must be accepted.
	if _, _, err := resolveRestampWindow("2026-08-27", "2026-08-27", now); err != nil {
		t.Errorf("yesterday rejected: %v", err)
	}
}

func TestRestampSourceAllowList(t *testing.T) {
	if got := restampSourceAllowList(""); got != nil {
		t.Errorf("empty -sources = %v, want nil (all sources)", got)
	}
	got := restampSourceAllowList(" sdex, soroswap ,")
	if len(got) != 2 || !got["sdex"] || !got["soroswap"] {
		t.Errorf("allow list = %v", got)
	}
}

// TestValidateRestampSourceAllowList_unknownNameRefused pins CA2-A13: an
// estimated tier's -sources value that names a source outside the tier's
// own registry must be refused, not silently dropped by
// [timescale.restampScanSources] into an empty, "clean-looking" scan.
func TestValidateRestampSourceAllowList_unknownNameRefused(t *testing.T) {
	// sdex is a DEX source; -tier cex-fx only recognises CEX sources.
	err := validateRestampSourceAllowList(restampTierCEXFX, restampSourceAllowList("sdex"))
	if err == nil {
		t.Fatal("validateRestampSourceAllowList(cex-fx, {sdex}) = nil — a DEX-only source on the CEX tier would scan nothing and exit 0")
	}
	if !strings.Contains(err.Error(), "sdex") {
		t.Errorf("refusal must name the offending value; got: %v", err)
	}
	if !strings.Contains(err.Error(), restampTierCEXFX) {
		t.Errorf("refusal must name the tier; got: %v", err)
	}

	// A plain typo on the DEX-anchored tiers is refused the same way.
	if err := validateRestampSourceAllowList(restampTierXLMBase, restampSourceAllowList("soroswapp")); err == nil {
		t.Fatal("validateRestampSourceAllowList(xlm-base, {soroswapp}) = nil — a typo would scan nothing and exit 0")
	}
}

// TestValidateRestampSourceAllowList_knownNamesAccepted: a real source on
// its own tier, and an unset -sources (nil allow-list, meaning "all"),
// pass through untouched.
func TestValidateRestampSourceAllowList_knownNamesAccepted(t *testing.T) {
	if err := validateRestampSourceAllowList(restampTierXLMBase, restampSourceAllowList("sdex")); err != nil {
		t.Errorf("known DEX source on xlm-base refused: %v", err)
	}
	if err := validateRestampSourceAllowList(restampTierCEXFX, nil); err != nil {
		t.Errorf("nil allow-list (all sources) refused: %v", err)
	}
	// The exact tier is data-driven, not registry-checked here.
	if err := validateRestampSourceAllowList(restampTierExact, restampSourceAllowList("not-a-real-source")); err != nil {
		t.Errorf("exact tier must not be registry-gated by this guard: %v", err)
	}
}
