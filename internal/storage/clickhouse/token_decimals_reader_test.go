package clickhouse

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// mkInstanceEntry builds a base64 contract-instance LedgerEntry whose
// storage carries a token-sdk METADATA map with the given decimal value
// (omitted entirely when withMetadata is false).
func mkInstanceEntry(t *testing.T, withMetadata bool, decimal uint32) string {
	t.Helper()
	var storage xdr.ScMap
	if withMetadata {
		metaSym := xdr.ScSymbol("METADATA")
		decSym := xdr.ScSymbol("decimal")
		nameSym := xdr.ScSymbol("name")
		name := xdr.ScString("Test Token")
		dec := xdr.Uint32(decimal)
		inner := xdr.ScMap{
			{
				Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &decSym},
				Val: xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &dec},
			},
			{
				Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &nameSym},
				Val: xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &name},
			},
		}
		innerPtr := &inner
		storage = xdr.ScMap{{
			Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &metaSym},
			Val: xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &innerPtr},
		}}
	}
	inst := xdr.ScContractInstance{
		Executable: xdr.ContractExecutable{Type: xdr.ContractExecutableTypeContractExecutableWasm, WasmHash: &xdr.Hash{1}},
		Storage:    &storage,
	}
	var cid xdr.ContractId
	cid[0] = 0xAB
	entry := xdr.LedgerEntry{
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.ContractDataEntry{
				Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid},
				Key:        xdr.ScVal{Type: xdr.ScValTypeScvLedgerKeyContractInstance},
				Durability: xdr.ContractDataDurabilityPersistent,
				Val:        xdr.ScVal{Type: xdr.ScValTypeScvContractInstance, Instance: &inst},
			},
		},
	}
	b64, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal instance entry: %v", err)
	}
	return b64
}

func TestDecimalsFromInstanceEntry(t *testing.T) {
	cases := []struct {
		name   string
		b64    string
		want   uint32
		wantOK bool
	}{
		{"token-sdk metadata decimal=18", mkInstanceEntry(t, true, 18), 18, true},
		{"sac-style decimal=7", mkInstanceEntry(t, true, 7), 7, true},
		{"zero decimals is a valid declaration", mkInstanceEntry(t, true, 0), 0, true},
		{"no METADATA map → not derivable", mkInstanceEntry(t, false, 0), 0, false},
		{"insane declaration rejected", mkInstanceEntry(t, true, maxSaneTokenDecimals+1), 0, false},
		{"garbage b64", "not-xdr", 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := decimalsFromInstanceEntry(tc.b64)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("decimalsFromInstanceEntry = (%d, %v), want (%d, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
