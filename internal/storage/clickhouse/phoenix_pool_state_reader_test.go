package clickhouse

import (
	"math/big"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

// mkPersistentDataEntry wraps an ScVal into a base64 persistent
// contract_data LedgerEntry — the wire shape phoenix/comet pool
// entries carry in the lake.
func mkPersistentDataEntry(t *testing.T, key, val xdr.ScVal) string {
	t.Helper()
	var cid xdr.ContractId
	cid[0] = 0x77
	entry := xdr.LedgerEntry{
		Data: xdr.LedgerEntryData{
			Type: xdr.LedgerEntryTypeContractData,
			ContractData: &xdr.ContractDataEntry{
				Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &cid},
				Key:        key,
				Durability: xdr.ContractDataDurabilityPersistent,
				Val:        val,
			},
		},
	}
	b64, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatalf("marshal persistent data entry: %v", err)
	}
	return b64
}

func i128ScVal(p xdr.Int128Parts) xdr.ScVal {
	pp := p
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &pp}
}

func addrScVal(cid xdr.ContractId) xdr.ScVal {
	c := cid
	return xdr.ScVal{Type: xdr.ScValTypeScvAddress, Address: &xdr.ScAddress{
		Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &c,
	}}
}

func mapScVal(entries ...xdr.ScMapEntry) xdr.ScVal {
	m := xdr.ScMap(entries)
	mp := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
}

// mkPhoenixConfigEntry builds a CONFIG entry with the source-derived
// Config map shape (token_a/token_b + a few of the other fields, to
// prove decode-by-field-name ignores them).
func mkPhoenixConfigEntry(t *testing.T, tokA, tokB xdr.ContractId, omit ...string) string {
	t.Helper()
	omitted := map[string]bool{}
	for _, o := range omit {
		omitted[o] = true
	}
	feeBps := xdr.Int64(30)
	var entries []xdr.ScMapEntry
	add := func(name string, val xdr.ScVal) {
		if omitted[name] {
			return
		}
		entries = append(entries, xdr.ScMapEntry{Key: symbolScVal(name), Val: val})
	}
	var share xdr.ContractId
	share[0] = 0x33
	add("fee_recipient", addrScVal(share))
	add("share_token", addrScVal(share))
	add(phoenixFieldTokenA, addrScVal(tokA))
	add(phoenixFieldTokenB, addrScVal(tokB))
	add("total_fee_bps", xdr.ScVal{Type: xdr.ScValTypeScvI64, I64: &feeBps})
	return mkPersistentDataEntry(t, symbolScVal(phoenixConfigKeySymbol), mapScVal(entries...))
}

func TestPhoenixReserveFromEntry(t *testing.T) {
	t.Run("i128 decodes, hi word preserved (ADR-0003)", func(t *testing.T) {
		b64 := mkPersistentDataEntry(t, u32ScVal(phoenixKeyReserveA),
			i128ScVal(xdr.Int128Parts{Hi: 7, Lo: 11}))
		got, ok := phoenixReserveFromEntry(b64)
		if !ok {
			t.Fatal("expected ok=true")
		}
		want := new(big.Int).Add(new(big.Int).Lsh(big.NewInt(7), 64), big.NewInt(11))
		if got.Cmp(want) != 0 {
			t.Fatalf("reserve = %s, want %s (hi word must not truncate)", got, want)
		}
	})

	t.Run("non-i128 value refuses to guess", func(t *testing.T) {
		b64 := mkPersistentDataEntry(t, u32ScVal(phoenixKeyReserveA), u32ScVal(42))
		if _, ok := phoenixReserveFromEntry(b64); ok {
			t.Fatal("expected ok=false for a u32-typed reserve value")
		}
	})

	t.Run("garbage input rejected", func(t *testing.T) {
		if _, ok := phoenixReserveFromEntry("not-xdr"); ok {
			t.Fatal("expected ok=false on garbage input")
		}
	})
}

func TestPhoenixTokensFromConfigEntry(t *testing.T) {
	var tokA, tokB xdr.ContractId
	tokA[0], tokB[0] = 0x01, 0x02

	t.Run("token_a/token_b decode by field name, extras ignored", func(t *testing.T) {
		a, b, ok := phoenixTokensFromConfigEntry(mkPhoenixConfigEntry(t, tokA, tokB))
		if !ok {
			t.Fatal("expected ok=true")
		}
		if a != cidStrkey(t, tokA) || b != cidStrkey(t, tokB) {
			t.Fatalf("tokens = (%s, %s)", a, b)
		}
	})

	t.Run("missing token_b refuses to guess", func(t *testing.T) {
		if _, _, ok := phoenixTokensFromConfigEntry(
			mkPhoenixConfigEntry(t, tokA, tokB, phoenixFieldTokenB)); ok {
			t.Fatal("expected ok=false when token_b is absent")
		}
	})

	t.Run("mis-typed token field fails the entry", func(t *testing.T) {
		bad := mkPersistentDataEntry(t, symbolScVal(phoenixConfigKeySymbol), mapScVal(
			xdr.ScMapEntry{Key: symbolScVal(phoenixFieldTokenA), Val: u32ScVal(1)},
			xdr.ScMapEntry{Key: symbolScVal(phoenixFieldTokenB), Val: addrScVal(tokB)},
		))
		if _, _, ok := phoenixTokensFromConfigEntry(bad); ok {
			t.Fatal("expected ok=false when token_a isn't an Address")
		}
	})

	t.Run("non-map value rejected", func(t *testing.T) {
		bad := mkPersistentDataEntry(t, symbolScVal(phoenixConfigKeySymbol),
			i128ScVal(xdr.Int128Parts{Lo: 1}))
		if _, _, ok := phoenixTokensFromConfigEntry(bad); ok {
			t.Fatal("expected ok=false for a non-map CONFIG value")
		}
	})
}

