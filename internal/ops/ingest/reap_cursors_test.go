package ingest

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func mkReapCursor(source, sub string, age time.Duration) timescale.Cursor {
	return timescale.Cursor{
		Source:    source,
		Sub:       sub,
		UpdatedAt: time.Now().UTC().Add(-age),
	}
}

// The selection rules, against the population that produced this
// subcommand: r1's table held 4,703 rows nobody had written to in over
// a week, alongside a live ledgerstream cursor that must survive any
// reap.
func TestPlanReapCursors_SelectsOnlyAbandonedNonProtectedRows(t *testing.T) {
	now := time.Now().UTC()
	rows := []timescale.Cursor{
		mkReapCursor("ledgerstream", "", 5*time.Second),
		mkReapCursor("projector", "soroswap", 30*time.Second),
		mkReapCursor("backfill", "2-15300000:sdex", 112*24*time.Hour),
		mkReapCursor("backfill", "30600000-45899998:sdex", 112*24*time.Hour),
		mkReapCursor("projected-rebuild", "shard-1", 40*24*time.Hour),
		mkReapCursor("backfill", "still-running", 20*time.Minute),
	}

	plan := planReapCursors(rows, now.Add(-7*24*time.Hour), "")

	if len(plan.candidates) != 3 {
		t.Fatalf("got %d candidates, want 3 (two sdex shards + one projected-rebuild shard); got %+v",
			len(plan.candidates), plan.candidates)
	}
	for _, c := range plan.candidates {
		if isReapProtected(c.Source) {
			t.Errorf("protected source %q reached the delete plan", c.Source)
		}
		if c.Sub == "still-running" {
			t.Error("a 20-minute-old cursor reached the delete plan — only rows past the cutoff are reapable")
		}
	}
	if plan.bySource["backfill"] != 2 || plan.bySource["projected-rebuild"] != 1 {
		t.Errorf("bySource = %v, want backfill=2 projected-rebuild=1", plan.bySource)
	}
}

// A live cursor past the cutoff is spared AND reported: that state is
// "ingest is stuck", and deleting the row would turn a lag into a
// restart from the configured start ledger.
func TestPlanReapCursors_StaleProtectedRowIsSparedAndSurfaced(t *testing.T) {
	now := time.Now().UTC()
	rows := []timescale.Cursor{
		mkReapCursor("ledgerstream", "", 30*24*time.Hour),
		mkReapCursor("projector", "blend", 30*24*time.Hour),
		mkReapCursor("census-backfill", "shard-9", 30*24*time.Hour),
	}

	plan := planReapCursors(rows, now.Add(-7*24*time.Hour), "")

	if len(plan.candidates) != 1 || plan.candidates[0].Source != "census-backfill" {
		t.Fatalf("candidates = %+v, want only the census-backfill shard", plan.candidates)
	}
	if len(plan.protected) != 2 {
		t.Fatalf("got %d protected rows, want 2 (ledgerstream + projector reported as stuck)", len(plan.protected))
	}
}

// -source narrows the plan to one job's shards without changing the
// age rule.
func TestPlanReapCursors_SourceFilter(t *testing.T) {
	now := time.Now().UTC()
	rows := []timescale.Cursor{
		mkReapCursor("backfill", "old", 30*24*time.Hour),
		mkReapCursor("projected-rebuild", "old", 30*24*time.Hour),
	}

	plan := planReapCursors(rows, now.Add(-7*24*time.Hour), "projected-rebuild")

	if len(plan.candidates) != 1 || plan.candidates[0].Source != "projected-rebuild" {
		t.Fatalf("candidates = %+v, want only the projected-rebuild shard", plan.candidates)
	}
}

