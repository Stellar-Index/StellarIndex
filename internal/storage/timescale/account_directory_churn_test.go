package timescale

import (
	"errors"
	"fmt"
	"testing"
)

// TestDirectoryChurnLimit_Ceiling pins the arithmetic behind the sync
// refusal: 5 % of the held rows, never below the floor, unbounded
// for a source's first sync and for the operator's explicit opt-in.
// The refusal itself runs against Postgres in
// test/integration/pg_accounts_test.go.
func TestDirectoryChurnLimit_Ceiling(t *testing.T) {
	cases := []struct {
		name     string
		limit    DirectoryChurnLimit
		existing int64
		want     int64
		bounded  bool
	}{
		{"default over the live table", DefaultDirectoryChurnLimit, 18500, 925, true},
		{"default rounds up", DefaultDirectoryChurnLimit, 18501, 926, true},
		{"floor holds a small table open", DefaultDirectoryChurnLimit, 2, 100, true},
		{"floor is the minimum, not an offset", DefaultDirectoryChurnLimit, 2000, 100, true},
		{"first sync is unbounded", DefaultDirectoryChurnLimit, 0, 0, false},
		{"operator opt-in is unbounded", DirectoryChurnUnbounded, 18500, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, bounded := tc.limit.ceiling(tc.existing)
			if bounded != tc.bounded || got != tc.want {
				t.Fatalf("ceiling(%d) = (%d, %v), want (%d, %v)", tc.existing, got, bounded, tc.want, tc.bounded)
			}
		})
	}
}

// TestDirectoryChurn_UnflagCeilingIsSizedOnTheFlaggedSet: the un-flag
// cap is a fraction of the addresses flagged before the sync, not of
// every row the source holds. Flagged rows are a minority, so a
// row-sized cap (925 on the live table) admitted clearing almost the
// whole flagged set in one run.
func TestDirectoryChurn_UnflagCeilingIsSizedOnTheFlaggedSet(t *testing.T) {
	cases := []struct {
		name      string
		rows      int64
		flagged   int
		unflagged int64
		refused   bool
	}{
		{"row-sized cap no longer admits clearing the set", 18500, 1000, 925, true},
		{"one over 5 % of the flagged set", 18500, 1000, 51, true},
		{"5 % of the flagged set", 18500, 1000, 50, false},
		{"floor holds a small flagged set open", 18500, 40, 10, false},
		{"floor is the minimum, not an offset", 18500, 40, 11, true},
		{"whole small flagged set", 18500, 40, 40, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := &directoryChurn{rows: tc.rows, flagged: map[string]struct{}{}}
			for i := range tc.flagged {
				before.flagged[fmt.Sprintf("G%055d", i)] = struct{}{}
			}
			err := before.check(DefaultDirectoryChurnLimit, DirectorySyncResult{Unflagged: tc.unflagged})
			if got := errors.Is(err, ErrDirectoryChurnExceeded); got != tc.refused {
				t.Fatalf("%d of %d flagged un-flagged: err = %v, refused = %v, want %v", tc.unflagged, tc.flagged, err, got, tc.refused)
			}
			if err := before.check(DirectoryChurnUnbounded, DirectorySyncResult{Unflagged: tc.unflagged}); err != nil {
				t.Fatalf("-accept-churn refused %d un-flags: %v", tc.unflagged, err)
			}
		})
	}
}

func TestDirectoryChurnLimit_UnflagCeiling(t *testing.T) {
	if got, ok := DefaultDirectoryChurnLimit.unflagCeiling(1001); !ok || got != 51 {
		t.Fatalf("unflagCeiling(1001) = (%d, %v), want (51, true): rounds up", got, ok)
	}
	if _, ok := DefaultDirectoryChurnLimit.unflagCeiling(0); ok {
		t.Fatal("unflagCeiling(0) bounded, want unbounded: nothing flagged, nothing to clear")
	}
	if _, ok := DirectoryChurnUnbounded.unflagCeiling(1000); ok {
		t.Fatal("DirectoryChurnUnbounded.unflagCeiling bounded, want unbounded")
	}
}

// TestReplaceDirectoryWithin_RefusesBeforeTheDB — the argument checks
// run before any DB call (s.db is nil: reaching one would panic), for
// the bounded entry point exactly as for ReplaceDirectory.
func TestReplaceDirectoryWithin_RefusesBeforeTheDB(t *testing.T) {
	s := &Store{}
	ctx := t.Context()
	if _, err := s.ReplaceDirectoryWithin(ctx, "stellar-expert", nil, DirectoryChurnUnbounded); err == nil {
		t.Fatal("ReplaceDirectoryWithin(empty) = nil error, want refusal")
	}
	if _, err := s.ReplaceDirectoryWithin(ctx, DirectoryOperatorOverrideSource, directoryEntriesN(1), DirectoryChurnUnbounded); err == nil {
		t.Fatal("ReplaceDirectoryWithin(operator-override source) = nil error, want refusal")
	}
}
