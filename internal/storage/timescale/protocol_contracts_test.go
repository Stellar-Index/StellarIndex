package timescale

import (
	"context"
	"strings"
	"testing"
)

// Defensive-guard coverage. The full INSERT/SELECT round-trip lives in
// test/integration/ per the established testcontainers-go pattern.

func TestProtocolContracts_rejectInvalidArgs(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name       string
		call       func(*Store) error
		wantSubstr string
	}{
		{"upsert empty source", func(s *Store) error {
			return s.UpsertProtocolContract(ctx, "", "Cchild", "Cfactory", 1)
		}, "source or contract_id"},
		{"upsert empty contract", func(s *Store) error {
			return s.UpsertProtocolContract(ctx, "blend", "", "Cfactory", 1)
		}, "source or contract_id"},
		{"upsert empty factory", func(s *Store) error {
			return s.UpsertProtocolContract(ctx, "blend", "Cchild", "", 1)
		}, "factory_id"},
		{"load empty source", func(s *Store) error {
			_, err := s.LoadProtocolContracts(ctx, "")
			return err
		}, "empty source"},
		{"list empty source", func(s *Store) error {
			_, err := s.ListProtocolContracts(ctx, "")
			return err
		}, "empty source"},
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
	if strings.Contains(q, "LIMIT") || sqlContainsFold(q, "interval") {
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
