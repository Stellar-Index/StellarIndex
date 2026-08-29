package timescale

import "testing"

// The Reason column of projection_dirty_windows is the ONLY thing that
// distinguishes its two writers, and issue #325 made a reader depend on
// that distinction: the projector suppresses stellarindex_projector_lag_high
// only for a `projector-replay` window, never for a `projected-rebuild`
// one (whose range routinely covers the live cursor's own position, and
// under -allow-live-overlap sits wholly above it). These tests pin the
// format so the classification cannot drift out from under that reader.

// TestProjectionDirtyWindowReasonIsStableAcrossReleases pins the strings
// BYTE-FOR-BYTE against what the shipped binaries have written since
// migration 0125. Rows recorded by an older binary are already in the r1
// table (they survive until compute-completeness re-verifies the range, up
// to a day), so a "harmless" wording change here would silently
// mis-classify live rows — a rebuild row read as a replay is a lag ticket
// suppressed with no operator rewind on record.
func TestProjectionDirtyWindowReasonIsStableAcrossReleases(t *testing.T) {
	// The 2026-08-29 reflector-fx replay, verbatim.
	if got, want := ProjectorReplayReason(64_177_283, 61_602_787),
		"projector-replay rewind 64177283 -> 61602787"; got != want {
		t.Errorf("ProjectorReplayReason = %q, want %q (rows written by older binaries must still classify)", got, want)
	}
	// The 2026-07-27 sep41_supply -allow-live-overlap rebuild, verbatim.
	if got, want := ProjectedRebuildReason(63_419_138, 63_671_020),
		"projected-rebuild -write [63419138,63671020]"; got != want {
		t.Errorf("ProjectedRebuildReason = %q, want %q (rows written by older binaries must still classify)", got, want)
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
