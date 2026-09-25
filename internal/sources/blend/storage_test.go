package blend

import (
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"
)

func TestDecodeReserveData(t *testing.T) {
	// Alphabetical-by-symbol order mirrors soroban-sdk's encode path
	// (the decoder is order-independent — it reads by name).
	sv := mapScVal([]xdr.ScMapEntry{
		{Key: symbolScVal("b_rate"), Val: i128ScVal(t, big.NewInt(1_123_456_789_000))},
		{Key: symbolScVal("b_supply"), Val: i128ScVal(t, big.NewInt(5_000_000_0000000))},
		{Key: symbolScVal("backstop_credit"), Val: i128ScVal(t, big.NewInt(517357))},
		{Key: symbolScVal("d_rate"), Val: i128ScVal(t, big.NewInt(1_345_678_123_000))},
		{Key: symbolScVal("d_supply"), Val: i128ScVal(t, big.NewInt(2_000_000_0000000))},
		{Key: symbolScVal("ir_mod"), Val: i128ScVal(t, big.NewInt(1_0000000))},
		{Key: symbolScVal("last_time"), Val: u64ScVal(1_700_000_000)},
	})
	rd, err := DecodeReserveData(sv, PoolV2)
	if err != nil {
		t.Fatalf("DecodeReserveData: %v", err)
	}
	if rd.BRate.Cmp(big.NewInt(1_123_456_789_000)) != 0 {
		t.Errorf("b_rate = %s", rd.BRate)
	}
	if rd.DSupply.Cmp(big.NewInt(2_000_000_0000000)) != 0 {
		t.Errorf("d_supply = %s", rd.DSupply)
	}
	if rd.BackstopCredit.Cmp(big.NewInt(517357)) != 0 {
		t.Errorf("backstop_credit = %s", rd.BackstopCredit)
	}
	if rd.LastTime != 1_700_000_000 {
		t.Errorf("last_time = %d", rd.LastTime)
	}
}

// v1ReserveDataScVal is a V1 pool's ResData: same field names as V2,
// but b_rate / d_rate / ir_mod carry 9 decimals (blend-contracts v1
// storage.rs / interest.rs). b_rate 1.05, d_rate 1.1, ir_mod 1.0.
func v1ReserveDataScVal(t *testing.T) xdr.ScVal {
	t.Helper()
	return mapScVal([]xdr.ScMapEntry{
		{Key: symbolScVal("b_rate"), Val: i128ScVal(t, big.NewInt(1_050_000_000))},
		{Key: symbolScVal("b_supply"), Val: i128ScVal(t, big.NewInt(10_000_000_000_000))},
		{Key: symbolScVal("backstop_credit"), Val: i128ScVal(t, big.NewInt(0))},
		{Key: symbolScVal("d_rate"), Val: i128ScVal(t, big.NewInt(1_100_000_000))},
		{Key: symbolScVal("d_supply"), Val: i128ScVal(t, big.NewInt(2_000_000_000_000))},
		{Key: symbolScVal("ir_mod"), Val: i128ScVal(t, big.NewInt(1_000_000_000))},
		{Key: symbolScVal("last_time"), Val: u64ScVal(1_700_000_000)},
	})
}

// A V1 reserve's totals are b_supply × b_rate / 1e9 (v1 reserve.rs
// total_supply = fixed_mul_floor(b_rate, SCALAR_9)); dividing by the V2
// 1e12 serves supplied / borrowed / TVL 1000x low.
func TestReserveData_V1RatesAreNineDecimals(t *testing.T) {
	rd, err := DecodeReserveData(v1ReserveDataScVal(t), PoolV1)
	if err != nil {
		t.Fatalf("DecodeReserveData(V1): %v", err)
	}
	if got, want := rd.SuppliedUnderlying(), big.NewInt(10_500_000_000_000); got.Cmp(want) != 0 {
		t.Errorf("V1 SuppliedUnderlying = %s, want %s (1e13 × 1.05)", got, want)
	}
	if got, want := rd.BorrowedUnderlying(), big.NewInt(2_200_000_000_000); got.Cmp(want) != 0 {
		t.Errorf("V1 BorrowedUnderlying = %s, want %s (2e12 × 1.1)", got, want)
	}
	// The same entry read as a V2 pool is 1000x smaller: the
	// generation, not the wire shape, decides the scale.
	v2, err := DecodeReserveData(v1ReserveDataScVal(t), PoolV2)
	if err != nil {
		t.Fatalf("DecodeReserveData(V2): %v", err)
	}
	if got, want := v2.SuppliedUnderlying(), big.NewInt(10_500_000_000); got.Cmp(want) != 0 {
		t.Errorf("V2 SuppliedUnderlying = %s, want %s", got, want)
	}
}

// A V1 reserve never reports an APR: BorrowRate is the V2 model with a
// 7-decimal ir_mod, which would read V1's 9-decimal ir_mod ~100x high.
func TestMetrics_V1WithholdsAPR(t *testing.T) {
	rd, err := DecodeReserveData(v1ReserveDataScVal(t), PoolV1)
	if err != nil {
		t.Fatalf("DecodeReserveData(V1): %v", err)
	}
	m := Metrics(rd, interestRefConfig(), 1_000_000)
	if m.HasAPR {
		t.Errorf("V1 reserve served APR borrow=%v supply=%v; want withheld", m.BorrowAPR, m.SupplyAPR)
	}
	if m.SuppliedUnderlying.Cmp(big.NewInt(10_500_000_000_000)) != 0 {
		t.Errorf("V1 Metrics supplied = %s, want 10500000000000", m.SuppliedUnderlying)
	}
}

