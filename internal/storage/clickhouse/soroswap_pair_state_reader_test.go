package clickhouse

import (
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"
)

// mkSoroswapPairEntry builds a base64 instance LedgerEntry matching the
// empirically-verified Soroswap pair layout: u32-keyed Token0/Token1
// addresses + Reserve0/Reserve1 i128s. Keys listed in `omit` are left
// out of the storage map.
func mkSoroswapPairEntry(t *testing.T, tok0, tok1 xdr.ContractId, r0, r1 xdr.Int128Parts, omit ...uint32) string {
	t.Helper()
	omitted := map[uint32]bool{}
	for _, o := range omit {
		omitted[o] = true
	}
	u32Val := func(v uint32) xdr.ScVal {
		u := xdr.Uint32(v)
		return xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &u}
	}
	addrVal := func(cid xdr.ContractId) xdr.ScVal {
		c := cid
		return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &xdr.ScAddress{
			Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &c,
		}}
	}
	i128Val := func(p xdr.Int128Parts) xdr.ScVal {
		pp := p
		return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &pp}
	}
	var storage xdr.ScMap
	add := func(key uint32, val xdr.ScVal) {
		if omitted[key] {
			return
		}
		storage = append(storage, xdr.ScMapEntry{Key: u32Val(key), Val: val})
	}
	add(soroswapKeyToken0, addrVal(tok0))
	add(soroswapKeyToken1, addrVal(tok1))
	add(soroswapKeyReserve0, i128Val(r0))
	add(soroswapKeyReserve1, i128Val(r1))

	inst := xdr.ScContractInstance{
		Executable: xdr.ContractExecutable{Type: xdr.ContractExecutableTypeContractExecutableWasm, WasmHash: &xdr.Hash{2}},
		Storage:    &storage,
	}
	var cid xdr.ContractId
	cid[0] = 0xCD
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
		t.Fatalf("marshal pair instance entry: %v", err)
	}
	return b64
}

func cidStrkey(t *testing.T, cid xdr.ContractId) string {
	t.Helper()
	s, err := strkey.Encode(strkey.VersionByteContract, cid[:])
	if err != nil {
		t.Fatalf("encode contract strkey: %v", err)
	}
	return s
}

func TestSoroswapStateFromInstanceEntry(t *testing.T) {
	var tok0, tok1 xdr.ContractId
	tok0[0], tok1[0] = 0x01, 0x02

	t.Run("full layout decodes, hi word preserved (ADR-0003)", func(t *testing.T) {
		// Reserve0 spans past int64: hi=3, lo=5 → 3·2^64 + 5.
		b64 := mkSoroswapPairEntry(t, tok0, tok1,
			xdr.Int128Parts{Hi: 3, Lo: 5},
			xdr.Int128Parts{Hi: 0, Lo: 603291773585},
		)
		st, ok := soroswapStateFromInstanceEntry(b64)
		if !ok {
			t.Fatal("expected ok=true")
		}
		wantR0 := new(big.Int).Add(
			new(big.Int).Lsh(big.NewInt(3), 64), big.NewInt(5))
		if st.Reserve0.Cmp(wantR0) != 0 {
			t.Fatalf("Reserve0 = %s, want %s (hi word must not truncate)", st.Reserve0, wantR0)
		}
		if got, want := st.Reserve1.String(), "603291773585"; got != want {
			t.Fatalf("Reserve1 = %s, want %s", got, want)
		}
		if st.Token0 != cidStrkey(t, tok0) || st.Token1 != cidStrkey(t, tok1) {
			t.Fatalf("token strkeys = (%s, %s)", st.Token0, st.Token1)
		}
	})

	t.Run("missing reserve key refuses to guess", func(t *testing.T) {
		b64 := mkSoroswapPairEntry(t, tok0, tok1,
			xdr.Int128Parts{Hi: 0, Lo: 1}, xdr.Int128Parts{Hi: 0, Lo: 2},
			soroswapKeyReserve1)
		if _, ok := soroswapStateFromInstanceEntry(b64); ok {
			t.Fatal("expected ok=false when Reserve1 is absent")
		}
	})

	t.Run("missing token key refuses to guess", func(t *testing.T) {
		b64 := mkSoroswapPairEntry(t, tok0, tok1,
			xdr.Int128Parts{Hi: 0, Lo: 1}, xdr.Int128Parts{Hi: 0, Lo: 2},
			soroswapKeyToken0)
		if _, ok := soroswapStateFromInstanceEntry(b64); ok {
			t.Fatal("expected ok=false when Token0 is absent")
		}
	})

	t.Run("non-instance entry rejected", func(t *testing.T) {
		if _, ok := soroswapStateFromInstanceEntry("not-xdr"); ok {
			t.Fatal("expected ok=false on garbage input")
		}
	})

	t.Run("zero reserves decode as zero, not failure", func(t *testing.T) {
		b64 := mkSoroswapPairEntry(t, tok0, tok1,
			xdr.Int128Parts{}, xdr.Int128Parts{})
		st, ok := soroswapStateFromInstanceEntry(b64)
		if !ok {
			t.Fatal("expected ok=true for zero reserves")
		}
		if st.Reserve0.Sign() != 0 || st.Reserve1.Sign() != 0 {
			t.Fatalf("reserves = (%s, %s), want zeros", st.Reserve0, st.Reserve1)
		}
	})
}

