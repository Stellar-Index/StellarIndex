package clickhouse

import (
	"math/big"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// mkCometRecord builds a Record ScvMap. Field order/spelling per the
// generation being simulated; only `balance` is load-bearing.
func mkCometRecordNew(balance xdr.Int128Parts) xdr.ScVal {
	idx := xdr.Uint32(0)
	return mapScVal(
		xdr.ScMapEntry{Key: symbolScVal("balance"), Val: i128ScVal(balance)},
		xdr.ScMapEntry{Key: symbolScVal("index"), Val: xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &idx}},
		xdr.ScMapEntry{Key: symbolScVal("scalar"), Val: i128ScVal(xdr.Int128Parts{Lo: 10_000_000})},
		xdr.ScMapEntry{Key: symbolScVal("weight"), Val: i128ScVal(xdr.Int128Parts{Lo: 5_000_000})},
	)
}

// mkCometRecordOld simulates the earlier Record generation
// (bound/index/denorm/balance) — decode-by-field-name must read it too.
func mkCometRecordOld(balance xdr.Int128Parts) xdr.ScVal {
	idx := xdr.Uint32(1)
	bound := true
	return mapScVal(
		xdr.ScMapEntry{Key: symbolScVal("balance"), Val: i128ScVal(balance)},
		xdr.ScMapEntry{Key: symbolScVal("bound"), Val: xdr.ScVal{Type: xdr.ScValTypeScvBool, B: &bound}},
		xdr.ScMapEntry{Key: symbolScVal("denorm"), Val: i128ScVal(xdr.Int128Parts{Lo: 5_0000000})},
		xdr.ScMapEntry{Key: symbolScVal("index"), Val: xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &idx}},
	)
}

// mkCometRecordDataEntry wraps token→record pairs into a base64
// AllRecordData persistent entry.
func mkCometRecordDataEntry(t *testing.T, entries ...xdr.ScMapEntry) string {
	t.Helper()
	sym := xdr.ScSymbol(cometRecordDataKeySymbol)
	vec := &xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &sym}}
	key := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vec}
	return mkPersistentDataEntry(t, key, mapScVal(entries...))
}

func TestCometLegsFromRecordEntry(t *testing.T) {
	var blnd, usdc xdr.ContractId
	blnd[0], usdc[0] = 0xB1, 0x0C

	t.Run("both record generations decode, hi word preserved (ADR-0003)", func(t *testing.T) {
		b64 := mkCometRecordDataEntry(t,
			xdr.ScMapEntry{Key: addrScVal(blnd), Val: mkCometRecordNew(xdr.Int128Parts{Hi: 2, Lo: 3})},
			xdr.ScMapEntry{Key: addrScVal(usdc), Val: mkCometRecordOld(xdr.Int128Parts{Hi: 0, Lo: 44_000_0000})},
		)
		legs, ok := cometLegsFromRecordEntry(b64)
		if !ok {
			t.Fatal("expected ok=true")
		}
		if len(legs) != 2 {
			t.Fatalf("legs = %d, want 2", len(legs))
		}
		byToken := map[string]*big.Int{}
		for _, l := range legs {
			byToken[l.Token] = l.Balance
		}
		wantBlnd := new(big.Int).Add(new(big.Int).Lsh(big.NewInt(2), 64), big.NewInt(3))
		if got := byToken[cidStrkey(t, blnd)]; got == nil || got.Cmp(wantBlnd) != 0 {
			t.Fatalf("blnd balance = %v, want %s (hi word must not truncate)", got, wantBlnd)
		}
		if got := byToken[cidStrkey(t, usdc)]; got == nil || got.String() != "440000000" {
			t.Fatalf("usdc balance = %v, want 440000000", got)
		}
		// Deterministic ordering by token strkey.
		if legs[0].Token > legs[1].Token {
			t.Fatal("legs must be sorted by token strkey")
		}
	})

	t.Run("empty record map is a valid zero-leg pool", func(t *testing.T) {
		legs, ok := cometLegsFromRecordEntry(mkCometRecordDataEntry(t))
		if !ok || len(legs) != 0 {
			t.Fatalf("ok=%v legs=%d, want ok with 0 legs", ok, len(legs))
		}
	})

	t.Run("record missing balance fails the whole entry", func(t *testing.T) {
		idx := xdr.Uint32(0)
		noBalance := mapScVal(
			xdr.ScMapEntry{Key: symbolScVal("index"), Val: xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &idx}},
		)
		b64 := mkCometRecordDataEntry(t,
			xdr.ScMapEntry{Key: addrScVal(blnd), Val: mkCometRecordNew(xdr.Int128Parts{Lo: 1})},
			xdr.ScMapEntry{Key: addrScVal(usdc), Val: noBalance},
		)
		if _, ok := cometLegsFromRecordEntry(b64); ok {
			t.Fatal("expected ok=false when one record lacks balance")
		}
	})

	t.Run("non-i128 balance refuses to guess", func(t *testing.T) {
		badBalance := mapScVal(
			xdr.ScMapEntry{Key: symbolScVal("balance"), Val: u32ScVal(9)},
		)
		b64 := mkCometRecordDataEntry(t,
			xdr.ScMapEntry{Key: addrScVal(blnd), Val: badBalance},
		)
		if _, ok := cometLegsFromRecordEntry(b64); ok {
			t.Fatal("expected ok=false for a u32-typed balance")
		}
	})

	t.Run("non-address record key fails the entry", func(t *testing.T) {
		b64 := mkCometRecordDataEntry(t,
			xdr.ScMapEntry{Key: u32ScVal(7), Val: mkCometRecordNew(xdr.Int128Parts{Lo: 1})},
		)
		if _, ok := cometLegsFromRecordEntry(b64); ok {
			t.Fatal("expected ok=false for a non-Address record key")
		}
	})

	t.Run("non-map value rejected", func(t *testing.T) {
		sym := xdr.ScSymbol(cometRecordDataKeySymbol)
		vec := &xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &sym}}
		key := xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vec}
		b64 := mkPersistentDataEntry(t, key, i128ScVal(xdr.Int128Parts{Lo: 1}))
		if _, ok := cometLegsFromRecordEntry(b64); ok {
			t.Fatal("expected ok=false for a non-map AllRecordData value")
		}
	})

	t.Run("garbage input rejected", func(t *testing.T) {
		if _, ok := cometLegsFromRecordEntry("not-xdr"); ok {
			t.Fatal("expected ok=false on garbage input")
		}
	})
}

