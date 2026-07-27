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
// (per the Test conventions in CLAUDE.md, integration tests run
// via testcontainers-go).

func TestInsertTrustlineObservation_RejectsEmptyAccountID(t *testing.T) {
	s := &Store{}
	err := s.InsertTrustlineObservation(context.Background(), TrustlineObservation{
		AssetKey: "USDC:GA5...",
		Balance:  big.NewInt(0),
	})
	if err == nil {
		t.Fatal("expected error on empty AccountID")
	}
	if !strings.Contains(err.Error(), "AccountID") {
		t.Errorf("err=%v should mention AccountID", err)
	}
}

func TestInsertTrustlineObservation_RejectsEmptyAssetKey(t *testing.T) {
	s := &Store{}
	err := s.InsertTrustlineObservation(context.Background(), TrustlineObservation{
		AccountID: "GA1",
		Balance:   big.NewInt(0),
	})
	if err == nil || !strings.Contains(err.Error(), "AssetKey") {
		t.Errorf("err=%v should mention AssetKey", err)
	}
}

func TestInsertTrustlineObservation_RejectsNilBalance(t *testing.T) {
	s := &Store{}
	err := s.InsertTrustlineObservation(context.Background(), TrustlineObservation{
		AccountID: "GA1",
		AssetKey:  "USDC:GA5...",
	})
	if err == nil || !strings.Contains(err.Error(), "Balance") {
		t.Errorf("err=%v should mention Balance", err)
	}
}

func TestInsertClaimableObservation_RejectsEmptyClaimableID(t *testing.T) {
	s := &Store{}
	err := s.InsertClaimableObservation(context.Background(), ClaimableObservation{
		AssetKey: "USDC:GA5...",
		Balance:  big.NewInt(0),
	})
	if err == nil || !strings.Contains(err.Error(), "ClaimableID") {
		t.Errorf("err=%v should mention ClaimableID", err)
	}
}

func TestInsertClaimableObservation_RejectsEmptyAssetKey(t *testing.T) {
	s := &Store{}
	err := s.InsertClaimableObservation(context.Background(), ClaimableObservation{
		ClaimableID: "abc",
		Balance:     big.NewInt(0),
	})
	if err == nil || !strings.Contains(err.Error(), "AssetKey") {
		t.Errorf("err=%v should mention AssetKey", err)
	}
}

func TestInsertClaimableObservation_RejectsNilBalance(t *testing.T) {
	s := &Store{}
	err := s.InsertClaimableObservation(context.Background(), ClaimableObservation{
		ClaimableID: "abc",
		AssetKey:    "USDC:GA5...",
	})
	if err == nil || !strings.Contains(err.Error(), "Balance") {
		t.Errorf("err=%v should mention Balance", err)
	}
}

func TestInsertLPReserveObservation_RejectsEmptyPoolID(t *testing.T) {
	s := &Store{}
	err := s.InsertLPReserveObservation(context.Background(), LPReserveObservation{
		AssetKey: "USDC:GA5...",
		Balance:  big.NewInt(0),
	})
	if err == nil || !strings.Contains(err.Error(), "PoolID") {
		t.Errorf("err=%v should mention PoolID", err)
	}
}

func TestInsertLPReserveObservation_RejectsEmptyAssetKey(t *testing.T) {
	s := &Store{}
	err := s.InsertLPReserveObservation(context.Background(), LPReserveObservation{
		PoolID:  "deadbeef",
		Balance: big.NewInt(0),
	})
	if err == nil || !strings.Contains(err.Error(), "AssetKey") {
		t.Errorf("err=%v should mention AssetKey", err)
	}
}

func TestInsertLPReserveObservation_RejectsNilBalance(t *testing.T) {
	s := &Store{}
	err := s.InsertLPReserveObservation(context.Background(), LPReserveObservation{
		PoolID:   "deadbeef",
		AssetKey: "USDC:GA5...",
	})
	if err == nil || !strings.Contains(err.Error(), "Balance") {
		t.Errorf("err=%v should mention Balance", err)
	}
}

func TestInsertSACBalanceObservation_RejectsEmptyContractID(t *testing.T) {
	s := &Store{}
	err := s.InsertSACBalanceObservation(context.Background(), SACBalanceObservation{
		AssetKey: "USDC:GA5...",
		Holder:   "GA1",
		Balance:  big.NewInt(0),
	})
	if err == nil || !strings.Contains(err.Error(), "ContractID") {
		t.Errorf("err=%v should mention ContractID", err)
	}
}

func TestInsertSACBalanceObservation_RejectsEmptyHolder(t *testing.T) {
	s := &Store{}
	err := s.InsertSACBalanceObservation(context.Background(), SACBalanceObservation{
		ContractID: "CA1",
		AssetKey:   "USDC:GA5...",
		Balance:    big.NewInt(0),
	})
	if err == nil || !strings.Contains(err.Error(), "Holder") {
		t.Errorf("err=%v should mention Holder", err)
	}
}

func TestInsertSACBalanceObservation_RejectsNilBalance(t *testing.T) {
	s := &Store{}
	err := s.InsertSACBalanceObservation(context.Background(), SACBalanceObservation{
		ContractID: "CA1",
		AssetKey:   "USDC:GA5...",
		Holder:     "GA1",
	})
	if err == nil || !strings.Contains(err.Error(), "Balance") {
		t.Errorf("err=%v should mention Balance", err)
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
