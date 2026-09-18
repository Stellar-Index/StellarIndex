package timescale

import (
	"context"
	"strings"
	"testing"
)

// TestBuildDirectoryUpsert_ConflictArmIsOwnershipScoped — the conflict
// arm must update only rows the syncing source already OWNS.
//
// Two things broke when it did not. Migration 0136 promises the table is
// "scoped by `source` so a future second directory source can coexist
// without the syncs deleting each other's rows", and an unconditional
// `source = EXCLUDED.source` makes every sync steal every shared address
// from the other. And with `source` rewritten there is no durable
// operator override for a false-positive scam flag at all: a hand-held
// correction is adopted into the upstream snapshot and overwritten by
// the next daily run, while the issuer's price stays withheld.
func TestBuildDirectoryUpsert_ConflictArmIsOwnershipScoped(t *testing.T) {
	q, _ := buildDirectoryUpsert(directoryEntriesN(2), "stellar-expert")

	if !strings.Contains(q, "WHERE account_directory.source = EXCLUDED.source") {
		t.Errorf("conflict arm is not ownership-scoped — a sync would overwrite rows another source owns:\n%s", q)
	}
	// `source` must never appear in the SET list: the WHERE already pins
	// it equal, and rewriting it is exactly how a row changed hands.
	// EXCLUDED.source may therefore be referenced exactly once, by the
	// guard.
	if got := strings.Count(q, "EXCLUDED.source"); got != 1 {
		t.Errorf("EXCLUDED.source referenced %d times, want 1 (the ownership guard only):\n%s", got, q)
	}
}

// TestReplaceDirectory_RefusesReservedOverrideSource — a sync running as
// the operator source would adopt every override into its snapshot and
// then prune the ones that snapshot omits, deleting the corrections the
// source exists to protect. Must refuse before touching the DB (s.db is
// nil here, so a DB call would panic — reaching the guard proves order).
func TestReplaceDirectory_RefusesReservedOverrideSource(t *testing.T) {
	s := &Store{}
	_, _, err := s.ReplaceDirectory(context.Background(), DirectoryOperatorOverrideSource, directoryEntriesN(1))
	if err == nil {
		t.Fatal("ReplaceDirectory(operator-override source) = nil error, want refusal")
	}
	if !strings.Contains(err.Error(), DirectoryOperatorOverrideSource) {
		t.Errorf("error %q does not name the reserved source", err)
	}
}
