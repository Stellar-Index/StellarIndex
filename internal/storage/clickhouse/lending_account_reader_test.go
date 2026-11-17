package clickhouse

import (
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

func u32Val(n uint32) xdr.ScVal {
	v := xdr.Uint32(n)
	return xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &v}
}

func mapVal(entries ...xdr.ScMapEntry) xdr.ScVal {
	m := xdr.ScMap(entries)
	mp := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
}

func vecVal(items ...xdr.ScVal) xdr.ScVal {
	v := xdr.ScVec(items)
	vp := &v
	return xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vp}
}

func indexMap(t *testing.T, kv map[uint32]string) xdr.ScVal {
	t.Helper()
	var es []xdr.ScMapEntry
	for k, v := range kv {
		es = append(es, xdr.ScMapEntry{Key: u32Val(k), Val: i128Val(t, v)})
	}
	return mapVal(es...)
}

func reserveListVal(t *testing.T, assets ...xdr.ContractId) xdr.ScVal {
	t.Helper()
	items := make([]xdr.ScVal, 0, len(assets))
	for _, a := range assets {
		items = append(items, contractAddressVal(a))
	}
	return vecVal(items...)
}

// TestAssembleAccountPositions_MapsIndexesAndSumsCollateral maps reserve indexes through ResList, sums collateral and supply
// b-tokens exactly above 2^63, and keeps d-tokens apart.
func TestAssembleAccountPositions_MapsIndexesAndSumsCollateral(t *testing.T) {
	a0, a1 := xdr.ContractId{1}, xdr.ContractId{2}
	s0, s1 := strkeyOf(t, a0), strkeyOf(t, a1)
	pos := mapVal(
		xdr.ScMapEntry{Key: symVal("collateral"), Val: indexMap(t, map[uint32]string{0: "18446744073709551616"})},
		xdr.ScMapEntry{Key: symVal("liabilities"), Val: indexMap(t, map[uint32]string{1: "7"})},
		xdr.ScMapEntry{Key: symVal("supply"), Val: indexMap(t, map[uint32]string{0: "1"})},
	)
	vals := map[string]xdr.ScVal{"pos": pos, "list": reserveListVal(t, a0, a1)}
	got := assembleAccountPositions(vals, map[string]bool{}, "pos", "list")
	if !got.Readable || got.Supply[s0].String() != "18446744073709551617" || got.Borrow[s1].String() != "7" ||
		got.Supply[s1] != nil || got.Borrow[s0] != nil {
		t.Fatalf("got %+v", got)
	}
}

// TestAssembleAccountPositions_AbsentArchivedAndBadIndex: a missing entry is a readable empty position; an archived entry or
// an index outside ResList is unreadable, never zero.
func TestAssembleAccountPositions_AbsentArchivedAndBadIndex(t *testing.T) {
	list := reserveListVal(t, xdr.ContractId{1})
	if got := assembleAccountPositions(map[string]xdr.ScVal{"list": list}, map[string]bool{}, "pos", "list"); !got.Readable || len(got.Supply) != 0 {
		t.Fatalf("absent entry = %+v, want readable and empty", got)
	}
	pos := mapVal(
		xdr.ScMapEntry{Key: symVal("collateral"), Val: indexMap(t, nil)},
		xdr.ScMapEntry{Key: symVal("liabilities"), Val: indexMap(t, nil)},
		xdr.ScMapEntry{Key: symVal("supply"), Val: indexMap(t, map[uint32]string{1: "5"})},
	)
	vals := map[string]xdr.ScVal{"pos": pos, "list": list}
	if got := assembleAccountPositions(vals, map[string]bool{}, "pos", "list"); got.Readable {
		t.Fatalf("index outside ResList read as %+v", got)
	}
	if got := assembleAccountPositions(vals, map[string]bool{"pos": true}, "pos", "list"); got.Readable {
		t.Fatalf("archived entry read as %+v", got)
	}
	if got := assembleAccountPositions(map[string]xdr.ScVal{"pos": pos}, map[string]bool{}, "pos", "list"); got.Readable {
		t.Fatalf("entry without ResList read as %+v", got)
	}
}

// TestDecodeUserBalance_CountsQueuedShares: queued-for-withdrawal shares are still held.
func TestDecodeUserBalance_CountsQueuedShares(t *testing.T) {
	q := func(amount string) xdr.ScVal {
		return mapVal(xdr.ScMapEntry{Key: symVal("amount"), Val: i128Val(t, amount)}, xdr.ScMapEntry{Key: symVal("exp"), Val: u32Val(0)})
	}
	v := mapVal(
		xdr.ScMapEntry{Key: symVal("q4w"), Val: vecVal(q("9223372036854775808"), q("2"))},
		xdr.ScMapEntry{Key: symVal("shares"), Val: i128Val(t, "9223372036854775807")},
	)
	got, err := decodeUserBalance(v)
	want, _ := new(big.Int).SetString("18446744073709551617", 10)
	if err != nil || got.Cmp(want) != 0 {
		t.Fatalf("held = %v, %v; want %s", got, err, want)
	}
}

// TestUserBalanceKey_TierLayout: Blend and BLNT SecondLoss use UserBalance{pool, user}; other BLNT tiers use
// TierUserBalance{pool, tier, user}.
func TestUserBalanceKey_TierLayout(t *testing.T) {
	user, err := addressScVal("GAAZI4TCR3TY5OJHCTJC2A4QSY6CJWJH5IAJTGKIN2ER7LBNVKOCCWN7")
	if err != nil {
		t.Fatal(err)
	}
	for tier, variant := range map[string]string{"": "UserBalance", "SecondLoss": "UserBalance", "FirstLoss": "TierUserBalance"} {
		k, err := userBalanceKeyXDR(xdr.ContractId{9}, xdr.ContractId{8}, user, tier)
		if err != nil {
			t.Fatal(err)
		}
		var lk xdr.LedgerKey
		if err := xdr.SafeUnmarshalBase64(k, &lk); err != nil {
			t.Fatal(err)
		}
		vec := *lk.ContractData.Key.Vec
		fields := *(*vec)[1].Map
		if string(*(*vec)[0].Sym) != variant || len(*fields) != map[bool]int{true: 2, false: 3}[variant == "UserBalance"] {
			t.Fatalf("tier %q key = %s with %d fields", tier, *(*vec)[0].Sym, len(*fields))
		}
	}
}

func strkeyOf(t *testing.T, id xdr.ContractId) string {
	t.Helper()
	addr := contractAddressVal(id)
	s, err := addr.Address.String()
	if err != nil {
		t.Fatal(err)
	}
	return s
}
