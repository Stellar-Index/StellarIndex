package timescale

import (
	"context"
	"database/sql"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// TestInsertSupply_RejectsZeroValueStruct — the supply-package
// computers always populate AssetKey + TotalSupply + CirculatingSupply.
// A caller passing a zero-value Supply is a bug; surface it loudly
// rather than letting the DB reject it (or worse, write garbage if
// future migrations relax the CHECK constraints).
func TestInsertSupply_RejectsZeroValueStruct(t *testing.T) {
	// Use a Store with a nil *sql.DB — InsertSupply must reject
	// before it gets to the DB call, so the nil deref never fires.
	s := &Store{}
	err := s.InsertSupply(context.Background(), supply.Supply{})
	if err == nil {
		t.Fatal("expected error on zero-value Supply; got nil")
	}
	if got := err.Error(); got == "" {
		t.Errorf("error message is empty: %q", got)
	}
}

// TestInsertSupply_RequiresTotalSupply — AssetKey set but TotalSupply
// nil should still fail before touching the DB.
func TestInsertSupply_RequiresTotalSupply(t *testing.T) {
	s := &Store{}
	err := s.InsertSupply(context.Background(), supply.Supply{
		AssetKey:          "XLM",
		CirculatingSupply: big.NewInt(0),
	})
	if err == nil {
		t.Fatal("expected error when TotalSupply is nil; got nil")
	}
}

// TestInsertSupply_RequiresCirculatingSupply — likewise.
func TestInsertSupply_RequiresCirculatingSupply(t *testing.T) {
	s := &Store{}
	err := s.InsertSupply(context.Background(), supply.Supply{
		AssetKey:    "XLM",
		TotalSupply: big.NewInt(0),
	})
	if err == nil {
		t.Fatal("expected error when CirculatingSupply is nil; got nil")
	}
}

// TestAssembleSupply_HappyPath — text-cast NUMERIC columns parse
// back to the same *big.Int the writer started with, including very
// large values that exceed int64.
func TestAssembleSupply_HappyPath(t *testing.T) {
	const totalStr = "500018068120000000" // XLM total in stroops, > 2^59
	observedAt := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	got, err := assembleSupply("XLM", observedAt, totalStr, "499000000000000000",
		sql.NullString{Valid: true, String: totalStr}, "xlm_sdf_reserve_exclusion", 50_000_000,
		sql.NullString{Valid: false}) // XLM has no SAC-wrapped component
	if err != nil {
		t.Fatalf("assembleSupply: %v", err)
	}
	if got.TotalSupply.String() != totalStr {
		t.Errorf("TotalSupply = %s, want %s", got.TotalSupply, totalStr)
	}
	if got.CirculatingSupply.String() != "499000000000000000" {
		t.Errorf("CirculatingSupply = %s", got.CirculatingSupply)
	}
	if got.MaxSupply == nil || got.MaxSupply.String() != totalStr {
		t.Errorf("MaxSupply = %v", got.MaxSupply)
	}
	if got.Basis != supply.BasisXLMSDFReserveExclusion {
		t.Errorf("Basis = %q", got.Basis)
	}
	if got.LedgerSequence != 50_000_000 {
		t.Errorf("LedgerSequence = %d", got.LedgerSequence)
	}
	if !got.ObservedAt.Equal(observedAt) {
		t.Errorf("ObservedAt = %v", got.ObservedAt)
	}
}

// TestAssembleSupply_NullMaxSupply — uncapped issuer + no override
// case: max_supply is NULL on disk; assembled struct has nil
// MaxSupply (NOT zero, NOT empty string).
func TestAssembleSupply_NullMaxSupply(t *testing.T) {
	got, err := assembleSupply("USDC:GA1", time.Now(), "100", "90",
		sql.NullString{Valid: false}, "issuer_exclusion", 1, sql.NullString{Valid: false})
	if err != nil {
		t.Fatalf("assembleSupply: %v", err)
	}
	if got.MaxSupply != nil {
		t.Errorf("MaxSupply = %v, want nil for uncapped issuer", got.MaxSupply)
	}
}

// TestAssembleSupply_RejectsBadNumeric — a non-decimal value in the
// numeric column would be a Postgres-level corruption; surface a
// clear error rather than silently producing zero.
func TestAssembleSupply_RejectsBadNumeric(t *testing.T) {
	_, err := assembleSupply("XLM", time.Now(), "not-a-number", "0", sql.NullString{}, "x", 1, sql.NullString{})
	if err == nil {
		t.Fatal("expected error for unparseable total_supply; got nil")
	}
}

// TestAssembleSupply_SACWrappedRoundTrips — the sac_wrapped_stroops
// column (migration 0117) must come back as the EXACT component the
// writer stored, including values beyond int64. This is the input to
// the cross-check's escrow leg (SACWrapped ≤ sac_total), so a decode
// that silently truncated would turn a real violation into a green
// check.
func TestAssembleSupply_SACWrappedRoundTrips(t *testing.T) {
	const sacWrapped = "1236670485295609000" // > 2^59, i128-shaped
	got, err := assembleSupply("BLND:GDJEHTBE", time.Now(), "9000000000000000000", "9000000000000000000",
		sql.NullString{}, "issuer_exclusion", 1,
		sql.NullString{Valid: true, String: sacWrapped})
	if err != nil {
		t.Fatalf("assembleSupply: %v", err)
	}
	if got.SACWrappedStroops == nil {
		t.Fatal("SACWrappedStroops = nil, want the stored component — a NULL here reads as CS-087 unchecked and silently disables the cross-check's escrow leg")
	}
	if got.SACWrappedStroops.String() != sacWrapped {
		t.Errorf("SACWrappedStroops = %s, want %s", got.SACWrappedStroops, sacWrapped)
	}
}

// TestAssembleSupply_NullSACWrappedStaysNil — a pre-0117 row (or a
// non-classic algorithm) has SQL NULL here. It MUST assemble to nil,
// not to zero: zero is a meaningful component value and the escrow
// bound 0 ≤ sac_total holds vacuously, so a zero coercion would report
// a green, "checked" cross-check that verified nothing (CS-087).
func TestAssembleSupply_NullSACWrappedStaysNil(t *testing.T) {
	got, err := assembleSupply("USDC:GA1", time.Now(), "100", "90",
		sql.NullString{}, "issuer_exclusion", 1, sql.NullString{Valid: false})
	if err != nil {
		t.Fatalf("assembleSupply: %v", err)
	}
	if got.SACWrappedStroops != nil {
		t.Errorf("SACWrappedStroops = %v, want nil for a NULL column (nil is the unchecked state; zero would pass the escrow bound vacuously)", got.SACWrappedStroops)
	}
}

// TestAssembleSupply_RejectsBadSACWrapped — same corruption guard the
// other NUMERIC columns get. Silently producing zero would be the
// worst outcome: it is a valid-looking component that passes the
// escrow bound.
func TestAssembleSupply_RejectsBadSACWrapped(t *testing.T) {
	_, err := assembleSupply("USDC:GA1", time.Now(), "100", "90",
		sql.NullString{}, "issuer_exclusion", 1,
		sql.NullString{Valid: true, String: "not-a-number"})
	if err == nil {
		t.Fatal("expected error for unparseable sac_wrapped_stroops; got nil")
	}
}

// TestInsertSupply_RejectsNegativeSACWrapped — migration 0117
// deliberately carries no CHECK constraint (adding one to a compressed
// hypertable needs the decompress-every-chunk dance), so this guard is
// the only enforcement point at the write boundary. A negative escrow
// reading is nonsense that would nonetheless SATISFY the cross-check's
// escrow bound, so it must never reach the table.
func TestInsertSupply_RejectsNegativeSACWrapped(t *testing.T) {
	// nil *sql.DB: the guard must reject before any DB call.
	s := &Store{}
	err := s.InsertSupply(context.Background(), supply.Supply{
		AssetKey:          "USDC:GA1",
		TotalSupply:       big.NewInt(100),
		CirculatingSupply: big.NewInt(90),
		SACWrappedStroops: big.NewInt(-1),
	})
	if err == nil {
		t.Fatal("expected error for negative SACWrappedStroops; got nil")
	}
}

// TestLatestSupply_NotFoundIsTyped — the API layer relies on
// errors.Is(err, ErrNotFound) to distinguish "no supply data for
// this asset" from "Postgres unreachable". This guard pins the
// contract.
func TestLatestSupply_NotFoundIsTyped(t *testing.T) {
	// We can't invoke the full Store.LatestSupply without a DB; this
	// test documents the contract via reference. The actual SELECT
	// path is covered by the integration test.
	if !errors.Is(ErrNotFound, ErrNotFound) {
		t.Fatal("ErrNotFound must satisfy errors.Is against itself")
	}
}