func TestPhoenixPoolPartsAssembly(t *testing.T) {
	var tokA, tokB xdr.ContractId
	tokA[0], tokB[0] = 0x0A, 0x0B

	t.Run("all three entries → complete", func(t *testing.T) {
		p := &phoenixPoolParts{}
		applyPhoenixEntry(p, phoenixKindReserveA,
			mkPersistentDataEntry(t, u32ScVal(1), i128ScVal(xdr.Int128Parts{Lo: 100})))
		applyPhoenixEntry(p, phoenixKindReserveB,
			mkPersistentDataEntry(t, u32ScVal(2), i128ScVal(xdr.Int128Parts{Lo: 200})))
		applyPhoenixEntry(p, phoenixKindConfig, mkPhoenixConfigEntry(t, tokA, tokB))
		if !p.complete() {
			t.Fatalf("expected complete parts, got %+v", p)
		}
	})

	t.Run("missing config → incomplete (undecodable, not guessed)", func(t *testing.T) {
		p := &phoenixPoolParts{}
		applyPhoenixEntry(p, phoenixKindReserveA,
			mkPersistentDataEntry(t, u32ScVal(1), i128ScVal(xdr.Int128Parts{Lo: 100})))
		applyPhoenixEntry(p, phoenixKindReserveB,
			mkPersistentDataEntry(t, u32ScVal(2), i128ScVal(xdr.Int128Parts{Lo: 200})))
		if p.complete() {
			t.Fatal("expected incomplete without the CONFIG entry")
		}
	})

	t.Run("one malformed entry poisons the pool", func(t *testing.T) {
		p := &phoenixPoolParts{}
		applyPhoenixEntry(p, phoenixKindReserveA,
			mkPersistentDataEntry(t, u32ScVal(1), u32ScVal(9)))
		applyPhoenixEntry(p, phoenixKindReserveB,
			mkPersistentDataEntry(t, u32ScVal(2), i128ScVal(xdr.Int128Parts{Lo: 200})))
		applyPhoenixEntry(p, phoenixKindConfig, mkPhoenixConfigEntry(t, tokA, tokB))
		if p.complete() {
			t.Fatal("expected shape-bad pool to stay incomplete")
		}
	})
}

// TestPhoenixPoolKeys pins the exact LedgerKeys the reader probes: 3
// persistent contract_data keys per pool — ScvU32(1), ScvU32(2), and
// ScvSymbol("CONFIG") — decoded back from the marshalled base64.
func TestPhoenixPoolKeys(t *testing.T) {
	var cid xdr.ContractId
	cid[0] = 0x55
	pool := cidStrkey(t, cid)
	keys, refByKey, err := phoenixPoolKeys([]string{pool})
	if err != nil {
		t.Fatalf("phoenixPoolKeys: %v", err)
	}
	if len(keys) != 3 || len(refByKey) != 3 {
		t.Fatalf("keys = %d refs = %d, want 3/3", len(keys), len(refByKey))
	}
	seen := map[phoenixKeyKind]bool{}
	for _, k := range keys {
		var lk xdr.LedgerKey
		if err := xdr.SafeUnmarshalBase64(k, &lk); err != nil {
			t.Fatalf("unmarshal key: %v", err)
		}
		cd := lk.ContractData
		if cd == nil || cd.Durability != xdr.ContractDataDurabilityPersistent {
			t.Fatalf("key %q is not a persistent contract_data key", k)
		}
		ref := refByKey[k]
		if ref.pool != pool {
			t.Fatalf("key ref pool = %q, want %q", ref.pool, pool)
		}
		seen[ref.kind] = true
		switch ref.kind {
		case phoenixKindReserveA, phoenixKindReserveB:
			want := xdr.Uint32(phoenixKeyReserveA)
			if ref.kind == phoenixKindReserveB {
				want = phoenixKeyReserveB
			}
			if u, ok := cd.Key.GetU32(); !ok || u != want {
				t.Fatalf("reserve key = %+v, want ScvU32(%d)", cd.Key, want)
			}
		case phoenixKindConfig:
			if s, ok := cd.Key.GetSym(); !ok || string(s) != phoenixConfigKeySymbol {
				t.Fatalf("config key = %+v, want ScvSymbol(%q)", cd.Key, phoenixConfigKeySymbol)
			}
		}
	}
	if len(seen) != 3 {
		t.Fatalf("kinds seen = %v, want all three", seen)
	}

	if _, _, err := phoenixPoolKeys([]string{"not-a-strkey"}); err == nil {
		t.Fatal("expected error for a malformed pool id")
	}
}

// TestPhoenixPoolStateQueryShape pins the SQL to the repo's bounded-
// lake-read conventions: current-state table with FINAL dedup, keyed
// probe, empty-entry filter, and the guard-rail SETTINGS.
func TestPhoenixPoolStateQueryShape(t *testing.T) {
	for _, s := range []string{
		"FROM stellar.ledger_entries_current FINAL",
		"entry_type = 'contract_data'",
		"key_xdr IN (?)",
		"entry_xdr != ''",
		"max_threads = 4",
		"max_memory_usage = 8000000000",
	} {
		if !strings.Contains(phoenixPoolStateQuery, s) {
			t.Errorf("phoenixPoolStateQuery missing %q", s)
		}
	}
}
