package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParse_RealBacklog — the actual launch-readiness backlog
// MUST parse cleanly. Catches regressions where someone changes
// the table format in a way the parser can't follow.
func TestParse_RealBacklog(t *testing.T) {
	const path = "../../../docs/architecture/launch-readiness-backlog.md"
	rows, err := parseFile(path)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	if len(rows) < 30 {
		t.Errorf("parsed %d rows; expected ≥ 30 — table format change?", len(rows))
	}
	// Every row should have a recognised status.
	for _, r := range rows {
		if r.Status == "?" {
			t.Errorf("row %s has unrecognised status", r.ID)
		}
	}
	// Every row should have a known surface prefix.
	for _, r := range rows {
		switch r.Surface {
		case "L1", "L2", "L3", "L4", "L5", "L6", "L7":
			// ok
		default:
			t.Errorf("row %s has unknown surface %q", r.ID, r.Surface)
		}
	}
}

// TestEngineeringReady_AllShipped — when every L1-L5 row is ✅,
// engineeringReady returns true (regardless of L6 status).
func TestEngineeringReady_AllShipped(t *testing.T) {
	rows := []Row{
		{ID: "L1.1", Status: "✅", Surface: "L1"},
		{ID: "L2.1", Status: "✅", Surface: "L2"},
		{ID: "L3.1", Status: "✅", Surface: "L3"},
		{ID: "L4.1", Status: "✅", Surface: "L4"},
		{ID: "L5.1", Status: "✅", Surface: "L5"},
		{ID: "L6.4", Status: "🔴", Surface: "L6"}, // operator-action-only — must not block
		{ID: "L7.1", Status: "⏳", Surface: "L7"},
	}
	if !engineeringReady(rows) {
		t.Error("engineeringReady should be true when L1-L5 are ✅, L6/L7 should be ignored")
	}
}

// TestEngineeringReady_OpsRunbookOK — 🟡 (operator-runbook-ready)
// is acceptable for L4/L5 but NOT for L1-L3.
func TestEngineeringReady_OpsRunbookOK(t *testing.T) {
	t.Run("L4_yellow_ok", func(t *testing.T) {
		rows := []Row{{ID: "L4.11", Status: "🟡", Surface: "L4"}}
		if !engineeringReady(rows) {
			t.Error("L4 with 🟡 should be ready (operator-runbook gated)")
		}
	})
	t.Run("L3_yellow_blocks", func(t *testing.T) {
		rows := []Row{{ID: "L3.5", Status: "🟡", Surface: "L3"}}
		if engineeringReady(rows) {
			t.Error("L3 with 🟡 should NOT be ready — engineering tier requires ✅/⚠")
		}
	})
}

// TestEngineeringReady_CaveatOK — ⚠ (shipped-with-caveat) is
// acceptable across all engineering + ops tiers.
func TestEngineeringReady_CaveatOK(t *testing.T) {
	rows := []Row{
		{ID: "L2.2", Status: "⚠", Surface: "L2"},
		{ID: "L5.4", Status: "⚠", Surface: "L5"},
	}
	if !engineeringReady(rows) {
		t.Error("⚠ should count as ready in both engineering and ops tiers")
	}
}

// TestEngineeringReady_GreenBlocks — 🟢 (in flight) blocks every
// engineering + ops tier.
func TestEngineeringReady_GreenBlocks(t *testing.T) {
	rows := []Row{{ID: "L3.9", Status: "🟢", Surface: "L3"}}
	if engineeringReady(rows) {
		t.Error("🟢 in L3 should block")
	}
}

