package timescale

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"testing"
	"time"
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

// TestSEP41SupplyEventWriters_ShareRowContract runs the same invalid second
// row through both multi-row sep41_supply_events writers: each must refuse
// it before touching the database, as InsertSEP41SupplyEvent does. The zero
// Store has no *sql.DB, so a row that slips past validation panics in the
// writer and is reported as reaching the DB. One negative row would
// otherwise fail CHECK (amount >= 0) and abort the whole statement.
func TestSEP41SupplyEventWriters_ShareRowContract(t *testing.T) {
	good := SEP41SupplyEvent{
		ContractID: "CDLZFC3SYJYDZT7K67VZ75HPJVIEUVNIXF47ZG2FB2RMQQVU2HHGCYSC",
		Ledger:     60_000_000,
		TxHash:     strings.Repeat("a", 64),
		ObservedAt: time.Unix(1_700_000_000, 0),
		Kind:       SEP41EventMint,
		Amount:     big.NewInt(5),
	}
	cases := []struct {
		name    string
		mutate  func(*SEP41SupplyEvent)
		wantErr string
	}{
		{"negative amount", func(e *SEP41SupplyEvent) {
			e.Amount = new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 100))
		}, "row 1 negative Amount -1267650600228229401496703205376"},
		{"nil amount", func(e *SEP41SupplyEvent) { e.Amount = nil }, "row 1 nil Amount"},
		{"empty contract", func(e *SEP41SupplyEvent) { e.ContractID = "" }, "row 1 empty ContractID"},
		{"empty tx hash", func(e *SEP41SupplyEvent) { e.TxHash = "" }, "row 1 empty TxHash"},
		{"invalid kind", func(e *SEP41SupplyEvent) { e.Kind = "transfer" }, `row 1 invalid Kind "transfer"`},
	}
	writers := map[string]func(*Store, []SEP41SupplyEvent) error{
		"InsertSEP41SupplyEventBatch": func(s *Store, rows []SEP41SupplyEvent) error {
			return s.InsertSEP41SupplyEventBatch(context.Background(), rows)
		},
		"CopyMergeSEP41SupplyEvents": func(s *Store, rows []SEP41SupplyEvent) error {
			return s.CopyMergeSEP41SupplyEvents(context.Background(), rows)
		},
	}
	for _, tc := range cases {
		bad := good
		bad.OpIndex = 1
		tc.mutate(&bad)
		for op, write := range writers {
			var err error
			func() {
				defer func() {
					if p := recover(); p != nil {
						err = fmt.Errorf("reached the database with an invalid row: %v", p)
					}
				}()
				err = write(&Store{}, []SEP41SupplyEvent{good, bad})
			}()
			if want := "timescale: " + op + ": " + tc.wantErr; err == nil || err.Error() != want {
				t.Errorf("%s/%s: err = %v, want %q", tc.name, op, err, want)
			}
		}
	}
}
