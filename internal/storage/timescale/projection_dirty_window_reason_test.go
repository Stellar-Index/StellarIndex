package timescale

import (
	"strings"
	"testing"
)

// The Reason column of projection_dirty_windows is the only thing that distinguishes its
// two writers, and a reader depends on it: the projector suppresses
// stellarindex_projector_lag_high only for a `projector-replay` window, never for a
// `projected-rebuild` one (whose range routinely covers the live cursor, and under
// -allow-live-overlap sits wholly above it). These tests pin the format so the
// classification cannot drift out from under that reader.

// TestProjectionDirtyWindowReasonIsStableAcrossReleases pins the strings byte-for-byte against what shipped
// binaries wrote. Older rows survive until compute-completeness re-verifies, so a wording change would
// misclassify them: a rebuild row read as a replay suppresses a lag ticket with no operator rewind.
func TestProjectionDirtyWindowReasonIsStableAcrossReleases(t *testing.T) {
	// A real reflector-fx replay reason, verbatim.
	if got, want := ProjectorReplayReason(64_177_283, 61_602_787),
		"projector-replay rewind 64177283 -> 61602787"; got != want {
		t.Errorf("ProjectorReplayReason = %q, want %q (rows written by older binaries must still classify)", got, want)
	}
	// A real sep41_supply -allow-live-overlap rebuild reason, verbatim.
	if got, want := ProjectedRebuildReason(63_419_138, 63_671_020),
		"projected-rebuild -write [63419138,63671020]"; got != want {
		t.Errorf("ProjectedRebuildReason = %q, want %q (rows written by older binaries must still classify)", got, want)
	}
	if got, want := BackfillWriteReason(50_000_000, 50_099_999),
		"backfill -write [50000000,50099999]"; got != want {
		t.Errorf("BackfillWriteReason = %q, want %q (rows written by older binaries must still classify)", got, want)
	}
}

// TestProjectionDirtyWindowIsProjectorReplay pins the classification the
// projector's replay-window flag keys off, including the fail-open answer
// for anything unrecognised.
func TestProjectionDirtyWindowIsProjectorReplay(t *testing.T) {
	cases := []struct {
		name   string
		reason string
		want   bool
	}{
		{"replay via constructor", ProjectorReplayReason(64_177_283, 61_602_787), true},
		{"replay as written by the pre-#325 binary", "projector-replay rewind 63550000 -> 62270000", true},
		{"rebuild via constructor", ProjectedRebuildReason(63_419_138, 63_671_020), false},
		{"rebuild as written by the pre-#325 binary", "projected-rebuild -write [60000000,62000000]", false},
		{"backfill via constructor", BackfillWriteReason(50_000_000, 50_099_999), false},
		{"empty reason (row from some future writer)", "", false},
		{"unrecognised reason", "manual fixup by an operator", false},
		{"mentions replay but is not one", "reverted a projector-replay rewind 1 -> 2", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := ProjectionDirtyWindow{Source: "reflector-fx", From: 1, To: 2, Reason: tc.reason}
			if got := w.IsProjectorReplay(); got != tc.want {
				t.Errorf("IsProjectorReplay(%q) = %v, want %v", tc.reason, got, tc.want)
			}
		})
	}
}

// The third writer of projection_dirty_windows: a clean-slate window that
// scripts/ops/ch-rebuild-projected.sh deleted and did not re-derive. Its provenance must be
// distinguishable from the other two and must NOT read as a projector-replay rewind, which
// suppresses stellarindex_projector_lag_high while this row means rows are MISSING.
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

// TestClearProjectionDirtyWindowGuardsOnUpdatedAt: the bounds only catch a widened window, so the clear must
// also require `updated_at = $4`; any re-record bumps it and the window stays pending (fail-closed).
// Asserted on the query text; the end-to-end path is in the integration suite.
func TestClearProjectionDirtyWindowGuardsOnUpdatedAt(t *testing.T) {
	t.Parallel()
	q := clearProjectionDirtyWindowQuery

	if !strings.Contains(q, "updated_at = $4") {
		t.Error("the clear does not compare updated_at — a subset or identical " +
			"re-record between this run's read and its clear would be deleted, " +
			"erasing an unverified rewind and letting the next verdict carry a " +
			"clean claim over it")
	}
	// The bounds must stay: they are what makes a WIDENED window survive.
	// updated_at alone would delete a widened row whose bounds no longer
	// match this run's obligation.
	for _, want := range []string{"from_ledger >= $2", "to_ledger <= $3", "source = $1"} {
		if !strings.Contains(q, want) {
			t.Errorf("the clear lost its %q predicate — the bounds and the "+
				"updated_at guard cover different races and both are required", want)
		}
	}
	if !strings.HasPrefix(strings.TrimSpace(q), "DELETE FROM projection_dirty_windows") {
		t.Errorf("unexpected statement shape: %q", strings.TrimSpace(q))
	}
}
