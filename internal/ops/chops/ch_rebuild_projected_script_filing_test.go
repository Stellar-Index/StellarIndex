// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

// The last leg. scripts/ops/ch-rebuild-projected.sh knows when it has left
// a window EMPTY and printed the command that tells the ADR-0033
// completeness verdict about it — but it did not run the command, so the
// obligation existed only for as long as it took an operator to read
// /var/log/ch-rebuild-projected.log. Between a failed re-derive and that
// reading, /v1/coverage kept certifying a range with no rows in it
// complete=true.
//
// These execute the shipped script (harness in
// ch_rebuild_projected_script_test.go) and pin the filing itself: every
// state that leaves a window emptied FILES one projection dirty window for
// exactly the deleted sources, a state that empties nothing files none
//, and a filing that fails is loud rather than silent.

import (
	"testing"
)

// assertFiledWindow checks the one -record-dirty-window invocation the
// script made names the emptied range and exactly the deleted sources. A
// record for the wrong window files the wrong obligation, and one carrying
// -write is refused by the binary (checkCHRebuildRecordDirtyFlags).
func assertFiledWindow(t *testing.T, run scriptRun, lo, hi, sources string) {
	t.Helper()
	recs := run.records()
	if len(recs) != 1 {
		t.Fatalf("the script left [%s,%s] EMPTY and filed %d projection dirty window(s), want 1 — "+
			"until one is filed /v1/coverage carries its prior clean claim over the hole (trace: %s)\n%s",
			lo, hi, len(recs), run.sequence(), run.log)
	}
	rec := recs[0]
	if rec.flag("-from") != lo || rec.flag("-to") != hi || rec.flag("-sources") != sources {
		t.Errorf("filed window = [%s,%s] sources=%s, want [%s,%s] sources=%s",
			rec.flag("-from"), rec.flag("-to"), rec.flag("-sources"), lo, hi, sources)
	}
	if rec.has("-write") || rec.has("-preflight") {
		t.Errorf("the record invocation carries -write/-preflight (%q) — the binary refuses either pairing", rec.args)
	}
	if rec.flag("-config") == "" {
		t.Errorf("the record invocation names no -config (%q), so it cannot reach Postgres", rec.args)
	}
}