// TestCometPoolKeys pins the candidate LedgerKeys: both plausible
// AllRecordData encodings (Vec[Symbol] per the contracttype spec +
// bare Symbol defensively), persistent durability, decoded back from
// the marshalled base64.
func TestCometPoolKeys(t *testing.T) {
	var cid xdr.ContractId
	cid[0] = 0x66
	pool := cidStrkey(t, cid)
	keys, err := cometPoolKeys(pool)
	if err != nil {
		t.Fatalf("cometPoolKeys: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("keys = %d, want 2 candidate encodings", len(keys))
	}
	var sawVec, sawSym bool
	for _, k := range keys {
		var lk xdr.LedgerKey
		if err := xdr.SafeUnmarshalBase64(k, &lk); err != nil {
			t.Fatalf("unmarshal key: %v", err)
		}
		cd := lk.ContractData
		if cd == nil || cd.Durability != xdr.ContractDataDurabilityPersistent {
			t.Fatalf("key %q is not a persistent contract_data key", k)
		}
		switch cd.Key.Type {
		case xdr.ScValTypeScvVec:
			vec := *cd.Key.Vec
			if len(*vec) != 1 {
				t.Fatalf("vec key arity = %d, want 1", len(*vec))
			}
			if s, ok := (*vec)[0].GetSym(); !ok || string(s) != cometRecordDataKeySymbol {
				t.Fatalf("vec key symbol = %+v, want %q", (*vec)[0], cometRecordDataKeySymbol)
			}
			sawVec = true
		case xdr.ScValTypeScvSymbol:
			if string(*cd.Key.Sym) != cometRecordDataKeySymbol {
				t.Fatalf("bare symbol key = %q, want %q", *cd.Key.Sym, cometRecordDataKeySymbol)
			}
			sawSym = true
		default:
			t.Fatalf("unexpected key type %s", cd.Key.Type)
		}
	}
	if !sawVec || !sawSym {
		t.Fatalf("candidates: vec=%v sym=%v, want both", sawVec, sawSym)
	}

	if _, err := cometPoolKeys("not-a-strkey"); err == nil {
		t.Fatal("expected error for a malformed pool id")
	}
}

// TestCometPoolStateQueryShape pins the SQL to the repo's bounded-
// lake-read conventions.
func TestCometPoolStateQueryShape(t *testing.T) {
	for _, s := range []string{
		"FROM stellar.ledger_entries_current FINAL",
		"entry_type = 'contract_data'",
		"key_xdr IN (?)",
		"entry_xdr != ''",
		"max_threads = 4",
		"max_memory_usage = 8000000000",
	} {
		if !strings.Contains(cometPoolStateQuery, s) {
			t.Errorf("cometPoolStateQuery missing %q", s)
		}
	}
}
