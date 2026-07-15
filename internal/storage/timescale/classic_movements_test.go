package timescale

import (
	"context"
	"os"
	"regexp"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// TestClassicMovementKind_ParityWithMigrationCheck pins the Go-side
// IsValid() set to migration 0103's movement_kind CHECK constraint —
// same regression class TestAquariusRewardsKind_ParityWithMigrationCheck
// guards for aquarius_rewards_events (v0.12.0 shipped a kind present
// in a migration CHECK but missing from Go's IsValid(), silently
// rejecting every insert of that kind client-side).
func TestClassicMovementKind_ParityWithMigrationCheck(t *testing.T) {
	raw, err := os.ReadFile("../../../migrations/0105_create_classic_movements.up.sql")
	if err != nil {
		t.Fatalf("read migration 0103: %v", err)
	}

	block := regexp.MustCompile(`(?s)movement_kind\s+text\s+NOT NULL CHECK \(movement_kind IN \((.*?)\)\)`).FindSubmatch(raw)
	if block == nil {
		t.Fatal("could not locate the movement_kind CHECK constraint in migration 0103")
	}
	kinds := regexp.MustCompile(`'([a-z_]+)'`).FindAllSubmatch(block[1], -1)
	if len(kinds) == 0 {
		t.Fatal("no kinds parsed out of the CHECK constraint")
	}

	for _, m := range kinds {
		k := ClassicMovementKind(m[1])
		if !k.IsValid() {
			t.Errorf("kind %q is in migration 0103's CHECK but IsValid() rejects it", k)
		}
	}

	goKinds := []ClassicMovementKind{
		ClassicMovementPayment, ClassicMovementCreateAccount, ClassicMovementPathPayment,
		ClassicMovementAccountMerge, ClassicMovementClawback,
		ClassicMovementClaimableBalanceCreate, ClassicMovementClaimableBalanceClaim,
		ClassicMovementClaimableBalanceClawback,
		ClassicMovementLiquidityPoolDeposit, ClassicMovementLiquidityPoolWithdraw,
	}
	if len(goKinds) != len(kinds) {
		t.Errorf("kind-count mismatch: %d Go constants vs %d in migration 0103's CHECK", len(goKinds), len(kinds))
	}
	inCheck := map[string]bool{}
	for _, m := range kinds {
		inCheck[string(m[1])] = true
	}
	for _, k := range goKinds {
		if !inCheck[string(k)] {
			t.Errorf("Go constant %q is not in migration 0103's CHECK", k)
		}
	}
}

// TestClassicMovementProvenance_ParityWithMigrationCheck is
// TestClassicMovementKind_ParityWithMigrationCheck's sibling for the
// provenance CHECK.
func TestClassicMovementProvenance_ParityWithMigrationCheck(t *testing.T) {
	raw, err := os.ReadFile("../../../migrations/0105_create_classic_movements.up.sql")
	if err != nil {
		t.Fatalf("read migration 0103: %v", err)
	}
	block := regexp.MustCompile(`(?s)provenance\s+text\s+NOT NULL DEFAULT 'classic_derived'\s*\n?\s*CHECK \(provenance IN \((.*?)\)\)`).FindSubmatch(raw)
	if block == nil {
		t.Fatal("could not locate the provenance CHECK constraint in migration 0103")
	}
	values := regexp.MustCompile(`'([a-z_0-9]+)'`).FindAllSubmatch(block[1], -1)
	if len(values) == 0 {
		t.Fatal("no provenance values parsed out of the CHECK constraint")
	}
	for _, m := range values {
		p := ClassicMovementProvenance(m[1])
		if !p.IsValid() {
			t.Errorf("provenance %q is in migration 0103's CHECK but IsValid() rejects it", p)
		}
	}
	goValues := []ClassicMovementProvenance{ClassicMovementClassicDerived, ClassicMovementCAP67Event}
	if len(goValues) != len(values) {
		t.Errorf("provenance-count mismatch: %d Go constants vs %d in migration 0103's CHECK", len(goValues), len(values))
	}
}

func TestValidateClassicMovementRow(t *testing.T) {
	valid := ClassicMovementRow{
		Kind:        ClassicMovementPayment,
		Ledger:      1,
		TxHash:      "tx",
		Asset:       "native",
		Amount:      canonical.NewAmount(nil),
		FromAddress: "GA",
		ToAddress:   "GB",
	}
	if err := validateClassicMovementRow(valid); err != nil {
		t.Errorf("expected valid row to pass, got %v", err)
	}

	bad := valid
	bad.Kind = ClassicMovementKind("bogus")
	if err := validateClassicMovementRow(bad); err == nil {
		t.Error("expected invalid Kind to be rejected")
	}

	bad = valid
	bad.TxHash = ""
	if err := validateClassicMovementRow(bad); err == nil {
		t.Error("expected empty TxHash to be rejected")
	}

	bad = valid
	bad.Asset = ""
	if err := validateClassicMovementRow(bad); err == nil {
		t.Error("expected empty Asset to be rejected")
	}
}

// TestBatchInsertClassicMovements_emptyIsNoop guards against a nil-
// Store panic on the zero-rows fast path (no DB round-trip needed).
func TestBatchInsertClassicMovements_emptyIsNoop(t *testing.T) {
	var s *Store
	landed, err := s.BatchInsertClassicMovements(context.Background(), nil)
	if err != nil {
		t.Errorf("expected no error on empty batch, got %v", err)
	}
	if landed != 0 {
		t.Errorf("landed = %d, want 0", landed)
	}
}
