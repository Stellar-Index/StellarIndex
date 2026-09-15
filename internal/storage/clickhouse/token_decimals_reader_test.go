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

// mkInstanceEntryKeys builds a base64 contract-instance LedgerEntry whose
// METADATA map declares the scale under each of the given keys. It is the
// general form of [mkInstanceEntry], which fixes the key at `decimal`.
func mkInstanceEntryKeys(t *testing.T, decls map[string]uint32) string {
	t.Helper()
	metaSym := xdr.ScSymbol("METADATA")
	inner := xdr.ScMap{}
	// Sorted so the encoded map is stable between runs; the decoder must
	// not depend on the order either way.
	for _, k := range []string{"decimal", "decimals"} {
		v, ok := decls[k]
		if !ok {
			continue
		}
		sym := xdr.ScSymbol(k)
		u := xdr.Uint32(v)
		inner = append(inner, xdr.ScMapEntry{
			Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym},
			Val: xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &u},
		})
	}
	innerPtr := &inner
	storage := xdr.ScMap{{
		Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &metaSym},
		Val: xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &innerPtr},
	}}
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

// TestDecimalsFromInstanceEntry_ReadsBothSpellings is the money bug this
// decoder shipped with, pinned.
//
// The soroban-token-sdk names the METADATA field `decimal`, and reading only
// that spelling was justified on the belief that virtually every SEP-41 token
// follows the SDK. Measured against the lake on 2026-09-15, over the 17
// Soroban contract addresses a public listing platform names on Stellar:
// SEVEN spell it `decimal` and TEN spell it `decimals`. The majority of that
// population was returning "no usable metadata", and every caller's documented
// response to that is to keep a hardcoded 7.
//
// On the RWA surface that default IS the published figure. A tokenized
// Treasury fund declaring 5 decimals, read at 7, publishes one HUNDREDTH of
// its capitalisation: 283,278,671 tokens at $1.22 is $345.6M, and the same
// row at the default is $3.46M. The measured supply is the real one.
func TestDecimalsFromInstanceEntry_ReadsBothSpellings(t *testing.T) {
	for _, tc := range []struct {
		name  string
		decls map[string]uint32
		want  uint32
		ok    bool
	}{
		// The soroban-token-sdk spelling — SACs and SDK builds.
		{"sdk spelling", map[string]uint32{"decimal": 7}, 7, true},
		// The hand-written spelling. Ten of the seventeen measured
		// contracts, including both admitted T-Bill funds at 5.
		{"plural spelling", map[string]uint32{"decimals": 5}, 5, true},
		// An 18-decimal token really is in that population. Read at the
		// caller's default of 7 it would publish eleven orders of
		// magnitude too much.
		{"eighteen decimals", map[string]uint32{"decimals": 18}, 18, true},
		{"zero is a scale", map[string]uint32{"decimals": 0}, 0, true},
		// Both, agreeing: one declaration, said twice.
		{"both agreeing", map[string]uint32{"decimal": 6, "decimals": 6}, 6, true},
		// Both, DISAGREEING: the contract has not told us its scale.
		// Refused rather than resolved by preference — picking one of
		// two contradictory self-declarations would be this layer
		// inventing an exponent for a money figure.
		{"both disagreeing", map[string]uint32{"decimal": 7, "decimals": 5}, 0, false},
		// Present but out of bounds under either spelling: the contract
		// answered and the answer was not a scale, so the other key is
		// not consulted as a fallback.
		{"plural out of bounds", map[string]uint32{"decimals": maxSaneTokenDecimals + 1}, 0, false},
		{"sdk out of bounds", map[string]uint32{"decimal": maxSaneTokenDecimals + 1}, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := decimalsFromInstanceEntry(mkInstanceEntryKeys(t, tc.decls))
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v (got %d)", ok, tc.ok, got)
			}
			if ok && got != tc.want {
				t.Errorf("decimals = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestDecimalsFromInstanceEntry_NoScaleIsNotZero keeps the two distinct
// answers distinct. A METADATA map with no scale under either spelling is
// UNKNOWN, and a caller told 0 would divide by 10^0 and publish the raw
// smallest-unit count as a token amount.
func TestDecimalsFromInstanceEntry_NoScaleIsNotZero(t *testing.T) {
	if got, ok := decimalsFromInstanceEntry(mkInstanceEntryKeys(t, map[string]uint32{})); ok {
		t.Errorf("a METADATA map declaring no scale answered %d", got)
	}
}
