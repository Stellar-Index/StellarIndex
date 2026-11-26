package clickhouse

import (
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/lendinginterest"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
)

// blntTestPool is a second, BLNT pool beside blendTestPool.
const blntTestPool = blnt.MainnetYieldBlentPool

func liResDataEntry(t *testing.T, dRate, bSupply string) string {
	t.Helper()
	u64 := xdr.Uint64(1_700_000_000)
	return histEntry(t, histMap(
		xdr.ScMapEntry{Key: testSym("b_rate"), Val: i128Val(t, histBRate)},
		xdr.ScMapEntry{Key: testSym("b_supply"), Val: i128Val(t, bSupply)},
		xdr.ScMapEntry{Key: testSym("backstop_credit"), Val: i128Val(t, "12345")},
		xdr.ScMapEntry{Key: testSym("d_rate"), Val: i128Val(t, dRate)},
		xdr.ScMapEntry{Key: testSym("d_supply"), Val: i128Val(t, histDSupply)},
		xdr.ScMapEntry{Key: testSym("ir_mod"), Val: i128Val(t, "10000000")},
		xdr.ScMapEntry{Key: testSym("last_time"), Val: xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &u64}},
	))
}

func liFeesEntry(t *testing.T, credit, carry string) string {
	t.Helper()
	return histEntry(t, histMap(xdr.ScMapEntry{
		Key: testContractAddr(t, blendTestAssetSAC),
		Val: histMap(
			xdr.ScMapEntry{Key: testSym("carry"), Val: i128Val(t, carry)},
			xdr.ScMapEntry{Key: testSym("credit"), Val: i128Val(t, credit)},
		),
	}))
}

func liKey(kind lendinginterest.Kind, proto lendinginterest.Protocol) LendingInterestKey {
	k := LendingInterestKey{Key: "K", Pool: blendTestPool, Protocol: proto, Kind: kind}
	if kind == lendinginterest.KindReserve {
		k.Asset = blendTestAssetSAC
	}
	return k
}

func liChange(changeType, entry string) lendingInterestChange {
	return lendingInterestChange{
		Key: "K", TxHash: "aa", ChangeType: changeType, Entry: entry, Ledger: 61_000_000, Intra: 4, ChangeIndex: 2,
		OpIndex: 1, CloseTime: time.Date(2027, 1, 1, 10, 5, 0, 0, time.UTC),
	}
}

func TestLendingInterestKeyList_ResDataInstanceAndBLNTFees(t *testing.T) {
	pools := []LendingContract{{ID: blendTestPool, Protocol: LendingBlendV2}, {ID: blntTestPool, Protocol: LendingBLNT}}
	ps, err := LendingPrefixes(pools)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := lendingInterestKeyList(pools, ps, []lendingInterestResRow{{blendTestPool, blendTestResDataKey, LendingBlendV2}})
	if err != nil {
		t.Fatal(err)
	}
	blntFees, err := lendingUnitKey(mustContractID(t, blntTestPool), "ProtocolFees")
	if err != nil {
		t.Fatal(err)
	}
	blntInst, err := instanceKeyXDR(xdr.Hash(mustContractID(t, blntTestPool)))
	if err != nil {
		t.Fatal(err)
	}
	want := []LendingInterestKey{
		{Key: blendTestResDataKey, Pool: blendTestPool, Protocol: lendinginterest.BlendV2, Kind: lendinginterest.KindReserve, Asset: blendTestAssetSAC},
		{Key: blendTestInstanceKeyPersistent, Pool: blendTestPool, Protocol: lendinginterest.BlendV2, Kind: lendinginterest.KindConfig},
		{Key: blendTestInstanceKeyTemporary, Pool: blendTestPool, Protocol: lendinginterest.BlendV2, Kind: lendinginterest.KindConfig},
		{Key: blntInst[0], Pool: blntTestPool, Protocol: lendinginterest.BLNT, Kind: lendinginterest.KindConfig},
		{Key: blntInst[1], Pool: blntTestPool, Protocol: lendinginterest.BLNT, Kind: lendinginterest.KindConfig},
		{Key: blntFees, Pool: blntTestPool, Protocol: lendinginterest.BLNT, Kind: lendinginterest.KindFees},
	}
	if len(keys) != len(want) {
		t.Fatalf("keys = %+v, want %+v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("key %d = %+v, want %+v", i, keys[i], want[i])
		}
	}
}

