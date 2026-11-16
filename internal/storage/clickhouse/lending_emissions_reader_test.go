package clickhouse

import (
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
)

func emisU64(n uint64) xdr.ScVal {
	v := xdr.Uint64(n)
	return xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &v}
}

func emisI128(lo uint64) xdr.ScVal {
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &xdr.Int128Parts{Hi: 1, Lo: xdr.Uint64(lo)}}
}

func emisBool(b bool) xdr.ScVal { return xdr.ScVal{Type: xdr.ScValTypeScvBool, B: &b} }

// emisRoundTrip passes v through a marshalled ContractData LedgerEntry, the form ledger_entries_current holds.
func emisRoundTrip(t *testing.T, contract string, key, v xdr.ScVal) xdr.ScVal {
	t.Helper()
	entry := xdr.LedgerEntry{Data: xdr.LedgerEntryData{Type: xdr.LedgerEntryTypeContractData, ContractData: &xdr.ContractDataEntry{
		Contract: testScAddress(t, contract), Key: key, Durability: xdr.ContractDataDurabilityPersistent, Val: v,
	}}}
	b64, err := xdr.MarshalBase64(entry)
	if err != nil {
		t.Fatal(err)
	}
	out, ok := contractDataValue(b64)
	if !ok {
		t.Fatal("contractDataValue rejected a marshalled entry")
	}
	return out
}

// TestDecodeEmissionData_EveryLayout decodes Blend V2's 4-field struct and BLNT's wider stored pool and backstop structs.
func TestDecodeEmissionData_EveryLayout(t *testing.T) {
	pool := blnt.MainnetFixedPool
	key := testEnumKey("EmisData", testU32(3))
	cases := map[string]xdr.ScVal{
		"blend_v2": testStruct("eps", emisU64(150_000_000_000_000), "expiration", emisU64(1_800_000_000),
			"index", emisI128(5), "last_time", emisU64(1_700_000_000)),
		"blnt_pool": testStruct("carry_initialized", emisBool(true), "eps", emisU64(150_000_000_000_000),
			"expiration", emisU64(1_800_000_000), "index", emisI128(5), "index_carry", emisI128(0),
			"last_time", emisU64(1_700_000_000), "remaining", emisI128(9)),
		"blnt_backstop": testStruct("eps", emisU64(150_000_000_000_000), "expiration", emisU64(1_800_000_000),
			"index", emisI128(5), "index_carry", emisI128(0), "last_time", emisU64(1_700_000_000), "schedule_carry", emisI128(1)),
	}
	want := EmissionData{EPS: 150_000_000_000_000, Expiration: 1_800_000_000, LastTime: 1_700_000_000}
	for name, v := range cases {
		got, err := decodeEmissionData(emisRoundTrip(t, pool, key, v))
		if err != nil || got != want {
			t.Errorf("%s: got %+v, %v; want %+v", name, got, err, want)
		}
	}
	if _, err := decodeEmissionData(testStruct("eps", emisU64(1), "expiration", emisU64(2))); err == nil {
		t.Error("a struct without last_time decoded")
	}
	if _, err := decodeEmissionData(testStruct("eps", testU32(1), "expiration", emisU64(2), "last_time", emisU64(3))); err == nil {
		t.Error("a u32 eps decoded")
	}
}

func TestReserveTokenSide(t *testing.T) {
	reserves := []string{"CA", "CB"}
	for _, c := range []struct {
		id          uint32
		asset, side string
		ok          bool
	}{
		{0, "CA", EmissionSideBorrow, true},
		{1, "CA", EmissionSideSupply, true},
		{2, "CB", EmissionSideBorrow, true},
		{3, "CB", EmissionSideSupply, true},
		{4, "", EmissionSideBorrow, false},
		{9, "", EmissionSideSupply, false},
	} {
		a, s, ok := ReserveTokenSide(c.id, reserves)
		if a != c.asset || s != c.side || ok != c.ok {
			t.Errorf("id %d = (%q, %q, %v), want (%q, %q, %v)", c.id, a, s, ok, c.asset, c.side, c.ok)
		}
	}
}

