// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type fixedCompletenessReader []timescale.CompletenessSnapshot

func (r fixedCompletenessReader) ListCompletenessSnapshots(context.Context) ([]timescale.CompletenessSnapshot, error) {
	return r, nil
}

// TestOverlayCompleteness_ZeroPctIsEmitted: a computed 0% verdict must
// serialise as completeness_pct:0 — the status page reads the field's
// presence as "the audit ran" — while a source with no verdict omits it.
func TestOverlayCompleteness_ZeroPctIsEmitted(t *testing.T) {
	srv := New(Options{
		Logger: slog.New(slog.DiscardHandler),
		CompletenessReader: fixedCompletenessReader{
			{Source: "blend", CoveragePct: 0, ComputedAt: time.Now()},
		},
	})
	rows := []BackfillCoverageRow{{Source: "blend"}, {Source: "phoenix"}}
	srv.overlayCompleteness(context.Background(), &rows)

	blend, err := json.Marshal(rows[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(blend), `"completeness_pct":0`) {
		t.Errorf("computed 0%% verdict dropped from the wire: %s", blend)
	}
	phoenix, err := json.Marshal(rows[1])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(phoenix), "completeness_pct") {
		t.Errorf("source without a verdict must omit completeness_pct: %s", phoenix)
	}
}

// TestOverlayCompleteness_FillsVerifiedRange: on-chain rows show the
// verified range (served floor or genesis, through the watermark); a row
// already carrying a range keeps it.
func TestOverlayCompleteness_FillsVerifiedRange(t *testing.T) {
	srv := New(Options{
		Logger: slog.New(slog.DiscardHandler),
		CompletenessReader: fixedCompletenessReader{
			{Source: "blend", Genesis: 100, Watermark: 900, ProjectionVerifiedFrom: 400},
			{Source: "sdex", Genesis: 2, Watermark: 800},
			{Source: "phoenix", Genesis: 50},
			{Source: "binance", Genesis: 1, Watermark: 9},
		},
	})
	rows := []BackfillCoverageRow{
		{Source: "blend"},
		{Source: "sdex"},
		{Source: "phoenix"},
		{Source: "binance", EarliestLedger: 5, LatestLedger: 7},
	}
	srv.overlayCompleteness(context.Background(), &rows)
	want := [][2]int64{{400, 900}, {2, 800}, {0, 0}, {5, 7}}
	for i, w := range want {
		if got := [2]int64{rows[i].EarliestLedger, rows[i].LatestLedger}; got != w {
			t.Errorf("%s: earliest/latest = %v, want %v", rows[i].Source, got, w)
		}
	}
}

// TestCoverageVerdictStaleAgeTighterThanLedgerHorizon pins the two
// stale gates' relationship: the age gate covers one daily audit period
// and fires before the ledger backstop (~2 periods at 5 s/ledger).
func TestCoverageVerdictStaleAgeTighterThanLedgerHorizon(t *testing.T) {
	const auditPeriod = 24 * time.Hour
	ledgerHorizon := time.Duration(coverageVerdictStaleLedgers) * 5 * time.Second
	if coverageVerdictStaleAge <= auditPeriod {
		t.Errorf("coverageVerdictStaleAge = %s, must exceed one audit period (%s) or every verdict flaps stale daily", coverageVerdictStaleAge, auditPeriod)
	}
	if coverageVerdictStaleAge >= ledgerHorizon {
		t.Errorf("coverageVerdictStaleAge = %s, must be tighter than the ledger horizon (%s)", coverageVerdictStaleAge, ledgerHorizon)
	}
}

// TestCoverageVerdictEvidenceStaleAgeOutlastsTheCarryBound pins the evidence
// gate against the audit's own re-verify cadence. Expiry is judged at run start
// against a DB-stamped proof time, so -pass re-proves a claim up to one audit
// period after MaxProjectionCarryAge, inside the unit's 180-minute budget; two
// further missed nights must still not read stale.
func TestCoverageVerdictEvidenceStaleAgeOutlastsTheCarryBound(t *testing.T) {
	if want := completeness.MaxProjectionCarryAge + 3*coverageVerdictStaleAge; coverageVerdictEvidenceStaleAge != want {
		t.Errorf("coverageVerdictEvidenceStaleAge = %s, want carry bound + 3 audit grace periods (%s)", coverageVerdictEvidenceStaleAge, want)
	}
	const auditPeriod, passBudget = 24 * time.Hour, 180 * time.Minute
	if floor := completeness.MaxProjectionCarryAge + 3*auditPeriod + passBudget; coverageVerdictEvidenceStaleAge <= floor {
		t.Errorf("coverageVerdictEvidenceStaleAge = %s, must exceed carry bound + the expiry slip + two missed nights + pass budget (%s)", coverageVerdictEvidenceStaleAge, floor)
	}
}
