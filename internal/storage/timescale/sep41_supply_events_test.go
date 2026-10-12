package timescale

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Tests cover the InsertSEP41SupplyEvent defensive guards. The
// SEP41NetMintAtOrBefore SQL needs a real DB and lives in
// test/integration/ per the established convention.

// TestInsertSEP41SupplyEvent_RejectsInvalidRows: amounts are non-negative (event_kind carries direction),
// so a negative one is upstream confusion; "transfer" is a valid SEP-41 event but not supply-affecting.
func TestInsertSEP41SupplyEvent_RejectsInvalidRows(t *testing.T) {
	hash := strings.Repeat("a", 64)
	cases := []struct {
		name       string
		in         SEP41SupplyEvent
		wantSubstr string
	}{
		{"empty contract", SEP41SupplyEvent{TxHash: hash, Kind: SEP41EventMint, Amount: big.NewInt(1)}, "ContractID"},
		{"empty tx hash", SEP41SupplyEvent{ContractID: "C1", Kind: SEP41EventMint, Amount: big.NewInt(1)}, "TxHash"},
		{"transfer kind", SEP41SupplyEvent{ContractID: "C1", TxHash: hash, Kind: SEP41EventKind("transfer"), Amount: big.NewInt(1)}, "Kind"},
		{"nil amount", SEP41SupplyEvent{ContractID: "C1", TxHash: hash, Kind: SEP41EventMint}, "Amount"},
		{"negative amount", SEP41SupplyEvent{ContractID: "C1", TxHash: hash, Kind: SEP41EventMint, Amount: big.NewInt(-1)}, "negative"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := (&Store{}).InsertSEP41SupplyEvent(context.Background(), tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("err=%v should mention %s", err, tc.wantSubstr)
			}
		})
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

// TestSEP41SupplyEventWriters_ShareRowContract: both multi-row writers must refuse an invalid row before
// touching the database, since one negative row fails CHECK (amount >= 0) and aborts the whole statement.
// The zero Store has no *sql.DB, so a row that slips through panics.
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

func TestCursorIndex16_ClampsInsteadOfWrapping(t *testing.T) {
	for in, want := range map[uint32]int16{0: 0, 7: 7, math.MaxInt16: math.MaxInt16, math.MaxInt16 + 1: math.MaxInt16, math.MaxUint16: math.MaxInt16, math.MaxUint32: math.MaxInt16} {
		if got := cursorIndex16(in); got != want {
			t.Errorf("cursorIndex16(%d) = %d, want %d", in, got, want)
		}
	}
}

// TestSEP41SupplyRollupMigrationsNeverPrescribeDestructiveReset pins that migrations never tell operators to
// `TRUNCATE sep41_supply_rollup`, which also wipes migration 0088's seeded genesis baseline and under-reports
// supply.
func TestSEP41SupplyRollupMigrationsNeverPrescribeDestructiveReset(t *testing.T) {
	paths, err := filepath.Glob("../../../migrations/*.sql")
	if err != nil || len(paths) == 0 {
		t.Fatalf("glob migrations: %d files, err=%v", len(paths), err)
	}
	destructive := regexp.MustCompile("(?i)(TRUNCATE\\s+(TABLE\\s+)?|DELETE\\s+FROM\\s+)`?sep41_supply_rollup")
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if loc := destructive.FindIndex(raw); loc != nil {
			t.Errorf("%s: %q — never TRUNCATE/DELETE sep41_supply_rollup; it drops the migration-0088 "+
				"genesis baseline. Reset the fold via Store.ResetSEP41SupplyRollupFold instead",
				filepath.Base(p), raw[loc[0]:loc[1]])
		}
	}

	header, err := os.ReadFile("../../../migrations/0085_create_sep41_supply_rollup.up.sql")
	if err != nil {
		t.Fatalf("read migration 0085: %v", err)
	}
	if !strings.Contains(string(header), "ResetSEP41SupplyRollupFold") {
		t.Error("migration 0085's header no longer names ResetSEP41SupplyRollupFold as the below-checkpoint reset")
	}
}
