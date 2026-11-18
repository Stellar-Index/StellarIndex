//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	chstore "github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func tdInstanceVal(withDecimal bool, decimal uint32) xdr.ScVal {
	nameSym, decSym, metaSym := xdr.ScSymbol("name"), xdr.ScSymbol("decimal"), xdr.ScSymbol("METADATA")
	name := xdr.ScString("Fixture Token")
	inner := xdr.ScMap{{
		Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &nameSym},
		Val: xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &name},
	}}
	if withDecimal {
		d := xdr.Uint32(decimal)
		inner = append(inner, xdr.ScMapEntry{
			Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &decSym},
			Val: xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &d},
		})
	}
	ip := &inner
	storage := xdr.ScMap{{
		Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &metaSym},
		Val: xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &ip},
	}}
	inst := xdr.ScContractInstance{Executable: xdr.ContractExecutable{Type: xdr.ContractExecutableTypeContractExecutableStellarAsset}, Storage: &storage}
	return xdr.ScVal{Type: xdr.ScValTypeScvContractInstance, Instance: &inst}
}

// The batched scale read answers exactly what the single read answers, per contract, from one query: the newest instance wins,
// an instance with no declaration and a contract with no instance are both absent, and neither is a failure.
func TestTokenDecimalsBatch_MatchesTheSingleRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	addr := clickhouseAddr(t)

	declared, declaredAddr := fhSyntheticContractAddr(t, 0xD1)
	undeclared, undeclaredAddr := fhSyntheticContractAddr(t, 0xD2)
	missing, _ := fhSyntheticContractAddr(t, 0xD3)
	instanceKey := xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance}

	var rows []chstore.LedgerEntryChangeRow
	for i, w := range []struct {
		contract xdr.ScAddress
		ledger   uint32
		val      xdr.ScVal
	}{
		{declaredAddr, 70_000_100, tdInstanceVal(true, 6)},
		{declaredAddr, 70_000_200, tdInstanceVal(true, 9)}, // the upgrade: 9 is current
		{undeclaredAddr, 70_000_100, tdInstanceVal(false, 0)},
	} {
		rows = append(rows, chstore.LedgerEntryChangeRow{
			LedgerSeq:   w.ledger,
			CloseTime:   time.Date(2026, 1, 1, 0, 0, i, 0, time.UTC),
			TxHash:      fmt.Sprintf("%064x", 0xD100+i),
			OpIndex:     0,
			ChangeIndex: 1,
			ChangeType:  "updated",
			EntryType:   "contract_data",
			KeyXDR:      fhKeyXDR(t, w.contract, instanceKey),
			EntryXDR:    fhEntryXDR(t, w.contract, instanceKey, w.val, w.ledger),
		})
	}
	if _, err := chstore.InsertEntryChanges(ctx, addr, rows, 0); err != nil {
		t.Fatalf("InsertEntryChanges: %v", err)
	}

	er, err := chstore.NewExplorerReader(ctx, addr)
	if err != nil {
		t.Fatalf("NewExplorerReader: %v", err)
	}
	got, err := er.TokenDecimalsBatch(ctx, []string{declared, undeclared, missing})
	if err != nil {
		t.Fatalf("TokenDecimalsBatch: %v", err)
	}
	if d, ok := got[declared]; !ok || d != 9 {
		t.Errorf("declared contract: got (%d, %v), want the current 9", d, ok)
	}
	for _, id := range []string{undeclared, missing} {
		if d, ok := got[id]; ok {
			t.Errorf("%s: got scale %d, want absent", id, d)
		}
	}
	for _, id := range []string{declared, undeclared, missing} {
		d, found, err := er.TokenDecimals(ctx, id)
		if err != nil {
			t.Fatalf("TokenDecimals(%s): %v", id, err)
		}
		if bd, bok := got[id]; bok != found || bd != d {
			t.Errorf("%s: batch (%d, %v) != single (%d, %v)", id, bd, bok, d, found)
		}
	}
}