// TestEmissionStreamKeys_MatchContractKeys pins the keys against independently built EmisData(u32), BEmisData(Address)
// and BEmisData(PoolTierKey) keys.
func TestEmissionStreamKeys_MatchContractKeys(t *testing.T) {
	pool, backstop := blnt.MainnetFixedPool, blnt.MainnetBackstop
	st := LendingEmissionsState{Instance: &PoolInstance{Backstop: backstop}, ReserveList: []string{"CA", "CB"}}
	poolID := mustContractID(t, pool)

	refs, err := emissionStreamKeys(poolID, st, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 5 {
		t.Fatalf("blend keys = %d, want 4 EmisData + 1 BEmisData", len(refs))
	}
	if r := refs[testDataKey(t, pool, testEnumKey("EmisData", testU32(3)))]; r.kind != emisKindReserve || r.id != 3 {
		t.Errorf("EmisData(3) ref = %+v", r)
	}
	if r := refs[testDataKey(t, backstop, testEnumKey("BEmisData", testAddrVal(t, pool)))]; r.kind != emisKindBackstop || r.tier != "" {
		t.Errorf("BEmisData(pool) ref = %+v", r)
	}

	refs, err = emissionStreamKeys(poolID, st, true)
	if err != nil {
		t.Fatal(err)
	}
	tierKey := testStruct("pool", testAddrVal(t, pool), "tier", testUnitKey("ThirdLoss"))
	if r := refs[testDataKey(t, backstop, testEnumKey("BEmisData", tierKey))]; r.kind != emisKindBackstop || r.tier != "ThirdLoss" {
		t.Errorf("BEmisData(PoolTierKey ThirdLoss) ref = %+v", r)
	}
	if len(refs) != 7 {
		t.Errorf("blnt keys = %d, want 4 EmisData + 3 BEmisData", len(refs))
	}

	head, err := poolHeadKeys(poolID)
	if err != nil {
		t.Fatal(err)
	}
	if r := head[testDataKey(t, pool, testSym("ResList"))]; r.kind != emisKindResList {
		t.Errorf("ResList key missing: %+v", head)
	}
}

// TestAssembleEmissions maps streams to reserves in token-id order, keeps an archived stream marked, skips an
// undecodable one, and serves only the backstop tiers with an entry.
func TestAssembleEmissions(t *testing.T) {
	pool, backstop := blnt.MainnetFixedPool, blnt.MainnetBackstop
	stream := func(eps uint64) xdr.ScVal {
		return testStruct("eps", emisU64(eps), "expiration", emisU64(10), "index", emisI128(0), "last_time", emisU64(5))
	}
	resList := &xdr.ScVec{testAddrVal(t, pool), testAddrVal(t, backstop)}
	resListVal := emisRoundTrip(t, pool, testSym("ResList"), xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &resList})
	head := assembleEmissionHead(map[string]xdr.ScVal{"rl": resListVal}, nil, map[string]emisRef{"rl": {kind: emisKindResList}}, true)
	if len(head.ReserveList) != 2 || head.ReserveList[1] != backstop {
		t.Fatalf("ResList = %v", head.ReserveList)
	}
	head.Instance = &PoolInstance{Backstop: backstop}
	refs := map[string]emisRef{
		"e3": {kind: emisKindReserve, id: 3}, "e0": {kind: emisKindReserve, id: 0}, "e1": {kind: emisKindReserve, id: 1},
		"e9": {kind: emisKindReserve, id: 9}, "b1": {kind: emisKindBackstop, tier: "SecondLoss"},
	}
	vals := map[string]xdr.ScVal{
		"e3": stream(3), "e0": stream(0), "e1": testSym("junk"), "e9": stream(9), "b1": stream(7),
	}
	assembleEmissionStreams(&head, vals, map[string]bool{"e0": true}, refs)
	r := head.Reserves
	if len(r) != 3 || r[0].TokenID != 0 || !r[0].Archived || r[0].Asset != pool || r[0].Side != EmissionSideBorrow ||
		r[1].TokenID != 3 || r[1].Asset != backstop || r[1].Side != EmissionSideSupply || r[1].Data.EPS != 3 ||
		r[2].TokenID != 9 || r[2].Asset != "" {
		t.Fatalf("reserves = %+v", r)
	}
	if len(head.Backstop) != 1 || head.Backstop[0].Tier != "SecondLoss" || head.Backstop[0].Data.EPS != 7 {
		t.Fatalf("backstop = %+v", head.Backstop)
	}
}
