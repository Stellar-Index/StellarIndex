package timescale

import (
	"context"
	"math/big"
	"strings"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/domain"
)

// TestInsertAccountObservation_RejectsEmptyAccountID: a zero-value AccountID is a caller bug; fail loudly
// rather than let the DB reject it or store an empty-string PK that no reader query will find.
func TestInsertAccountObservation_RejectsEmptyAccountID(t *testing.T) {
	// nil *sql.DB — the validation guard fires before any DB call.
	s := &Store{}
	err := s.InsertAccountObservation(context.Background(), domain.AccountObservation{
		Balance: big.NewInt(0),
	})
	if err == nil {
		t.Fatal("expected error on empty AccountID; got nil")
	}
	if !strings.Contains(err.Error(), "AccountID") {
		t.Errorf("err=%v should mention AccountID", err)
	}
}

// TestInsertAccountObservation_RejectsNilBalance — silently
// converting nil Balance to zero would publish wrong-but-plausible
// values to readers. Reject loudly.
func TestInsertAccountObservation_RejectsNilBalance(t *testing.T) {
	s := &Store{}
	err := s.InsertAccountObservation(context.Background(), domain.AccountObservation{
		AccountID: "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF",
	})
	if err == nil {
		t.Fatal("expected error on nil Balance; got nil")
	}
	if !strings.Contains(err.Error(), "Balance") {
		t.Errorf("err=%v should mention Balance", err)
	}
	if !strings.Contains(err.Error(), "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAWHF") {
		t.Errorf("err=%v should include the offending AccountID", err)
	}
}

// These tests cover the Upsert defensive guards; the real Postgres round-trip (overwrite, not duplicate,
// the singleton row) needs testcontainers-go and lives in test/integration/ per CONTRIBUTING.md.

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

// These tests cover the Upsert/Read defensive guards — the real
// round-trip against Postgres needs testcontainers-go and lives in
// test/integration/ (per CONTRIBUTING.md §Testing).

func TestUpsertSACBalanceSeedProvenance_RejectsInvalidRows(t *testing.T) {
	const (
		contract = "CBZ7M5B3Y4WWBZ5XK5UZCAFOEZ23KSSZXYECYX3IXM6E2JOLQC52DK32"
		asset    = "PHO:GAX5..."
	)
	cases := []struct {
		name       string
		in         SACBalanceSeedProvenance
		wantSubstr string
	}{
		{"empty contract", SACBalanceSeedProvenance{AssetKey: asset, Source: SACBalanceSeedSourceFullHistory}, "ContractID"},
		{"empty asset", SACBalanceSeedProvenance{ContractID: contract, Source: SACBalanceSeedSourceFullHistory}, "AssetKey"},
		{"invalid source", SACBalanceSeedProvenance{ContractID: contract, AssetKey: asset, Source: "made_up_source"}, "invalid source"},
		{"negative holders", SACBalanceSeedProvenance{
			ContractID: contract, AssetKey: asset, Source: SACBalanceSeedSourceCurrentState, HoldersSeeded: -1,
		}, "negative"},
		{"full_history without LakeVerifiedThrough", SACBalanceSeedProvenance{
			ContractID: contract, AssetKey: asset, Source: SACBalanceSeedSourceFullHistory,
		}, "LakeVerifiedThrough"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := (&Store{}).UpsertSACBalanceSeedProvenance(context.Background(), tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("err=%v should mention %s", err, tc.wantSubstr)
			}
		})
	}
}

func TestSACBalanceSeedProvenanceFor_RejectsEmptyContractID(t *testing.T) {
	s := &Store{}
	_, _, err := s.SACBalanceSeedProvenanceFor(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "contractID") {
		t.Errorf("err=%v should mention contractID", err)
	}
}
