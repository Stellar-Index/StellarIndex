// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestProjectionEvidence_CarryKeepsThePriorProofTime pins what a verdict
// publishes as the age of its projection evidence. Only a run that reconciled
// the whole served range stamps it now; a carry restates the prior claim and
// must keep the prior claim's proof time, or every nightly incremental run
// would present a weeks-old proof as fresh.
func TestProjectionEvidence_CarryKeepsThePriorProofTime(t *testing.T) {
	const servedFrom = uint32(61_609_957)
	proven := time.Date(2026, 9, 1, 5, 40, 0, 0, time.UTC)
	prior := priorProjection{known: true, ok: true, tip: 64_000_000, evidencedAt: proven}

	if now, at := projectionEvidence(true, servedFrom, servedFrom, prior); !now || !at.IsZero() {
		t.Errorf("full-range reconcile: (now=%v, at=%v), want (true, zero) — this run is the proof", now, at)
	}
	if now, at := projectionEvidence(true, servedFrom, 64_000_001, prior); now || !at.Equal(proven) {
		t.Errorf("carried claim: (now=%v, at=%v), want (false, %v) — a carry is not new evidence", now, at, proven)
	}
	if now, at := projectionEvidence(true, servedFrom, 64_000_001, priorProjection{known: true, ok: true, tip: 64_000_000}); now || !at.IsZero() {
		t.Errorf("carry from a prior with no evidence on record: (now=%v, at=%v), want (false, zero)", now, at)
	}
	if now, at := projectionEvidence(false, servedFrom, servedFrom, prior); now || !at.IsZero() {
		t.Errorf("failing claim: (now=%v, at=%v), want (false, zero) — no claim, no evidence", now, at)
	}
}

// TestProjectionFloor_PassReVerifiesAnExpiredCarry: a clean prior whose
// full-range evidence is past -max-carry-age must re-reconcile from genesis,
// so the run proves the whole served range instead of carrying it yet again.
func TestProjectionFloor_PassReVerifiesAnExpiredCarry(t *testing.T) {
	expired := priorProjection{known: true, ok: true, tip: sushiTip, evidenceExpired: true}
	if got := projectionFloor(sushiGenesis, true, expired, sushiTip, 0); got != sushiGenesis {
		t.Errorf("expired-carry floor = %d, want genesis %d", got, sushiGenesis)
	}
	// Outside -pass the floor stays operator-stated.
	if got := projectionFloor(sushiGenesis, false, expired, sushiTip, 63_000_000); got != 63_000_000 {
		t.Errorf("non-pass floor = %d, want the -from 63000000", got)
	}
}

func TestExpireStaleCarries(t *testing.T) {
	now := time.Date(2026, 10, 2, 5, 30, 0, 0, time.UTC)
	const maxAge = 7 * 24 * time.Hour
	cat := []reconSource{{name: "fresh"}, {name: "old"}, {name: "unknown"}, {name: "failing"}}
	prior := map[string]priorProjection{
		"fresh":   {known: true, ok: true, evidencedAt: now.Add(-time.Hour)},
		"old":     {known: true, ok: true, evidencedAt: now.Add(-maxAge - time.Minute)},
		"unknown": {known: true, ok: true},
		"failing": {known: true, ok: false},
	}
	expireStaleCarries(prior, cat, now, maxAge)
	for name, want := range map[string]bool{"fresh": false, "old": true, "unknown": true, "failing": false} {
		if got := prior[name].evidenceExpired; got != want {
			t.Errorf("%s: evidenceExpired = %v, want %v", name, got, want)
		}
	}

	disabled := map[string]priorProjection{"unknown": {known: true, ok: true}}
	expireStaleCarries(disabled, []reconSource{{name: "unknown"}}, now, 0)
	if disabled["unknown"].evidenceExpired {
		t.Error("-max-carry-age 0 must carry without bound")
	}
}