func TestLendingInterestKeyList_Refusals(t *testing.T) {
	pool := []LendingContract{{ID: blendTestPool, Protocol: LendingBlendV2}}
	ps, err := LendingPrefixes(pool)
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		pools []LendingContract
		res   []lendingInterestResRow
		want  string
	}{
		"ResData key under another pool": {pool, []lendingInterestResRow{{blntTestPool, blendTestResDataKey, LendingBlendV2}}, "unrequested pool"},
		"key held by another contract": {
			[]LendingContract{{ID: blendTestPool, Protocol: LendingBlendV2}, {ID: blntTestPool, Protocol: LendingBLNT}},
			[]lendingInterestResRow{{blntTestPool, blendTestResDataKey, LendingBLNT}},
			"held by another contract",
		},
		"not a ResData key":   {pool, []lendingInterestResRow{{blendTestPool, blendTestInstanceKeyPersistent, LendingBlendV2}}, "not ResData(asset)"},
		"backstop":            {[]LendingContract{{ID: blendTestPool, Protocol: LendingBlendV2, Backstop: true}}, nil, "backstop"},
		"unknown protocol":    {[]LendingContract{{ID: blendTestPool, Protocol: "comet"}}, nil, "unknown protocol"},
		"undecodable key_xdr": {pool, []lendingInterestResRow{{blendTestPool, "!!", LendingBlendV2}}, "key !!"},
		// A registry edit must not silently switch a pool's rate scale.
		"registry protocol differs from the derived one": {
			pool, []lendingInterestResRow{{blendTestPool, blendTestResDataKey, LendingBlendV1}}, "is blend_v2 in the registry",
		},
	} {
		if _, err := lendingInterestKeyList(c.pools, ps, c.res); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want one containing %q", name, err, c.want)
		}
	}
}

func TestLendingInterestVersion_DecodesEachKind(t *testing.T) {
	rv, err := lendingInterestVersion(liKey(lendinginterest.KindReserve, lendinginterest.BLNT), liChange("updated", liResDataEntry(t, histDRate, histBSupply)))
	if err != nil {
		t.Fatal(err)
	}
	wantSupply, _ := new(big.Int).SetString(histBSupply, 10)
	if rv.Reserve.Version != blend.PoolV2 || rv.Reserve.BSupply.Cmp(wantSupply) != 0 || rv.Asset != blendTestAssetSAC || rv.Removed {
		t.Fatalf("reserve version = %+v, want V2 with b_supply %s", rv, wantSupply)
	}
	if rv.Ledger != 61_000_000 || rv.IntraLedgerSeq != 4 || rv.TxHash != "aa" || rv.OpIndex != 1 || rv.ChangeIndex != 2 ||
		rv.Kind != lendinginterest.KindReserve || rv.Pool != blendTestPool || rv.CloseTime.Hour() != 10 {
		t.Fatalf("reserve version position = %+v", rv)
	}
	cv, err := lendingInterestVersion(liKey(lendinginterest.KindConfig, lendinginterest.BlendV2), liChange("created", histInstance(t)))
	if err != nil || cv.BstopRate != 2_000_000 || cv.WasmHash != "" {
		t.Fatalf("config version = %+v (%v), want bstop_rate 2000000 and no WASM (a SAC executable)", cv, err)
	}
	wasmInst := poolInstanceVal(t, v2PoolConfigMap(t))
	hash := xdr.Hash{0xa4, 0x1f}
	wasmInst.Instance.Executable = xdr.ContractExecutable{Type: xdr.ContractExecutableTypeContractExecutableWasm, WasmHash: &hash}
	wv, err := lendingInterestVersion(liKey(lendinginterest.KindConfig, lendinginterest.BlendV2), liChange("updated", histEntry(t, wasmInst)))
	if want := "a41f" + strings.Repeat("0", 60); err != nil || wv.WasmHash != want {
		t.Fatalf("wasm instance version hash %q (%v), want %s", wv.WasmHash, err, want)
	}
	fv, err := lendingInterestVersion(liKey(lendinginterest.KindFees, lendinginterest.BLNT), liChange("restored", liFeesEntry(t, "70", "9999999")))
	if err != nil {
		t.Fatal(err)
	}
	if f := fv.Fees[blendTestAssetSAC]; len(fv.Fees) != 1 || f.Credit.Cmp(big.NewInt(70)) != 0 || f.Carry.Cmp(big.NewInt(9_999_999)) != 0 {
		t.Fatalf("fees version = %+v", fv.Fees)
	}
	// The lake writes no eviction of a persistent key, so a removal is the contract's delete: no fees stored.
	deleted, err := lendingInterestVersion(liKey(lendinginterest.KindFees, lendinginterest.BLNT), liChange("removed", ""))
	if err != nil || deleted.Removed || deleted.Fees == nil || len(deleted.Fees) != 0 {
		t.Fatalf("deleted ProtocolFees = %+v (%v), want an explicit empty map, not a removal gap", deleted, err)
	}
}

