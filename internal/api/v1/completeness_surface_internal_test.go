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