// TestExpireStaleCarries_NeverRefloorsTheCensus: a full SDEX re-derive outlasts
// the pass's deadline and a deadline-cut source writes nothing, so a forced
// re-floor would recur every night without ever refreshing the evidence.
func TestExpireStaleCarries_NeverRefloorsTheCensus(t *testing.T) {
	now := time.Date(2026, 10, 2, 5, 30, 0, 0, time.UTC)
	cat := []reconSource{{name: "sdex", census: true}, {name: "aquarius"}}
	prior := map[string]priorProjection{
		"sdex":     {known: true, ok: true},
		"aquarius": {known: true, ok: true},
	}
	got := expireStaleCarries(prior, cat, now, completeness.MaxProjectionCarryAge)
	if prior["sdex"].evidenceExpired {
		t.Error("census source marked expired; it must keep carrying with its evidence ageing honestly")
	}
	if len(got) != 1 || got[0] != "aquarius" {
		t.Errorf("refloored = %v, want [aquarius]", got)
	}
	if f := projectionFloor(100, true, prior["sdex"], 64_000_000, 0); f != 64_000_000 {
		t.Errorf("census -pass floor = %d, want its prior watermark 64000000", f)
	}
}

// TestExpireStaleCarries_CapsOldestFirst: at most maxEvidenceRefloorsPerPass
// sources re-prove per pass, unknown evidence first, then the oldest; the rest
// keep carrying, which staggers the next expiry instead of re-flooring all at once.
func TestExpireStaleCarries_CapsOldestFirst(t *testing.T) {
	if maxEvidenceRefloorsPerPass != 3 {
		t.Fatalf("maxEvidenceRefloorsPerPass = %d, test assumes 3", maxEvidenceRefloorsPerPass)
	}
	now := time.Date(2026, 10, 2, 5, 30, 0, 0, time.UTC)
	day := 24 * time.Hour
	cat := []reconSource{{name: "a"}, {name: "b"}, {name: "c"}, {name: "d"}, {name: "e"}}
	prior := map[string]priorProjection{
		"a": {known: true, ok: true, evidencedAt: now.Add(-8 * day)},
		"b": {known: true, ok: true, evidencedAt: now.Add(-20 * day)},
		"c": {known: true, ok: true},
		"d": {known: true, ok: true, evidencedAt: now.Add(-9 * day)},
		"e": {known: true, ok: true, evidencedAt: now.Add(-day)},
	}
	got := expireStaleCarries(prior, cat, now, completeness.MaxProjectionCarryAge)
	if strings.Join(got, ",") != "c,b,d" {
		t.Errorf("refloored = %v, want [c b d] (unknown first, then oldest)", got)
	}
	for name, want := range map[string]bool{"a": false, "b": true, "c": true, "d": true, "e": false} {
		if prior[name].evidenceExpired != want {
			t.Errorf("%s: evidenceExpired = %v, want %v", name, prior[name].evidenceExpired, want)
		}
	}
	if !prior["a"].evidencedAt.Equal(now.Add(-8 * day)) {
		t.Error("a source left over the cap must keep its old evidence time")
	}
}

func TestBuildPriorVerdicts_CarriesTheEvidenceTime(t *testing.T) {
	proven := time.Date(2026, 9, 1, 5, 40, 0, 0, time.UTC)
	priorProj, _, _, _ := buildPriorVerdicts([]timescale.CompletenessSnapshot{
		{Source: "sdex", ProjectionOK: true, Watermark: 64_000_000, ProjectionEvidencedAt: proven},
	})
	if got := priorProj["sdex"].evidencedAt; !got.Equal(proven) {
		t.Errorf("prior evidencedAt = %v, want %v", got, proven)
	}
}

func TestCarriedEvidenceDetail(t *testing.T) {
	if d := carriedEvidenceDetail(time.Time{}); !strings.Contains(d, "no full-range reconcile on record") {
		t.Errorf("unknown evidence detail = %q", d)
	}
	at := time.Date(2026, 9, 1, 5, 40, 0, 0, time.UTC)
	if d := carriedEvidenceDetail(at); !strings.Contains(d, "2026-09-01T05:40:00Z") {
		t.Errorf("detail = %q, want it to name the proof time", d)
	}
}

