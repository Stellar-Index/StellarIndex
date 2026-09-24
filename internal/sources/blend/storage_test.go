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
	rd, err := DecodeReserveData(sv)
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
