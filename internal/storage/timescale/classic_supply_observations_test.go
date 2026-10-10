package timescale

import (
	"context"
	"math/big"
	"strings"
	"testing"
	"time"
)

// These tests cover the Insert*Observation defensive guards —
// the Sum* methods need a real DB and live in test/integration/
// (per CONTRIBUTING.md §Testing, integration tests run
// via testcontainers-go).

// TestInsertObservation_RejectsMissingFields: each single-row writer
// refuses a row missing an identity field or its balance, and names it.
func TestInsertObservation_RejectsMissingFields(t *testing.T) {
	ctx := context.Background()
	zero := big.NewInt(0)
	const asset = "USDC:GA5..."
	cases := []struct {
		name       string
		call       func(*Store) error
		wantSubstr string
	}{
		{"trustline/AccountID", func(s *Store) error {
			return s.InsertTrustlineObservation(ctx, TrustlineObservation{AssetKey: asset, Balance: zero})
		}, "AccountID"},
		{"trustline/AssetKey", func(s *Store) error {
			return s.InsertTrustlineObservation(ctx, TrustlineObservation{AccountID: "GA1", Balance: zero})
		}, "AssetKey"},
		{"trustline/Balance", func(s *Store) error {
			return s.InsertTrustlineObservation(ctx, TrustlineObservation{AccountID: "GA1", AssetKey: asset})
		}, "Balance"},
		{"claimable/ClaimableID", func(s *Store) error {
			return s.InsertClaimableObservation(ctx, ClaimableObservation{AssetKey: asset, Balance: zero})
		}, "ClaimableID"},
		{"claimable/AssetKey", func(s *Store) error {
			return s.InsertClaimableObservation(ctx, ClaimableObservation{ClaimableID: "abc", Balance: zero})
		}, "AssetKey"},
		{"claimable/Balance", func(s *Store) error {
			return s.InsertClaimableObservation(ctx, ClaimableObservation{ClaimableID: "abc", AssetKey: asset})
		}, "Balance"},
		{"lp_reserve/PoolID", func(s *Store) error {
			return s.InsertLPReserveObservation(ctx, LPReserveObservation{AssetKey: asset, Balance: zero})
		}, "PoolID"},
		{"lp_reserve/AssetKey", func(s *Store) error {
			return s.InsertLPReserveObservation(ctx, LPReserveObservation{PoolID: "deadbeef", Balance: zero})
		}, "AssetKey"},
		{"lp_reserve/Balance", func(s *Store) error {
			return s.InsertLPReserveObservation(ctx, LPReserveObservation{PoolID: "deadbeef", AssetKey: asset})
		}, "Balance"},
		{"sac_balance/ContractID", func(s *Store) error {
			return s.InsertSACBalanceObservation(ctx, SACBalanceObservation{AssetKey: asset, Holder: "GA1", Balance: zero})
		}, "ContractID"},
		{"sac_balance/Holder", func(s *Store) error {
			return s.InsertSACBalanceObservation(ctx, SACBalanceObservation{ContractID: "CA1", AssetKey: asset, Balance: zero})
		}, "Holder"},
		{"sac_balance/Balance", func(s *Store) error {
			return s.InsertSACBalanceObservation(ctx, SACBalanceObservation{ContractID: "CA1", AssetKey: asset, Holder: "GA1"})
		}, "Balance"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(&Store{})
			if err == nil || !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("err=%v should mention %s", err, tc.wantSubstr)
			}
		})
	}
}

// ─── batch writer (the ops claimable seed's write path) ─────────────────

// TestInsertClaimableObservationBatch_Validates — the same three defensive
// guards the single-row writer applies, per row, BEFORE any SQL is built. A
// nil Balance reaching the statement builder would panic on .String().
func TestInsertClaimableObservationBatch_Validates(t *testing.T) {
	s := &Store{}
	good := ClaimableObservation{ClaimableID: "cb1", AssetKey: "AQUA:GA5", Balance: big.NewInt(1)}
	cases := []struct {
		name string
		row  ClaimableObservation
		want string
	}{
		{"empty claimable id", ClaimableObservation{AssetKey: "AQUA:GA5", Balance: big.NewInt(1)}, "ClaimableID"},
		{"empty asset key", ClaimableObservation{ClaimableID: "cb1", Balance: big.NewInt(1)}, "AssetKey"},
		{"nil balance", ClaimableObservation{ClaimableID: "cb1", AssetKey: "AQUA:GA5"}, "Balance"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := s.InsertClaimableObservationBatch(context.Background(), []ClaimableObservation{good, c.row})
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err=%v should mention %s", err, c.want)
			}
		})
	}
}

// TestInsertClaimableObservationBatch_EmptyIsNoop — an empty flush must not
// build a statement with no VALUES (a syntax error) or touch the nil db.
func TestInsertClaimableObservationBatch_EmptyIsNoop(t *testing.T) {
	s := &Store{}
	if err := s.InsertClaimableObservationBatch(context.Background(), nil); err != nil {
		t.Errorf("empty batch should be a no-op, got %v", err)
	}
}

// TestDedupeClaimableObservations — Postgres rejects a single ON CONFLICT DO
// UPDATE statement presenting the same conflict key twice ("cannot affect row
// a second time"), so intra-batch duplicates must collapse LAST-wins in
// first-seen order before the statement is built.
func TestDedupeClaimableObservations(t *testing.T) {
	at := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	rows := []ClaimableObservation{
		{ClaimableID: "cb1", AssetKey: "AQUA:GA5", Ledger: 10, ObservedAt: at, Balance: big.NewInt(1)},
		{ClaimableID: "cb2", AssetKey: "AQUA:GA5", Ledger: 10, ObservedAt: at, Balance: big.NewInt(2)},
		// Same conflict key as cb1, different instant representation.
		{ClaimableID: "cb1", AssetKey: "AQUA:GA5", Ledger: 10, ObservedAt: at.In(time.FixedZone("X", 3600)), Balance: big.NewInt(3)},
		// Same id, DIFFERENT ledger — a distinct row, not a duplicate.
		{ClaimableID: "cb1", AssetKey: "AQUA:GA5", Ledger: 11, ObservedAt: at, Balance: big.NewInt(4)},
	}
	got := dedupeClaimableObservations(rows)
	if len(got) != 3 {
		t.Fatalf("got %d rows, want 3: %+v", len(got), got)
	}
	if got[0].Balance.Cmp(big.NewInt(3)) != 0 {
		t.Errorf("row 0 balance = %s, want 3 (last write for the duplicated conflict key)", got[0].Balance)
	}
	if got[1].ClaimableID != "cb2" || got[2].Ledger != 11 {
		t.Errorf("first-seen order not preserved: %+v", got)
	}
}
