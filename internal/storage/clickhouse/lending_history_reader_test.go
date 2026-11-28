package clickhouse

import (
	"reflect"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
)

// histEntry wraps a value as a base64 pool contract_data entry, as lending_state_hourly stores it.
func histEntry(t *testing.T, val xdr.ScVal) string {
	t.Helper()
	pid := mustContractID(t, blendTestPool)
	keyVec := &xdr.ScVec{testSym("ResData")}
	b64, err := xdr.MarshalBase64(xdr.LedgerEntry{Data: xdr.LedgerEntryData{
		Type: xdr.LedgerEntryTypeContractData,
		ContractData: &xdr.ContractDataEntry{
			Contract:   xdr.ScAddress{Type: xdr.ScAddressTypeScAddressTypeContract, ContractId: &pid},
			Key:        xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &keyVec},
			Durability: xdr.ContractDataDurabilityPersistent,
			Val:        val,
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	return b64
}

func histMap(entries ...xdr.ScMapEntry) xdr.ScVal {
	m := xdr.ScMap(entries)
	mp := &m
	return xdr.ScVal{Type: xdr.ScValTypeScvMap, Map: &mp}
}

// histResData is a ResData entry; above 2^63 the supplies prove no int64 narrowing.
func histResData(t *testing.T, bRate, dRate, bSupply, dSupply string) string {
	t.Helper()
	u64 := xdr.Uint64(1_700_000_000)
	return histEntry(t, histMap(
		xdr.ScMapEntry{Key: testSym("b_rate"), Val: i128Val(t, bRate)},
		xdr.ScMapEntry{Key: testSym("b_supply"), Val: i128Val(t, bSupply)},
		xdr.ScMapEntry{Key: testSym("backstop_credit"), Val: i128Val(t, "12345")},
		xdr.ScMapEntry{Key: testSym("d_rate"), Val: i128Val(t, dRate)},
		xdr.ScMapEntry{Key: testSym("d_supply"), Val: i128Val(t, dSupply)},
		xdr.ScMapEntry{Key: testSym("ir_mod"), Val: i128Val(t, "10000000")},
		xdr.ScMapEntry{Key: testSym("last_time"), Val: xdr.ScVal{Type: xdr.ScValTypeScvU64, U64: &u64}},
	))
}

// histResConfig is a V2 ResConfig entry with collateral factor cFactor.
func histResConfig(t *testing.T, cFactor uint32) string {
	t.Helper()
	enabled := true
	fields := []xdr.ScMapEntry{
		{Key: testSym("c_factor"), Val: testU32(cFactor)},
		{Key: testSym("decimals"), Val: testU32(7)},
		{Key: testSym("enabled"), Val: xdr.ScVal{Type: xdr.ScValTypeScvBool, B: &enabled}},
		{Key: testSym("index"), Val: testU32(0)},
		{Key: testSym("l_factor"), Val: testU32(9_500_000)},
		{Key: testSym("max_util"), Val: testU32(9_500_000)},
		{Key: testSym("r_base"), Val: testU32(100_000)},
		{Key: testSym("r_one"), Val: testU32(500_000)},
		{Key: testSym("r_three"), Val: testU32(50_000_000)},
		{Key: testSym("r_two"), Val: testU32(5_000_000)},
		{Key: testSym("reactivity"), Val: testU32(20)},
		{Key: testSym("supply_cap"), Val: i128Val(t, "100000000000000000000")},
		{Key: testSym("util"), Val: testU32(8_000_000)},
	}
	return histEntry(t, histMap(fields...))
}

func histInstance(t *testing.T) string {
	t.Helper()
	return histEntry(t, poolInstanceVal(t, v2PoolConfigMap(t)))
}

// histRow is a row closing in its own hour (ledger N closes at hour N), so only rows of one ledger share an hour.
func histRow(protocol string, ledger, intra uint32, entry string) stateRow {
	return stateRow{protocol: protocol, ledger: ledger, intra: intra, close: time.Unix(int64(ledger)*3600, 0).UTC(), entry: entry}
}

func histRowAt(protocol string, ledger uint32, at time.Time, entry string) stateRow {
	r := histRow(protocol, ledger, 1, entry)
	r.close = at
	return r
}

const (
	histBRate   = "1000000000000"
	histDRate   = "1100000000000"
	histBSupply = "20000000000000000000" // > 2^64
	histDSupply = "15000000000000000000"
)

func TestAssembleReserveHistory_ConfigInForcePairing(t *testing.T) {
	data := histResData(t, histBRate, histDRate, histBSupply, histDSupply)
	cfg := []stateRow{histRow(LendingBlendV2, 100, 3, histResConfig(t, 9_000_000)), histRow(LendingBlendV2, 200, 4, histResConfig(t, 8_000_000))}
	inst := []stateRow{histRow(LendingBlendV2, 50, 1, histInstance(t))}
	rows := []stateRow{
		histRow(LendingBlendV2, 40, 1, data),  // before any config
		histRow(LendingBlendV2, 150, 1, data), // old config
		histRow(LendingBlendV2, 200, 3, data), // same hour, earlier transaction: the config in force is unknown
		histRow(LendingBlendV2, 200, 4, data), // the transaction that wrote the new config
		histRow(LendingBlendV2, 250, 1, data), // new config
	}
	h := assembleReserveHistory(blendTestPool, blendTestAssetSAC, rows, cfg, inst)
	if h.Undecodable != 0 || len(h.Points) != len(rows) || h.Protocol != LendingBlendV2 {
		t.Fatalf("history = %+v", h)
	}
	want := []struct {
		cfgLedger uint32
		cFactor   uint32
		uncertain bool
	}{{0, 0, false}, {100, 9_000_000, false}, {0, 0, true}, {200, 8_000_000, false}, {200, 8_000_000, false}}
	for i, w := range want {
		p := h.Points[i]
		if p.ConfigLedger != w.cfgLedger || p.ConfigUncertain != w.uncertain {
			t.Fatalf("point %d config ledger = %d uncertain %v, want %d %v", i, p.ConfigLedger, p.ConfigUncertain, w.cfgLedger, w.uncertain)
		}
		if w.cfgLedger == 0 {
			if p.State.Config != nil || p.State.Metrics.HasAPR {
				t.Fatalf("point %d has a config or APR with none in force: %+v", i, p.State)
			}
			continue
		}
		if p.State.Config == nil || p.State.Config.CFactor != w.cFactor || !p.State.Metrics.HasAPR {
			t.Fatalf("point %d config = %+v", i, p.State.Config)
		}
	}
}

// TestAssembleReserveHistory_ConfigChangedLaterInTheSameHourIsUncertain: only an hour's last ResConfig and instance rows are
// kept, so a change later in a state row's hour leaves the version in force at that row unknown.
func TestAssembleReserveHistory_ConfigChangedLaterInTheSameHourIsUncertain(t *testing.T) {
	data, instEntry := histResData(t, histBRate, histDRate, histBSupply, histDSupply), histInstance(t)
	h10 := time.Date(2027, 3, 1, 10, 0, 0, 0, time.UTC)
	cfg := []stateRow{
		histRowAt(LendingBlendV2, 100, h10.Add(-time.Hour), histResConfig(t, 9_000_000)),
		histRowAt(LendingBlendV2, 300, h10.Add(50*time.Minute), histResConfig(t, 8_000_000)),
	}
	inst := []stateRow{
		histRowAt(LendingBlendV2, 50, h10.Add(-2*time.Hour), instEntry),
		histRowAt(LendingBlendV2, 310, h10.Add(55*time.Minute), instEntry),
	}
	rows := []stateRow{
		histRowAt(LendingBlendV2, 150, h10.Add(-30*time.Minute), data), // config from an earlier hour: in force
		histRowAt(LendingBlendV2, 200, h10.Add(10*time.Minute), data),  // config changed later in this hour: unknown
		histRowAt(LendingBlendV2, 300, h10.Add(50*time.Minute), data),  // same transaction as the config; instance later this hour
		histRowAt(LendingBlendV2, 400, h10.Add(70*time.Minute), data),  // next hour: both in force
	}
	h := assembleReserveHistory(blendTestPool, blendTestAssetSAC, rows, cfg, inst)
	if h.Undecodable != 0 || len(h.Points) != 4 {
		t.Fatalf("history = %+v", h)
	}
	want := []struct {
		cfgLedger               uint32
		cfgUnknown, poolUnknown bool
	}{{100, false, false}, {0, true, false}, {300, false, true}, {300, false, false}}
	for i, w := range want {
		p := h.Points[i]
		if p.ConfigLedger != w.cfgLedger || p.ConfigUncertain != w.cfgUnknown || p.PoolConfigUncertain != w.poolUnknown {
			t.Fatalf("point %d = ledger %d uncertain %v/%v, want %+v", i, p.ConfigLedger, p.ConfigUncertain, p.PoolConfigUncertain, w)
		}
		if p.State.Metrics.SuppliedUnderlying == nil || (p.State.Config == nil) != w.cfgUnknown || p.State.Metrics.HasAPR == w.cfgUnknown {
			t.Fatalf("point %d state = %+v", i, p.State)
		}
	}
}

func TestAssembleReserveHistory_RemovedRows(t *testing.T) {
	data := histResData(t, histBRate, histDRate, histBSupply, histDSupply)
	cfg := []stateRow{histRow(LendingBlendV2, 10, 1, histResConfig(t, 9_000_000)), histRow(LendingBlendV2, 30, 1, "")}
	inst := []stateRow{histRow(LendingBlendV2, 5, 1, histInstance(t))}
	rows := []stateRow{histRow(LendingBlendV2, 20, 1, data), histRow(LendingBlendV2, 40, 1, data), histRow(LendingBlendV2, 50, 1, "")}
	h := assembleReserveHistory(blendTestPool, blendTestAssetSAC, rows, cfg, inst)
	if len(h.Points) != 3 || h.Undecodable != 0 {
		t.Fatalf("history = %+v", h)
	}
	if !h.Points[0].State.Metrics.HasAPR || h.Points[1].ConfigLedger != 0 || h.Points[1].State.Metrics.HasAPR {
		t.Fatalf("a removed config must take the config out of force: %+v", h.Points[:2])
	}
	if p := h.Points[2]; !p.Removed || p.StateLedger != 50 || p.State.Metrics.SuppliedUnderlying != nil {
		t.Fatalf("removed state point = %+v", p)
	}
}

func TestAssembleReserveHistory_V1AndV2RateScales(t *testing.T) {
	data := histResData(t, histBRate, histDRate, histBSupply, histDSupply)
	supplied := func(protocol string) string {
		h := assembleReserveHistory(blendTestPool, blendTestAssetSAC, []stateRow{histRow(protocol, 10, 1, data)}, nil, nil)
		if len(h.Points) != 1 {
			t.Fatalf("%s: %+v", protocol, h)
		}
		return h.Points[0].State.Metrics.SuppliedUnderlying.String()
	}
	if v2 := supplied(LendingBlendV2); v2 != "20000000000000000000" {
		t.Fatalf("V2 supplied = %s (b_rate 1e12 is 1.0 at 12 decimals)", v2)
	}
	if v1 := supplied(LendingBlendV1); v1 != "20000000000000000000000" {
		t.Fatalf("V1 supplied = %s (b_rate 1e12 is 1000.0 at 9 decimals)", v1)
	}
}

func TestAssembleReserveHistory_BLNTSupplyRate(t *testing.T) {
	data := histResData(t, histBRate, histDRate, histBSupply, histDSupply)
	cfg := []stateRow{histRow(LendingBLNT, 1, 1, histResConfig(t, 9_000_000))}
	inst := []stateRow{histRow(LendingBLNT, 1, 1, histInstance(t))}
	got := assembleReserveHistory(blendTestPool, blendTestAssetSAC, []stateRow{histRow(LendingBLNT, 10, 1, data)}, cfg, inst).Points[0].State.Metrics
	rd, err := blnt.DecodeReserveData(mustValue(t, data))
	if err != nil {
		t.Fatal(err)
	}
	rc, err := blnt.DecodeReserveConfig(mustValue(t, cfg[0].entry))
	if err != nil {
		t.Fatal(err)
	}
	want := blnt.Metrics(rd, rc, 2_000_000)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("BLNT metrics = %+v, want %+v", got, want)
	}
	if blendV2 := blend.Metrics(rd, rc, 2_000_000); blendV2.SupplyAPR == got.SupplyAPR {
		t.Fatalf("BLNT supply APR %v must net the protocol fee off Blend's %v", got.SupplyAPR, blendV2.SupplyAPR)
	}
}

func TestLendingHistoryQuery_AlignedRoundsFromDownToItsBucket(t *testing.T) {
	from := time.Date(2027, 3, 1, 10, 30, 15, 0, time.UTC)
	if got := (LendingHistoryQuery{From: from}).Aligned().From; !got.Equal(time.Date(2027, 3, 1, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("hourly from = %v", got)
	}
	if got := (LendingHistoryQuery{From: from, Daily: true}).Aligned().From; !got.Equal(time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("daily from = %v", got)
	}
}

func mustValue(t *testing.T, b64 string) xdr.ScVal {
	t.Helper()
	v, ok := contractDataValue(b64)
	if !ok {
		t.Fatalf("not a contract data entry: %q", b64)
	}
	return v
}

func TestAssembleReserveHistory_DecodeFailureIsDroppedAndCounted(t *testing.T) {
	data := histResData(t, histBRate, histDRate, histBSupply, histDSupply)
	badCfg := []stateRow{histRow(LendingBlendV2, 100, 1, histEntry(t, testU32(1)))}
	rows := []stateRow{
		histRow(LendingBlendV2, 10, 1, "not-xdr"),
		histRow("unknown", 20, 1, data),
		histRow(LendingBlendV2, 30, 1, histEntry(t, testU32(1))),
		histRow(LendingBlendV2, 40, 1, data),
		histRow(LendingBlendV2, 110, 1, data), // its config in force is undecodable
	}
	h := assembleReserveHistory(blendTestPool, blendTestAssetSAC, rows, badCfg, nil)
	if h.Undecodable != 4 || len(h.Points) != 1 || h.Points[0].StateLedger != 40 {
		t.Fatalf("history = %+v", h)
	}
}

// TestReserveHistory_SameModelAsLiveOnSameInputs decodes the same entries through the live endpoint's read path
// (scanReserveParts, assembleReserveStates) and the history, and requires identical state and metrics.
func TestReserveHistory_SameModelAsLiveOnSameInputs(t *testing.T) {
	for _, protocol := range []string{LendingBlendV1, LendingBlendV2, LendingBLNT} {
		data := histResData(t, histBRate, histDRate, histBSupply, histDSupply)
		cfgEntry, instEntry := histResConfig(t, 9_000_000), histInstance(t)
		model, _ := historyModel(protocol)

		const dataKey, instKey = "k-data", "k-inst"
		refByKey := map[string]keyRef{dataKey: {asset: blendTestAssetSAC, kind: "ResData"}, instKey: {kind: "Instance"}}
		rows := &stubRows{data: [][]any{{dataKey, data}, {instKey, instEntry}}}
		parts, err := scanReserveParts(rows, refByKey, model.decode)
		if err != nil {
			t.Fatal(err)
		}
		dataByAsset, inst := parts.data, parts.inst
		rc, err := blend.DecodeReserveConfig(mustValue(t, cfgEntry))
		if err != nil {
			t.Fatal(err)
		}
		live := assembleReserveStates(blendTestPool, []string{blendTestAssetSAC}, dataByAsset,
			map[string]blend.ReserveConfig{blendTestAssetSAC: rc}, inst, model.metrics)

		h := assembleReserveHistory(blendTestPool, blendTestAssetSAC,
			[]stateRow{histRow(protocol, 90, 1, histResData(t, "1", "1", "1", "0")), histRow(protocol, 100, 1, data)},
			[]stateRow{histRow(protocol, 80, 1, cfgEntry)}, []stateRow{histRow(protocol, 70, 1, instEntry)})
		last := h.Points[len(h.Points)-1].State
		if len(live) != 1 || !reflect.DeepEqual(last, live[0]) {
			t.Fatalf("%s: history %+v != live %+v", protocol, last, live)
		}
	}
}

func TestAssembleBackstopHistory_TiersSeedRemovedAndUndecodable(t *testing.T) {
	backstop := mustContractID(t, blendTestAssetSAC)
	pool := mustContractID(t, blendTestPool)
	bal := func(shares, tokens, q4w string) string {
		return poolBalanceEntry(t, backstop, contractAddressVal(pool), shares, tokens, q4w)
	}
	tierKey := map[string]string{"FirstLoss": "k1", "SecondLoss": "k2", "ThirdLoss": "k3"}
	row := func(key string, ledger uint32, entry string) stateRow {
		r := histRow(LendingBLNT, ledger, 1, entry)
		r.key = key
		return r
	}
	seed := []stateRow{row("k2", 5, bal("100", "200", "10"))}
	rng := []stateRow{
		row("k1", 10, bal("18446744073709551616", "9223372036854775819", "0")),
		row("k1", 20, ""),
		row("k2", 15, "garbage"),
		row("k2", 25, bal("110", "220", "11")),
	}
	tiers, undecodable := assembleBackstopHistory(blntBackstopTiers, tierKey, seed, rng)
	if undecodable != 1 || len(tiers) != 3 {
		t.Fatalf("tiers = %+v, undecodable = %d", tiers, undecodable)
	}
	first, second, third := tiers[0], tiers[1], tiers[2]
	if !first.BeforeFirstState || len(first.Points) != 2 || first.Points[0].Balance.Shares.String() != "18446744073709551616" || !first.Points[1].Removed {
		t.Fatalf("FirstLoss = %+v", first)
	}
	if second.BeforeFirstState || len(second.Points) != 2 || second.Points[0].Ledger != 5 || second.Points[1].Balance.Tokens.String() != "220" {
		t.Fatalf("SecondLoss = %+v", second)
	}
	if !third.BeforeFirstState || len(third.Points) != 0 {
		t.Fatalf("ThirdLoss = %+v", third)
	}
}
