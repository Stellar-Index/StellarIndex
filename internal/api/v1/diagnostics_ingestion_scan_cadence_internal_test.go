// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type fixedCoverageReader []timescale.SourceCoverage

func (f fixedCoverageReader) ListSourceCoverage(context.Context) ([]timescale.SourceCoverage, error) {
	return f, nil
}

// A 6 h target's snapshot must not be aged against the 30 min default,
// or its row reads stale for most of every healthy cycle.
func TestOverlaySourceCoverageV2_ServesPerRowScanCadence(t *testing.T) {
	now := time.Now()
	srv := New(Options{})
	srv.coverageReader = fixedCoverageReader{
		{Source: "sdex", GapFreePct: 1, LastUpdated: now},
		{Source: "blend-positions", GapFreePct: 1, LastUpdated: now},
		{Source: "blend-emissions", GapFreePct: 1, LastUpdated: now},
	}
	rows := []BackfillCoverageRow{{Source: "sdex"}, {Source: "blend"}, {Source: "phoenix"}}

	srv.overlaySourceCoverageV2(context.Background(), &rows)

	want := map[string]int64{
		"sdex":    int64(6 * time.Hour / time.Second),
		"blend":   int64(timescale.GapDetectorInterval / time.Second),
		"phoenix": 0, // no snapshot yet: no cadence claimed
	}
	for _, r := range rows {
		if r.CoverageScanCadenceS != want[r.Source] {
			t.Errorf("%s: coverage_scan_cadence_s = %d, want %d", r.Source, r.CoverageScanCadenceS, want[r.Source])
		}
	}
}