// Neither Blend nor BLNT deletes a ResData or instance entry, so one refuses the window naming the key.
func TestLendingInterestVersion_DeletedReserveOrInstanceRefuses(t *testing.T) {
	for _, kind := range []lendinginterest.Kind{lendinginterest.KindReserve, lendinginterest.KindConfig} {
		v, err := lendingInterestVersion(liKey(kind, lendinginterest.BlendV2), liChange("removed", ""))
		if err == nil || !strings.Contains(err.Error(), "ledger 61000000 tx aa key K") || !strings.Contains(err.Error(), "deleted its") {
			t.Errorf("kind %d: version %+v, err = %v; want a refusal naming the change", kind, v, err)
		}
	}
}

// A renumbering leftover (one change at two intra_ledger_seq) refuses; P23's restored-then-updated pair
// and a re-read at the same position do not.
func TestLendingInterestLeftovers(t *testing.T) {
	at := func(ct, entry string, intra, idx uint32) lendingInterestChange {
		c := liChange(ct, entry)
		c.Intra, c.ChangeIndex = intra, idx
		return c
	}
	e := liResDataEntry(t, histDRate, histBSupply)
	for name, c := range map[string]struct {
		rows  []lendingInterestChange
		bad   bool
		other func(*lendingInterestChange)
	}{
		"leftover":                 {[]lendingInterestChange{at("updated", e, 4, 2), at("updated", e, 9, 5)}, true, nil},
		"restored then updated":    {[]lendingInterestChange{at("restored", e, 4, 2), at("updated", e, 5, 3)}, false, nil},
		"same position, re-read":   {[]lendingInterestChange{at("updated", e, 4, 2), at("updated", e, 4, 5)}, false, nil},
		"other content":            {[]lendingInterestChange{at("updated", e, 4, 2), at("updated", "", 9, 5)}, false, nil},
		"same change, next ledger": {[]lendingInterestChange{at("updated", e, 4, 2), at("updated", e, 9, 5)}, false, func(c *lendingInterestChange) { c.Ledger++ }},
	} {
		var l lendingInterestLeftovers
		if c.other != nil {
			c.other(&c.rows[1])
		}
		var err error
		for _, r := range c.rows {
			if err = l.check(r); err != nil {
				break
			}
		}
		if bad := err != nil; bad != c.bad || (bad && !strings.Contains(err.Error(), "ledger 61000000")) {
			t.Errorf("%s: err = %v, want refusal %t", name, err, c.bad)
		}
	}
}

// A change that does not decode refuses the window with its position, never reads as zero.
func TestLendingInterestVersion_DecodeFailuresNameTheChange(t *testing.T) {
	noConfig := xdr.ScMap{{Key: testSym("bstop_rate"), Val: testU32(1)}}
	for name, c := range map[string]struct {
		key    LendingInterestKey
		change lendingInterestChange
		want   string
	}{
		"corrupt reserve":          {liKey(lendinginterest.KindReserve, lendinginterest.BlendV2), liChange("updated", histInstance(t)), "ResData"},
		"not contract data":        {liKey(lendinginterest.KindReserve, lendinginterest.BlendV2), liChange("updated", "AAAA"), "not contract data"},
		"instance without Config":  {liKey(lendinginterest.KindConfig, lendinginterest.BlendV2), liChange("updated", histEntry(t, poolInstanceVal(t, noConfig))), "no decodable pool Config"},
		"corrupt fees":             {liKey(lendinginterest.KindFees, lendinginterest.BLNT), liChange("updated", liFeesEntry(t, "1", "10000000")), "ProtocolFees"},
		"transaction-level change": {liKey(lendinginterest.KindReserve, lendinginterest.BlendV2), func() lendingInterestChange { c := liChange("updated", ""); c.OpIndex = -1; return c }(), "op_index -1"},
		"unknown change type":      {liKey(lendinginterest.KindReserve, lendinginterest.BlendV2), liChange("evicted", ""), "unknown change type"},
	} {
		_, err := lendingInterestVersion(c.key, c.change)
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "ledger 61000000 tx aa key K") {
			t.Errorf("%s: err = %v, want one naming the change and %q", name, err, c.want)
		}
	}
}

