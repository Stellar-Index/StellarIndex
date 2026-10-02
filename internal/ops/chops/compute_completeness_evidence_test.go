// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"strings"
	"testing"
	"time"

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
	prior := map[string]priorProjection{
		"fresh":   {known: true, ok: true, evidencedAt: now.Add(-time.Hour)},
		"old":     {known: true, ok: true, evidencedAt: now.Add(-maxAge - time.Minute)},
		"unknown": {known: true, ok: true},
		"failing": {known: true, ok: false},
	}
	expireStaleCarries(prior, now, maxAge)
	for name, want := range map[string]bool{"fresh": false, "old": true, "unknown": true, "failing": false} {
		if got := prior[name].evidenceExpired; got != want {
			t.Errorf("%s: evidenceExpired = %v, want %v", name, got, want)
		}
	}

	disabled := map[string]priorProjection{"unknown": {known: true, ok: true}}
	expireStaleCarries(disabled, now, 0)
	if disabled["unknown"].evidenceExpired {
		t.Error("-max-carry-age 0 must carry without bound")
	}
}

// TestOrderForPass_ExpiredCarryJoinsTheFromGenesisGroup: a re-verify forced by
// an expired carry is as slow as any other from-genesis reconcile, so it must
// run after the cheap incremental sources, not ahead of them.
func TestOrderForPass_ExpiredCarryJoinsTheFromGenesisGroup(t *testing.T) {
	cat := []reconSource{{name: "a", genesis: 10}, {name: "b", genesis: 10}}
	prior := map[string]priorProjection{
		"a": {known: true, ok: true, tip: 100, evidenceExpired: true},
		"b": {known: true, ok: true, tip: 100},
	}
	got := orderForPass(cat, prior, map[string]uint32{"a": 100, "b": 100})
	if got[0].name != "b" || got[1].name != "a" {
		t.Errorf("order = [%s %s], want [b a]", got[0].name, got[1].name)
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
