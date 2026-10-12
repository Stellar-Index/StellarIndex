// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

// ch-rebuild-projected.sh must itself file the ADR-0033 dirty window whenever it leaves a window empty, or /v1/coverage
// keeps certifying a range with no rows complete=true. Every emptying state files one window for exactly the deleted
// sources; a state that empties nothing files none; a failed filing is loud.

import (
	"testing"
)

// assertFiledWindow checks the one -record-dirty-window invocation names the emptied range and exactly the deleted
// sources. A wrong window files the wrong obligation, and one carrying -write is refused by the binary
// (checkCHRebuildRecordDirtyFlags).
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