// A prior clean projection verdict carries only as far as its Watermark (the
// range it reconciled), never to Tip, which sits above a recognition gap.
func TestBuildPriorVerdicts_ProjectionCarryBoundsToWatermarkNotTip(t *testing.T) {
	const (
		servedFrom = uint32(61_500_000)
		watermark  = uint32(62_000_000)
		tip        = uint32(62_500_000)
	)
	snaps := []timescale.CompletenessSnapshot{
		{Source: "soroswap", ProjectionOK: true, SubstrateOK: true, RecognitionOK: true, Tip: tip, Watermark: watermark},
	}
	priorProj, _, _, _ := buildPriorVerdicts(snaps)

	prior := priorProj["soroswap"]
	if prior.tip != watermark {
		t.Fatalf("priorProj[soroswap].tip = %d, want %d (Watermark, not Tip=%d)", prior.tip, watermark, tip)
	}
	ok, detail := projectionClaim(servedFrom, tip, tip, true, "", prior, testScope)
	if ok {
		t.Fatalf("projectionClaim carried a prior verdict over [%d,%d], a band the prior run never reconciled", watermark+1, tip-1)
	}
	if !strings.Contains(detail, fmt.Sprintf("%d", watermark)) {
		t.Errorf("rejection detail must name the prior verdict's true reach (watermark=%d), got: %s", watermark, detail)
	}
}

func TestBuildPriorVerdicts_CarriesProjectionVerifiedFrom(t *testing.T) {
	prior, _, _, _ := buildPriorVerdicts([]timescale.CompletenessSnapshot{
		{Source: "band", ProjectionOK: true, Watermark: bandTip, ProjectionVerifiedFrom: bandServedMin},
	})
	if got := prior["band"].verifiedFrom; got != bandServedMin {
		t.Fatalf("priorProj[band].verifiedFrom = %d, want %d", got, bandServedMin)
	}
}

// -pass resumes each source's projection from its own watermark when its prior
// verdict is clean (keeping the nightly cheap), from genesis when it is red or
// unseeded; outside -pass the floor is the operator-stated max(genesis, -from).
func TestProjectionFloor(t *testing.T) {
	const (
		aquariusGenesis     = uint32(52_728_375)
		healthyGenesis      = uint32(50_746_266)
		blendEmitterGenesis = uint32(51_499_914)
		tip                 = uint32(63_997_554)
	)
	clean := priorProjection{known: true, ok: true, tip: tip}
	clean63 := priorProjection{known: true, ok: true, tip: 63_000_000}
	failing63 := priorProjection{known: true, ok: false, tip: 63_000_000}
	cases := []struct {
		name      string
		genesis   uint32
		pass      bool
		prior     priorProjection
		watermark uint32
		from      uint
		want      uint32
	}{
		{"pass: healthy resumes at watermark", healthyGenesis, true, clean, tip - 100, 0, tip - 100},
		{"pass: recognition-capped resumes at its low watermark", aquariusGenesis, true, clean, 55_363_631, 0, 55_363_631},
		{"pass: never-seeded floors at genesis", blendEmitterGenesis, true, priorProjection{}, 0, 0, blendEmitterGenesis},
		{"pass: sub-genesis watermark clamps", blendEmitterGenesis, true, clean63, 40_000_000, 0, blendEmitterGenesis},
		{"pass: clean prior keeps the cheap resume", sushiGenesis, true, priorProjection{known: true, ok: true, tip: sushiTip}, sushiTip, 0, sushiTip},
		{"pass: failing prior re-verifies from genesis", sushiGenesis, true, priorProjection{known: true, ok: false, tip: sushiTip}, sushiTip, 0, sushiGenesis},
		{"non-pass -from, clean prior", healthyGenesis, false, clean63, 999_999, 63_000_000, 63_000_000},
		{"non-pass -from, failing prior", healthyGenesis, false, failing63, 999_999, 63_000_000, 63_000_000},
		{"non-pass -from, no prior", healthyGenesis, false, priorProjection{}, 999_999, 63_000_000, 63_000_000},
		{"non-pass full run ignores the watermark", healthyGenesis, false, clean63, 63_000_000, 0, healthyGenesis},
	}
	for _, tc := range cases {
		if got := projectionFloor(tc.genesis, tc.pass, tc.prior, tc.watermark, tc.from); got != tc.want {
			t.Errorf("%s: projectionFloor = %d, want %d", tc.name, got, tc.want)
		}
	}
}
