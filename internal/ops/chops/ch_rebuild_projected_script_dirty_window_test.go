// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

// An emptied window has TWO records to keep in scripts/ops/ch-rebuild-
// projected.sh:
//
//   - $DIRTY, the local marker that drives this script's own recovery. It
//     must be WRITTEN before anything is deleted — a failed append may not
//     be shrugged off, or the DELETE proceeds with nothing left to rebuild
//     the window.
//   - the ADR-0033 projection dirty window, which is what the completeness
//     verdict reads. Nothing on this box's filesystem reaches it, so until
//     the emptied range is filed there, /v1/coverage keeps carrying its
//     prior clean claim over a hole.
//
// These tests EXECUTE the shipped script (the harness in
// ch_rebuild_projected_script_test.go) and pin both, plus the case that
// must NOT change: a window that re-derived cleanly files nothing (a measured decision —
// filing one per routine window would point the next nightly at ~12.9M
// ledgers across 8 sources and time every verdict out).

import (
	"strings"
	"testing"
)

// verdictNoteRE-ish: the note is matched as plain text on purpose — it is
// what an operator greps for in /var/log/ch-rebuild-projected.log.
const verdictNoteHead = "TELL THE VERDICT"

// recordCommandFor returns the `-record-dirty-window` command line the
// script printed for [lo,hi], or "" if it printed none.
func recordCommandFor(log, lo, hi string) string {
	var seen bool
	for _, line := range strings.Split(log, "\n") {
		switch {
		case strings.HasPrefix(line, verdictNoteHead+" ["+lo+","+hi+"]"):
			seen = true
		case seen && strings.Contains(line, "-record-dirty-window"):
			return strings.TrimSpace(line)
		case seen && strings.HasPrefix(line, verdictNoteHead):
			seen = false // a different window's note; keep looking
		}
	}
	return ""
}

// assertRecordCommand checks the printed command names the emptied range
// and exactly the deleted sources — a note that points at the wrong window
// files the wrong obligation.
func assertRecordCommand(t *testing.T, cmd, lo, hi, sources string) {
	t.Helper()
	for _, want := range []string{
		"ch-rebuild", "-from " + lo, "-to " + hi, "-sources " + sources, "-record-dirty-window",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("printed record command %q does not carry %q", cmd, want)
		}
	}
	if strings.Contains(cmd, "-write") {
		t.Errorf("printed record command %q carries -write — the record is not a re-derive, and the binary refuses the pair", cmd)
	}
}

// ── the local marker: rule 3 is only a rule if the write is CHECKED ──────
