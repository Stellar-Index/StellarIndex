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

func TestUpsertAccountObservationSeedProvenance_RejectsNoWatchlist(t *testing.T) {
	s := &Store{}
	err := s.UpsertAccountObservationSeedProvenance(context.Background(), AccountObservationSeedProvenance{})
	if err == nil || !strings.Contains(err.Error(), "AccountsWatched") {
		t.Errorf("err=%v should refuse a pass with no watchlist", err)
	}
}

func TestUpsertAccountObservationSeedProvenance_RejectsNegativeCounts(t *testing.T) {
	s := &Store{}
	err := s.UpsertAccountObservationSeedProvenance(context.Background(), AccountObservationSeedProvenance{
		AccountsWatched: 3,
		AccountsSeeded:  -1,
	})
	if err == nil || !strings.Contains(err.Error(), "negative") {
		t.Errorf("err=%v should mention a negative count", err)
	}
}

func TestUpsertAccountObservationSeedProvenance_RejectsPartialCoverage(t *testing.T) {
	s := &Store{}
	// 1 seeded + 1 missing = 2, but the watchlist has 3: a partial or
	// failed pass must not claim it covered the whole watchlist.
	err := s.UpsertAccountObservationSeedProvenance(context.Background(), AccountObservationSeedProvenance{
		AccountsWatched: 3,
		AccountsSeeded:  1,
		AccountsMissing: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "did not cover the whole watchlist") {
		t.Errorf("err=%v should refuse counts that don't sum to AccountsWatched", err)
	}
}

func TestUpsertAccountObservationSeedProvenance_RejectsWatchedAccountsCardinalityMismatch(t *testing.T) {
	s := &Store{}
	// AccountsWatched says 3, but only 2 G-strkeys are given: a count
	// unsupported by an actual account list is not traceable.
	err := s.UpsertAccountObservationSeedProvenance(context.Background(), AccountObservationSeedProvenance{
		AccountsWatched: 3,
		WatchedAccounts: []string{"GA", "GB"},
		AccountsSeeded:  3,
	})
	if err == nil || !strings.Contains(err.Error(), "WatchedAccounts") {
		t.Errorf("err=%v should refuse a WatchedAccounts/AccountsWatched cardinality mismatch", err)
	}
}

func TestUpsertAccountObservationSeedProvenance_RejectsMissingAccountsCardinalityMismatch(t *testing.T) {
	s := &Store{}
	// AccountsMissing says 1, but 2 G-strkeys are given.
	err := s.UpsertAccountObservationSeedProvenance(context.Background(), AccountObservationSeedProvenance{
		AccountsWatched: 3,
		WatchedAccounts: []string{"GA", "GB", "GC"},
		AccountsSeeded:  2,
		AccountsMissing: 1,
		MissingAccounts: []string{"GA", "GB"},
	})
	if err == nil || !strings.Contains(err.Error(), "MissingAccounts") {
		t.Errorf("err=%v should refuse a MissingAccounts/AccountsMissing cardinality mismatch", err)
	}
}
