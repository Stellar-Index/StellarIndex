package timescale

import (
	"context"
	"strings"
	"testing"
)

// Defensive-guard coverage. The full INSERT/SELECT round-trip lives in
// test/integration/ per the established testcontainers-go pattern.

func TestUpsertProtocolContract_rejectsEmptySource(t *testing.T) {
	s := &Store{}
	err := s.UpsertProtocolContract(context.Background(), "", "Cchild", "Cfactory", 1)
	if err == nil || !strings.Contains(err.Error(), "source or contract_id") {
		t.Errorf("err=%v should mention empty source or contract_id", err)
	}
}

func TestUpsertProtocolContract_rejectsEmptyContract(t *testing.T) {
	s := &Store{}
	err := s.UpsertProtocolContract(context.Background(), "blend", "", "Cfactory", 1)
	if err == nil || !strings.Contains(err.Error(), "source or contract_id") {
		t.Errorf("err=%v should mention empty source or contract_id", err)
	}
}

func TestUpsertProtocolContract_rejectsEmptyFactory(t *testing.T) {
	s := &Store{}
	err := s.UpsertProtocolContract(context.Background(), "blend", "Cchild", "", 1)
	if err == nil || !strings.Contains(err.Error(), "factory_id") {
		t.Errorf("err=%v should mention empty factory_id", err)
	}
}

func TestLoadProtocolContracts_rejectsEmptySource(t *testing.T) {
	s := &Store{}
	_, err := s.LoadProtocolContracts(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "empty source") {
		t.Errorf("err=%v should mention empty source", err)
	}
}

func TestListProtocolContracts_rejectsEmptySource(t *testing.T) {
	s := &Store{}
	_, err := s.ListProtocolContracts(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "empty source") {
		t.Errorf("err=%v should mention empty source", err)
	}
}

// TestSourceContractCountQueryShape guards the count-without-enumeration
// path: sorocredit's child-contract total must come from a DISTINCT over
// credit_positions.collateral_contract (the NewCollateralContract announcement
// of each Collateral-<uuid> child, migration 0090) and must be unwindowed —
// it is the lifetime contract set, not a window's activity. Every other
// source has no such query, so its roster stays its count.
func TestSourceContractCountQueryShape(t *testing.T) {
	q, ok := sourceContractCountQuery("sorocredit")
	if !ok {
		t.Fatal("sorocredit must have a count-only path: its ~116k child contracts are 23x the roster cap")
	}
	if !strings.Contains(q, "count(DISTINCT collateral_contract)") {
		t.Errorf("sorocredit count must be DISTINCT over the child contract column, got %q", q)
	}
	if !strings.Contains(q, "credit_positions") {
		t.Errorf("sorocredit count must read credit_positions, got %q", q)
	}
	if strings.Contains(q, "LIMIT") || strings.Contains(q, "interval") {
		t.Errorf("sorocredit count must be the unwindowed, uncapped total, got %q", q)
	}

	for _, source := range []string{"blend", "soroswap", "defindex", "aquarius", "sdex", ""} {
		if q, ok := sourceContractCountQuery(source); ok {
			t.Errorf("source %q must have no count-only path (its roster is its count), got %q", source, q)
		}
	}
}

// TestCountSourceContracts_uncountedSourceIssuesNoQuery pins that a source
// without a count-only path never reaches the database — the nil-db Store
// below would panic if it did.
func TestCountSourceContracts_uncountedSourceIssuesNoQuery(t *testing.T) {
	s := &Store{}
	n, ok, err := s.CountSourceContracts(context.Background(), "blend")
	if n != 0 || ok || err != nil {
		t.Errorf("CountSourceContracts(blend) = (%d, %v, %v), want (0, false, nil)", n, ok, err)
	}
}