// TestNormaliseStatus — picks the first known emoji from the column.
func TestNormaliseStatus(t *testing.T) {
	cases := map[string]string{
		"✅":           "✅",
		"🟢":           "🟢",
		"⚠":           "⚠",
		"🟡 designed":  "🟡",
		"shipped ✅":   "✅",
		"random text": "?",
		"":            "?",
	}
	for in, want := range cases {
		if got := normaliseStatus(in); got != want {
			t.Errorf("normaliseStatus(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestParse_HandlesPipesInDescription — descriptions can contain
// `|` characters (e.g. backtick-wrapped paths or markdown links).
// The status is always the LAST non-empty cell, so even a row
// where the description wraps multiple `|` should land the right
// status.
func TestParse_HandlesPipesInDescription(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "backlog.md")
	body := "# Header\n\n" +
		"| ID | Item | Status |\n" +
		"|---|---|---|\n" +
		"| L1.1 | Description with `|` pipe-ish | ✅ |\n" +
		"| L2.1 | Plain description | 🟢 |\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	rows, err := parseFile(path)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].Status != "✅" || rows[1].Status != "🟢" {
		t.Errorf("statuses = [%q, %q], want [✅, 🟢]", rows[0].Status, rows[1].Status)
	}
}

// TestSurfaceFor — extracts the first two characters as the surface.
func TestSurfaceFor(t *testing.T) {
	cases := map[string]string{
		"L1.1":   "L1",
		"L2.12a": "L2",
		"L7.7":   "L7",
		"":       "",
		"?":      "",
	}
	for in, want := range cases {
		if got := surfaceFor(in); got != want {
			t.Errorf("surfaceFor(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSurfaceReadiness_NamesBlocker — when a row blocks, the
// returned reason names the row ID so the operator knows which
// to chase.
func TestSurfaceReadiness_NamesBlocker(t *testing.T) {
	rows := []Row{
		{ID: "L3.5", Status: "🟢", Surface: "L3"},
	}
	ready, reason := surfaceReadiness("L3", rows)
	if ready {
		t.Fatal("expected not ready")
	}
	if !strings.Contains(reason, "L3.5") {
		t.Errorf("reason should name L3.5; got %q", reason)
	}
}

// TestParseSkipIDs — accepts a comma-separated list with arbitrary
// whitespace; empty input is nil.
func TestParseSkipIDs(t *testing.T) {
	t.Run("empty_returns_nil", func(t *testing.T) {
		if got := parseSkipIDs(""); got != nil {
			t.Errorf("empty input should be nil, got %v", got)
		}
		if got := parseSkipIDs("   "); got != nil {
			t.Errorf("whitespace-only input should be nil, got %v", got)
		}
	})
	t.Run("comma_separated", func(t *testing.T) {
		got := parseSkipIDs("L4.14, L4.15 ,L5.6,,")
		if len(got) != 3 {
			t.Fatalf("got %d entries, want 3: %v", len(got), got)
		}
		for _, id := range []string{"L4.14", "L4.15", "L5.6"} {
			if _, ok := got[id]; !ok {
				t.Errorf("missing %q in skip set", id)
			}
		}
	})
}

// TestEngineeringReady_SkipFlipsVerdict — a 🔴 row that is in the
// skip set must not block the verdict, but other 🔴 rows still do.
// This pins the `-skip-ids` flag's gating semantics — the row
// stays visible in the report with its real status; only the
// engineering-ready verdict ignores it.
func TestEngineeringReady_SkipFlipsVerdict(t *testing.T) {
	rows := []Row{
		{ID: "L1.1", Status: "✅", Surface: "L1"},
		{ID: "L2.1", Status: "✅", Surface: "L2"},
		{ID: "L3.1", Status: "✅", Surface: "L3"},
		{ID: "L4.1", Status: "✅", Surface: "L4"},
		{ID: "L4.14", Status: "🔴", Surface: "L4"}, // multi-region — skipped in single-region mode
		{ID: "L5.1", Status: "✅", Surface: "L5"},
	}
	if engineeringReady(rows) {
		t.Fatal("baseline must NOT be ready: L4.14 is 🔴 and unskipped")
	}
	if !engineeringReadyWithSkip(rows, map[string]struct{}{"L4.14": {}}) {
		t.Error("with L4.14 skipped, the surface must be ready")
	}
	// Skipping the wrong row leaves the verdict false.
	if engineeringReadyWithSkip(rows, map[string]struct{}{"L4.99": {}}) {
		t.Error("skipping a non-matching ID must not flip the verdict")
	}
}

// TestRealBacklog_SingleRegionPosture — pins the project's
// "live-in-development on R1" posture: skipping the multi-region
// + chaos + external-security rows (L4.14-17, L5.6, L5.8) should
// produce a green verdict against the real backlog. If a NEW
// engineering-tier blocker is introduced this test fails — that's
// the intended regression signal.
func TestRealBacklog_SingleRegionPosture(t *testing.T) {
	const path = "../../../docs/architecture/launch-readiness-backlog.md"
	rows, err := parseFile(path)
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	skip := parseSkipIDs("L4.14,L4.15,L4.16,L4.17,L5.6,L5.8")
	if !engineeringReadyWithSkip(rows, skip) {
		blockers := collectBlockersWithSkip(rows, skip)
		ids := make([]string, 0, len(blockers))
		for _, b := range blockers {
			ids = append(ids, b.ID+"="+b.Status)
		}
		t.Errorf("single-region posture should be ready; remaining blockers: %v", ids)
	}
}
