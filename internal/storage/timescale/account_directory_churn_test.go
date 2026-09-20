package timescale

import "testing"

// TestDirectoryChurnLimit_Ceiling pins the arithmetic behind the sync
// refusal: 5 % of the held rows, never below the floor, unbounded
// for a source's first sync and for the operator's explicit opt-in.
// The refusal itself runs against Postgres in
// test/integration/account_directory_churn_test.go.
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
