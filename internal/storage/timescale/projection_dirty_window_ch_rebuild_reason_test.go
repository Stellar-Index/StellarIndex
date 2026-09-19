// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package timescale

import "testing"

// The third writer of projection_dirty_windows (F075): a clean-slate window
// scripts/ops/ch-rebuild-projected.sh DELETEd and did not re-derive. Its
// provenance has to be distinguishable from the other two, and — the part
// that matters to an on-call — it must NOT read as a projector-replay
// rewind: that classification SUPPRESSES stellarindex_projector_lag_high,
// and this row means rows are MISSING, which is the last state to silence.
func TestCHRebuildEmptiedReason(t *testing.T) {
	got := CHRebuildEmptiedReason(61_000_000, 61_999_999)
	if want := "ch-rebuild-projected emptied [61000000,61999999]"; got != want {
		t.Errorf("CHRebuildEmptiedReason = %q, want %q", got, want)
	}
	w := ProjectionDirtyWindow{Source: "cctp", From: 61_000_000, To: 61_999_999, Reason: got}
	if w.IsProjectorReplay() {
		t.Errorf("an emptied-window row classifies as a projector-replay rewind (%q) — the lag alert would be suppressed over a range whose rows are gone", got)
	}
	// Distinct from the rebuild prefix too: `projected-rebuild -write`
	// REWROTE its range, this one EMPTIED it, and an operator reading the
	// table must be able to tell which.
	if pr := ProjectedRebuildReason(61_000_000, 61_999_999); pr == got {
		t.Errorf("the emptied reason is indistinguishable from projected-rebuild's: %q", got)
	}
}
