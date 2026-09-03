package timescale

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

// ReapCursors is the only irreversible statement in the cursor-cleanup
// path, and its safety lives entirely in three predicates. The Go-side
// planner in `stellarindex-ops reap-cursors` applies the same rules, but
// the planner's output does NOT constrain this DELETE: the statement is
// predicated on the cutoff, not on the previewed keys, so a dropped
// clause here deletes rows no preview ever showed. Pin all three.
func TestReapCursors_DeletePredicates(t *testing.T) {
	cutoff := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)

	store, conn := newScriptedStore(t, scriptedResult{rowsAffected: 4703})

	n, err := store.ReapCursors(context.Background(), cutoff, "projected-rebuild", LiveCursorSources())
	if err != nil {
		t.Fatalf("ReapCursors: %v", err)
	}
	if n != 4703 {
		t.Errorf("deleted = %d, want the driver's 4703", n)
	}

	stmt := conn.only(t)
	q := normaliseSQL(stmt.sql)

	if !strings.HasPrefix(q, "DELETE FROM ingestion_cursors ") {
		t.Fatalf("statement is not a DELETE against ingestion_cursors:\n%s", stmt.sql)
	}
	// The age bound. Without it the statement deletes the whole table.
	if !strings.Contains(q, "WHERE last_updated < $1") {
		t.Errorf("DELETE is missing the `last_updated < $1` cutoff — without it every cursor is in range:\n%s", stmt.sql)
	}
	// The -source narrowing, with its empty-means-every-source escape.
	// A bare `source = $2` would delete nothing on an unscoped run; a
	// missing clause would ignore -source and delete every job's shards.
	if !strings.Contains(q, "AND ($2 = '' OR source = $2)") {
		t.Errorf("DELETE is missing the `($2 = '' OR source = $2)` scope clause:\n%s", stmt.sql)
	}
	// The live-namespace guard. This is the second of the two copies —
	// the planner is the first — and the only one that binds when the
	// statement runs.
	if !strings.Contains(q, "AND source <> ALL($3)") {
		t.Errorf("DELETE is missing the `source <> ALL($3)` guard — a stuck ledgerstream/projector row would be deleted, turning a lag into a restart from the configured start ledger:\n%s", stmt.sql)
	}

	wantTime(t, stmt.arg(t, 1), cutoff)
	if got := stmt.arg(t, 2); got != "projected-rebuild" {
		t.Errorf("$2 = %v, want the -source value", got)
	}
	// $3 must reach the driver as a Go []string: pgx's stdlib driver
	// encodes that as a Postgres text[], which is what ALL() needs. A
	// scalar or a joined string binds as text and the guard stops
	// matching.
	protected, ok := stmt.arg(t, 3).([]string)
	if !ok {
		t.Fatalf("$3 is %T (%v), want a []string bound as text[]", stmt.arg(t, 3), stmt.arg(t, 3))
	}
	for _, src := range LiveCursorSources() {
		if !slices.Contains(protected, src) {
			t.Errorf("$3 = %v, missing live namespace %q", protected, src)
		}
	}
}

// A nil protected list must still bind an empty array rather than NULL:
// `source <> ALL(NULL)` is NULL, not true, so every row would fail the
// predicate and the reap would silently delete nothing.
func TestReapCursors_NilProtectedBindsEmptyArray(t *testing.T) {
	store, conn := newScriptedStore(t, scriptedResult{rowsAffected: 0})

	if _, err := store.ReapCursors(context.Background(), time.Now().UTC(), "", nil); err != nil {
		t.Fatalf("ReapCursors: %v", err)
	}
	got := conn.only(t).arg(t, 3)
	arr, ok := got.([]string)
	if !ok {
		t.Fatalf("$3 is %T (%v), want an empty []string", got, got)
	}
	// A nil slice is what pgx encodes as SQL NULL, and that is the
	// failure being pinned: it must reach the driver as a non-nil empty
	// slice, i.e. an empty text[]. Length alone cannot tell the two
	// apart.
	if arr == nil {
		t.Fatal("$3 is a nil []string — pgx encodes that as NULL, and `source <> ALL(NULL)` is NULL for every row, so the reap would silently delete nothing")
	}
	if len(arr) != 0 {
		t.Errorf("$3 = %v, want empty", arr)
	}
}

// The live namespaces are ONE list with two consumers that must agree:
// reap-cursors refuses to delete them, and /v1/diagnostics/cursors
// refuses to classify them abandoned. Both read this.
func TestLiveCursorSources(t *testing.T) {
	for _, src := range []string{"ledgerstream", "projector"} {
		if !IsLiveCursorSource(src) {
			t.Errorf("IsLiveCursorSource(%q) = false, want true", src)
		}
	}
	for _, src := range []string{"backfill", "projected-rebuild", "census-backfill", "gap-detector-scan", ""} {
		if IsLiveCursorSource(src) {
			t.Errorf("IsLiveCursorSource(%q) = true — a one-shot job's shards are reapable and can go abandoned", src)
		}
	}
	// The accessor hands out a copy: a caller that sorts or appends to
	// the returned slice must not reach the shared list.
	got := LiveCursorSources()
	got[0] = "clobbered"
	if !IsLiveCursorSource("ledgerstream") {
		t.Error("mutating the returned slice changed the package list")
	}
}

// normaliseSQL collapses the indentation of a raw-string query so a
// predicate assertion reads as one line.
func normaliseSQL(q string) string {
	return strings.TrimSpace(regexp.MustCompile(`\s+`).ReplaceAllString(q, " "))
}
