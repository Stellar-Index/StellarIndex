package timescale

import (
	"context"
	"strings"
	"testing"
)

// These tests cover the Upsert defensive guards — the real round-trip
// against Postgres (an upsert overwriting the singleton row, not
// duplicating it) needs testcontainers-go and lives in test/integration/
// (per CONTRIBUTING.md §Testing), mirroring
// TestClaimableSeedProvenanceRoundTrip.

func TestUpsertAccountObservationSeedProvenance_RejectsInvalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   AccountObservationSeedProvenance
		want string
	}{
		{"no watchlist", AccountObservationSeedProvenance{}, "AccountsWatched"},
		{"negative count", AccountObservationSeedProvenance{AccountsWatched: 3, AccountsSeeded: -1}, "negative"},
		// 1 seeded + 1 missing = 2 of 3: a partial pass must not claim
		// it covered the whole watchlist.
		{
			"partial coverage",
			AccountObservationSeedProvenance{AccountsWatched: 3, AccountsSeeded: 1, AccountsMissing: 1},
			"did not cover the whole watchlist",
		},
		// A count unsupported by an actual account list is not traceable.
		{"watched cardinality mismatch", AccountObservationSeedProvenance{
			AccountsWatched: 3, WatchedAccounts: []string{"GA", "GB"}, AccountsSeeded: 3,
		}, "WatchedAccounts"},
		{"missing cardinality mismatch", AccountObservationSeedProvenance{
			AccountsWatched: 3, WatchedAccounts: []string{"GA", "GB", "GC"}, AccountsSeeded: 2,
			AccountsMissing: 1, MissingAccounts: []string{"GA", "GB"},
		}, "MissingAccounts"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Store{}
			err := s.UpsertAccountObservationSeedProvenance(context.Background(), tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err=%v should be refused and mention %q", err, tc.want)
			}
		})
	}
}