// Without a known generation the scale is a guess, so decode refuses.
func TestDecodeReserveData_UnknownVersionRefused(t *testing.T) {
	if _, err := DecodeReserveData(v1ReserveDataScVal(t), PoolVersionUnknown); err == nil {
		t.Fatal("DecodeReserveData accepted an unknown pool version")
	}
}

func TestPoolVersionForFactory(t *testing.T) {
	for _, c := range []struct {
		factory string
		want    PoolVersion
	}{
		{MainnetPoolFactoryV1, PoolV1},
		{MainnetPoolFactory, PoolV2},
		{"", PoolVersionUnknown},
		{"CURATED", PoolVersionUnknown},
	} {
		if got := PoolVersionForFactory(c.factory); got != c.want {
			t.Errorf("PoolVersionForFactory(%q) = %d, want %d", c.factory, got, c.want)
		}
	}
}

func TestDecodeReserveConfig(t *testing.T) {
	rc, err := DecodeReserveConfig(reserveConfigScVal(t))
	if err != nil {
		t.Fatalf("DecodeReserveConfig: %v", err)
	}
	// Values from the existing reserveConfigScVal helper.
	if rc.Util != 8_000_000 || rc.RBase != 100_000 || rc.ROne != 500_000 ||
		rc.RTwo != 1_000_000 || rc.RThree != 2_000_000 || rc.Reactivity != 50_000 {
		t.Errorf("config rate params = %+v", rc)
	}
	if rc.Decimals != 7 || !rc.Enabled || rc.Index != 3 {
		t.Errorf("config meta = %+v", rc)
	}
}

// A V1 pool's ResConfig entry has no supply_cap / enabled: the reserve
// is always enabled and uncapped.
func TestDecodeReserveConfig_V1Pool(t *testing.T) {
	rc, err := DecodeReserveConfig(reserveConfigV1ScVal(t))
	if err != nil {
		t.Fatalf("DecodeReserveConfig(V1): %v", err)
	}
	if rc.Util != 8_000_000 || rc.Reactivity != 50_000 || rc.Index != 3 {
		t.Errorf("config = %+v", rc)
	}
	if !rc.Enabled || rc.SupplyCap != nil {
		t.Errorf("V1 enabled/supply_cap = %v/%v, want true/nil", rc.Enabled, rc.SupplyCap)
	}
	if _, err := DecodeReserveConfig(reserveConfigV1ScVal(t, "util")); err == nil {
		t.Error("missing util: want error")
	}
}

func TestParseReserveConfigMetadata_V1Pool(t *testing.T) {
	cfg, err := ParseReserveConfigMetadata([]byte(`{"index":1,"decimals":7,"c_factor":9000000,` +
		`"l_factor":9500000,"util":8000000,"max_util":9500000,"r_base":50000,` +
		`"r_one":500000,"r_two":5000000,"r_three":15000000,"reactivity":200}`))
	if err != nil {
		t.Fatalf("ParseReserveConfigMetadata: %v", err)
	}
	if cfg.Util != 8_000_000 || cfg.Reactivity != 200 {
		t.Errorf("config = %+v", cfg)
	}
	if !cfg.Enabled || cfg.SupplyCap != nil {
		t.Errorf("V1 enabled/supply_cap = %v/%v, want true/nil", cfg.Enabled, cfg.SupplyCap)
	}

	cfg, err = ParseReserveConfigMetadata([]byte(`{"util":1,"supply_cap":"5","enabled":false}`))
	if err != nil {
		t.Fatalf("ParseReserveConfigMetadata(V2): %v", err)
	}
	if cfg.Enabled || cfg.SupplyCap == nil || cfg.SupplyCap.Int64() != 5 {
		t.Errorf("V2 enabled/supply_cap = %v/%v, want false/5", cfg.Enabled, cfg.SupplyCap)
	}
}

func TestDecodePoolConfig(t *testing.T) {
	const oracle = "CCYHURAC5VTN2ZU663UUS5F24S4GURDPO4FHZ75JLN5DMLRTLCG44H44"
	sv := mapScVal([]xdr.ScMapEntry{
		{Key: symbolScVal("bstop_rate"), Val: u32ScVal(1_000_000)}, // 0.10
		{Key: symbolScVal("max_positions"), Val: u32ScVal(4)},
		{Key: symbolScVal("min_collateral"), Val: i128ScVal(t, big.NewInt(0))},
		{Key: symbolScVal("oracle"), Val: addressScVal(t, oracle)},
		{Key: symbolScVal("status"), Val: u32ScVal(0)},
	})
	pc, err := DecodePoolConfig(sv)
	if err != nil {
		t.Fatalf("DecodePoolConfig: %v", err)
	}
	if pc.BstopRate != 1_000_000 || pc.Oracle != oracle || pc.MaxPositions != 4 {
		t.Errorf("pool config = %+v", pc)
	}
}