func TestDisplayMetaFromInstanceEntry(t *testing.T) {
	t.Run("full metadata", func(t *testing.T) {
		b64 := mkTokenMetaEntry(t, "USDC", "USD Coin", 7)
		meta, ok := displayMetaFromInstanceEntry(b64)
		if !ok || !meta.HasMeta {
			t.Fatalf("expected metadata, got ok=%v meta=%+v", ok, meta)
		}
		if meta.Symbol != "USDC" || meta.Name != "USD Coin" || meta.Decimals != 7 {
			t.Fatalf("meta = %+v", meta)
		}
	})
	t.Run("no metadata map", func(t *testing.T) {
		b64 := mkInstanceEntry(t, false, 0)
		if _, ok := displayMetaFromInstanceEntry(b64); ok {
			t.Fatal("expected ok=false without METADATA")
		}
	})
	t.Run("insane decimals rejected wholesale", func(t *testing.T) {
		b64 := mkTokenMetaEntry(t, "EVIL", "Evil Token", maxSaneTokenDecimals+1)
		if _, ok := displayMetaFromInstanceEntry(b64); ok {
			t.Fatal("expected ok=false for out-of-bounds decimals")
		}
	})
}

// mkTokenMetaEntry builds an instance entry with a complete METADATA
// map (decimal + name + symbol) — the shape TokenDisplays consumes.
func mkTokenMetaEntry(t *testing.T, symbol, name string, decimal uint32) string {
	t.Helper()
	metaSym := xdr.ScSymbol("METADATA")
	decSym := xdr.ScSymbol("decimal")
	nameSym := xdr.ScSymbol("name")
	symSym := xdr.ScSymbol("symbol")
	nameStr := xdr.ScString(name)
	symStr := xdr.ScString(symbol)
	dec := xdr.Uint32(decimal)
	inner := xdr.ScMap{
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &decSym}, Val: xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &dec}},
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &nameSym}, Val: xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &nameStr}},
		{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &symSym}, Val: xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &symStr}},
	}
	innerPtr := &inner
	storage := xdr.ScMap{{
		Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &metaSym},
		Val: xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &innerPtr},
	}}
	inst := xdr.ScContractInstance{
		Executable: xdr.ContractExecutable{Type: xdr.ContractExecutableTypeContractExecutableWasm, WasmHash: &xdr.Hash{3}},
		Storage:    &storage,
	}
	var cid xdr.ContractId
	cid[0] = 0xEF
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
		t.Fatalf("marshal token meta entry: %v", err)
	}
	return b64
}