func TestLendingInterestSeeds_OneCandidatePerKeyAndEmptyBLNTFees(t *testing.T) {
	pools := []LendingContract{{ID: blendTestPool, Protocol: LendingBLNT}, {ID: blntTestPool, Protocol: LendingBLNT}}
	res := LendingInterestKey{Key: "R", Pool: blendTestPool, Protocol: lendinginterest.BLNT, Kind: lendinginterest.KindReserve, Asset: blendTestAssetSAC}
	newRes := LendingInterestKey{Key: "R2", Pool: blntTestPool, Protocol: lendinginterest.BLNT, Kind: lendinginterest.KindReserve, Asset: blendTestAssetSAC}
	inst := LendingInterestKey{Key: "I", Pool: blendTestPool, Protocol: lendinginterest.BLNT, Kind: lendinginterest.KindConfig}
	fees := LendingInterestKey{Key: "F", Pool: blendTestPool, Protocol: lendinginterest.BLNT, Kind: lendinginterest.KindFees}
	hour := time.Date(2027, 1, 1, 11, 0, 0, 0, time.UTC)
	s, err := lendingInterestSeeds(pools, []LendingInterestKey{res, newRes, inst, fees}, []lendingInterestSeedRow{
		{Key: "R", Ledger: 9, Entry: liResDataEntry(t, histDRate, histBSupply)},
		{Key: "I", Ledger: 8, Entry: histInstance(t)},
		{Key: "F", Ledger: 7, Entry: liFeesEntry(t, "3", "4")},
	}, hour)
	if err != nil {
		t.Fatal(err)
	}
	r := s.Reserves[lendinginterest.ReserveKey{Pool: blendTestPool, Asset: blendTestAssetSAC}]
	if len(s.Reserves) != 1 || len(r) != 1 || r[0].DRate.String() != histDRate {
		t.Fatalf("reserve seeds = %+v, want one candidate for the reserve with a row", s.Reserves)
	}
	if c := s.Configs[blendTestPool]; len(s.Configs) != 1 || len(c) != 1 || c[0] != 2_000_000 {
		t.Fatalf("config seeds = %+v", s.Configs)
	}
	if f := s.Fees[blendTestPool]; len(f) != 1 || f[0][blendTestAssetSAC].Credit.Cmp(big.NewInt(3)) != 0 {
		t.Fatalf("fee seeds for the pool with a row = %+v", f)
	}
	if f, ok := s.Fees[blntTestPool]; !ok || len(f) != 1 || f[0] == nil || len(f[0]) != 0 {
		t.Fatalf("fee seeds for the BLNT pool without a row = %#v, want one explicit empty map", f)
	}
	if !s.CommittedBefore.Equal(hour) {
		t.Fatalf("CommittedBefore = %s, want %s", s.CommittedBefore, hour)
	}

	v2 := []LendingContract{{ID: blendTestPool, Protocol: LendingBlendV2}}
	if s, err := lendingInterestSeeds(v2, nil, nil, hour); err != nil || len(s.Fees) != 0 {
		t.Fatalf("Blend pool fee seeds = %+v (%v), want none", s.Fees, err)
	}
	deleted, err := lendingInterestSeeds(pools, []LendingInterestKey{res, inst, fees}, []lendingInterestSeedRow{
		{Key: "F", Ledger: 7},
	}, hour)
	if f := deleted.Fees[blendTestPool]; err != nil || len(f) != 1 || f[0] == nil || len(f[0]) != 0 {
		t.Fatalf("seed after a ProtocolFees delete = %#v (%v), want one explicit empty map", f, err)
	}
	for name, row := range map[string]lendingInterestSeedRow{
		"deleted reserve":  {Key: "R", Ledger: 9},
		"deleted instance": {Key: "I", Ledger: 9},
		"undecodable":      {Key: "R", Ledger: 9, Entry: histInstance(t)},
		"unrequested":      {Key: "X", Ledger: 9, Entry: histInstance(t)},
		"corrupt config":   {Key: "I", Ledger: 9, Entry: liFeesEntry(t, "1", "1")},
	} {
		if _, err := lendingInterestSeeds(pools, []LendingInterestKey{res, inst}, []lendingInterestSeedRow{row}, hour); err == nil {
			t.Errorf("%s: seed accepted, want a refusal", name)
		}
	}
}

func TestCheckLendingInterestRow_RefusesAMissingAmount(t *testing.T) {
	zero := func() *big.Int { return new(big.Int) }
	r := lendinginterest.HourRow{
		Pool: blendTestPool, Asset: blendTestAssetSAC, Protocol: lendinginterest.BLNT,
		Gross: zero(), ProtocolFee: zero(), Backstop: zero(), SupplierIntended: zero(), SupplierRealised: zero(),
		BackstopCreditDelta: zero(), BackstopResidual: zero(), SupplyResidual: zero(), DefaultLoss: zero(), FeeCreditResidual: zero(),
		ResidualExplained: zero(), ResidualAttributed: zero(),
	}
	if err := checkLendingInterestRow(r); err != nil {
		t.Fatalf("complete row refused: %v", err)
	}
	for _, drop := range []func(*lendinginterest.HourRow){
		func(r *lendinginterest.HourRow) { r.SupplyResidual = nil },
		func(r *lendinginterest.HourRow) { r.DefaultLoss = nil },
		func(r *lendinginterest.HourRow) { r.ResidualAttributed = nil },
	} {
		c := r
		drop(&c)
		if err := checkLendingInterestRow(c); err == nil {
			t.Fatal("row with a nil amount accepted")
		}
	}
}
