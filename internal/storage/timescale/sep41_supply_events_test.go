package timescale

import (
	"context"
	"math/big"
	"strings"
	"testing"
)

// Tests cover the InsertSEP41SupplyEvent defensive guards. The
// SEP41NetMintAtOrBefore SQL needs a real DB and lives in
// test/integration/ per the established convention.

func TestInsertSEP41SupplyEvent_RejectsEmptyContractID(t *testing.T) {
	s := &Store{}
	err := s.InsertSEP41SupplyEvent(context.Background(), SEP41SupplyEvent{
		TxHash: strings.Repeat("a", 64),
		Kind:   SEP41EventMint,
		Amount: big.NewInt(1),
	})
	if err == nil || !strings.Contains(err.Error(), "ContractID") {
		t.Errorf("err=%v should mention ContractID", err)
	}
}

func TestInsertSEP41SupplyEvent_RejectsEmptyTxHash(t *testing.T) {
	s := &Store{}
	err := s.InsertSEP41SupplyEvent(context.Background(), SEP41SupplyEvent{
		ContractID: "C1",
		Kind:       SEP41EventMint,
		Amount:     big.NewInt(1),
	})
	if err == nil || !strings.Contains(err.Error(), "TxHash") {
		t.Errorf("err=%v should mention TxHash", err)
	}
}

func TestInsertSEP41SupplyEvent_RejectsInvalidKind(t *testing.T) {
	s := &Store{}
	err := s.InsertSEP41SupplyEvent(context.Background(), SEP41SupplyEvent{
		ContractID: "C1",
		TxHash:     strings.Repeat("a", 64),
		Kind:       SEP41EventKind("transfer"), // valid SEP-41 event but NOT supply-affecting
		Amount:     big.NewInt(1),
	})
	if err == nil {
		t.Fatal("expected error on transfer kind (not supply-affecting)")
	}
	if !strings.Contains(err.Error(), "Kind") {
		t.Errorf("err=%v should mention Kind", err)
	}
}

func TestInsertSEP41SupplyEvent_RejectsNilAmount(t *testing.T) {
	s := &Store{}
	err := s.InsertSEP41SupplyEvent(context.Background(), SEP41SupplyEvent{
		ContractID: "C1",
		TxHash:     strings.Repeat("a", 64),
		Kind:       SEP41EventMint,
	})
	if err == nil || !strings.Contains(err.Error(), "Amount") {
		t.Errorf("err=%v should mention Amount", err)
	}
}

// TestInsertSEP41SupplyEvent_RejectsNegativeAmount — by
// convention amounts are non-negative; event_kind discriminates
// direction. A negative amount is upstream confusion the
// observer hasn't caught.
func TestInsertSEP41SupplyEvent_RejectsNegativeAmount(t *testing.T) {
	s := &Store{}
	err := s.InsertSEP41SupplyEvent(context.Background(), SEP41SupplyEvent{
		ContractID: "C1",
		TxHash:     strings.Repeat("a", 64),
		Kind:       SEP41EventMint,
		Amount:     big.NewInt(-1),
	})
	if err == nil {
		t.Fatal("expected error on negative amount")
	}
	if !strings.Contains(err.Error(), "negative") {
		t.Errorf("err=%v should mention negative", err)
	}
}

func TestSEP41EventKind_IsValid(t *testing.T) {
	cases := map[SEP41EventKind]bool{
		SEP41EventMint:             true,
		SEP41EventBurn:             true,
		SEP41EventClawback:         true,
		SEP41EventKind("transfer"): false,
		SEP41EventKind(""):         false,
		SEP41EventKind("nonsense"): false,
	}
	for k, want := range cases {
		if got := k.IsValid(); got != want {
			t.Errorf("%q.IsValid() = %v, want %v", k, got, want)
		}
	}
}