// The preview IS the review step, so it has to carry the three things
// an operator decides on: what would go, what was spared and why, and
// that nothing has been deleted yet.
func TestPrintReapPlan_PreviewCarriesTheReview(t *testing.T) {
	now := time.Now().UTC()
	cutoff := now.Add(-7 * 24 * time.Hour)
	plan := planReapCursors([]timescale.Cursor{
		mkReapCursor("backfill", "2-15300000:sdex", 112*24*time.Hour),
		mkReapCursor("projected-rebuild", "shard-1", 40*24*time.Hour),
		mkReapCursor("ledgerstream", "", 9*24*time.Hour),
	}, cutoff, "")

	var buf bytes.Buffer
	printReapPlan(&buf, plan, cutoff, reapCursorsOpts{olderThan: 7 * 24 * time.Hour})
	out := buf.String()

	for _, want := range []string{
		"2-15300000:sdex",    // the rows themselves
		"TOTAL",              // exact counts
		"NOT reaped",         // the spared live cursor
		"re-run with -write", // nothing deleted yet
	} {
		if !strings.Contains(out, want) {
			t.Errorf("preview is missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "deleted") {
		t.Errorf("preview claims a deletion:\n%s", out)
	}
}

// A plan that is nothing BUT a stuck live cursor is the case the
// warning matters most in, and the one an early return is easiest to
// lose it to: the table is otherwise clean, so the stuck cursor is the
// only thing the run has to say. It must still be reported, and the
// "nothing to reap" line must not claim no cursor is that old when one
// plainly is.
func TestPrintReapPlan_ProtectedOnlyPlanStillReportsTheStuckCursor(t *testing.T) {
	now := time.Now().UTC()
	cutoff := now.Add(-7 * 24 * time.Hour)
	plan := planReapCursors([]timescale.Cursor{
		mkReapCursor("ledgerstream", "", 30*24*time.Hour),
	}, cutoff, "")
	if len(plan.candidates) != 0 || len(plan.protected) != 1 {
		t.Fatalf("fixture is not protected-only: %d candidates, %d protected", len(plan.candidates), len(plan.protected))
	}

	var buf bytes.Buffer
	printReapPlan(&buf, plan, cutoff, reapCursorsOpts{olderThan: 7 * 24 * time.Hour})
	out := buf.String()

	if !strings.Contains(out, "ledgerstream") || !strings.Contains(out, "NOT reaped") {
		t.Errorf("protected-only preview does not report the stuck live cursor:\n%s", out)
	}
	if strings.Contains(out, "no cursor is that old") {
		t.Errorf("preview claims no cursor is past the cutoff while reporting one that is:\n%s", out)
	}
	if !strings.Contains(out, "protected live namespace") {
		t.Errorf("preview does not say why there is nothing to reap:\n%s", out)
	}
}

// The empty case keeps its own line: nothing past the cutoff at all is
// a different fact from everything past it being protected.
func TestPrintReapPlan_EmptyPlanSaysNoCursorIsThatOld(t *testing.T) {
	now := time.Now().UTC()
	cutoff := now.Add(-7 * 24 * time.Hour)
	plan := planReapCursors([]timescale.Cursor{
		mkReapCursor("ledgerstream", "", 5*time.Second),
		mkReapCursor("backfill", "running", 20*time.Minute),
	}, cutoff, "")

	var buf bytes.Buffer
	printReapPlan(&buf, plan, cutoff, reapCursorsOpts{olderThan: 7 * 24 * time.Hour})
	out := buf.String()

	if !strings.Contains(out, "no cursor is that old") {
		t.Errorf("empty preview is missing its explanation:\n%s", out)
	}
	if strings.Contains(out, "NOT reaped") {
		t.Errorf("empty preview warns about a stuck cursor it does not have:\n%s", out)
	}
}

// The flag posture: preview by default, a floor under -older-than, and
// a refusal to aim the subcommand at a live cursor namespace.
func TestParseReapCursorsFlags_Posture(t *testing.T) {
	opts, err := parseReapCursorsFlags([]string{"-config", "/etc/stellarindex.toml"})
	if err != nil {
		t.Fatalf("defaults rejected: %v", err)
	}
	if opts.write {
		t.Error("write = true without -write — reap-cursors must preview by default")
	}
	if opts.olderThan != reapDefaultAge {
		t.Errorf("olderThan = %s, want the %s default", opts.olderThan, reapDefaultAge)
	}

	opts, err = parseReapCursorsFlags([]string{"-config", "/etc/stellarindex.toml", "-write"})
	if err != nil {
		t.Fatalf("-write rejected: %v", err)
	}
	if !opts.write {
		t.Error("write = false with -write")
	}

	if _, err = parseReapCursorsFlags([]string{"-older-than", "48h"}); err == nil {
		t.Error("missing -config accepted")
	}

	_, err = parseReapCursorsFlags([]string{"-config", "/etc/stellarindex.toml", "-older-than", "1h"})
	if err == nil || !strings.Contains(err.Error(), "floor") {
		t.Errorf("-older-than 1h error = %v, want a refusal naming the %s floor", err, reapMinAge)
	}

	_, err = parseReapCursorsFlags([]string{"-config", "/etc/stellarindex.toml", "-source", "ledgerstream"})
	if err == nil {
		t.Error("-source ledgerstream accepted — the live cursor namespace must be refused outright")
	}
}
