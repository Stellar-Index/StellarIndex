package blnt

import (
	"encoding/json"
	"math/big"
	"os"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
)

type storageFixture struct {
	Pool     string `json:"pool"`
	Kind     string `json:"kind"`
	Asset    string `json:"asset"`
	EntryXDR string `json:"entry_xdr"`
}

func loadStorageFixtures(t *testing.T) []storageFixture {
	t.Helper()
	b, err := os.ReadFile("../../../test/fixtures/blnt_storage/reserve_entries.json")
	if err != nil {
		t.Fatal(err)
	}
	var out []storageFixture
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func contractDataVal(t *testing.T, b64 string) xdr.ScVal {
	t.Helper()
	var e xdr.LedgerEntry
	if err := xdr.SafeUnmarshalBase64(b64, &e); err != nil {
		t.Fatal(err)
	}
	cd, ok := e.Data.GetContractData()
	if !ok {
		t.Fatal("not a contract_data entry")
	}
	return cd.Val
}

func instanceBstopRate(t *testing.T, v xdr.ScVal) uint32 {
	t.Helper()
	inst, ok := v.GetInstance()
	if !ok || inst.Storage == nil {
		t.Fatal("instance entry has no storage")
	}
	for _, e := range *inst.Storage {
		if e.Key.Sym != nil && string(*e.Key.Sym) == "Config" {
			pc, err := DecodePoolConfig(e.Val)
			if err != nil {
				t.Fatalf("DecodePoolConfig: %v", err)
			}
			return pc.BstopRate
		}
	}
	t.Fatal("instance storage has no Config")
	return 0
}

type decodedPool struct {
	bstop   uint32
	data    map[string]ReserveData
	configs map[string]ReserveConfig
}

func decodeStorageFixtures(t *testing.T) map[string]*decodedPool {
	t.Helper()
	pools := map[string]*decodedPool{}
	for _, f := range loadStorageFixtures(t) {
		p := pools[f.Pool]
		if p == nil {
			p = &decodedPool{data: map[string]ReserveData{}, configs: map[string]ReserveConfig{}}
			pools[f.Pool] = p
		}
		v := contractDataVal(t, f.EntryXDR)
		switch f.Kind {
		case "Instance":
			p.bstop = instanceBstopRate(t, v)
		case "ResData":
			rd, err := DecodeReserveData(v)
			if err != nil {
				t.Fatalf("%s %s ResData: %v", f.Pool, f.Asset, err)
			}
			p.data[f.Asset] = rd
		case "ResConfig":
			rc, err := DecodeReserveConfig(v)
			if err != nil {
				t.Fatalf("%s %s ResConfig: %v", f.Pool, f.Asset, err)
			}
			p.configs[f.Asset] = rc
		default:
			t.Fatalf("unknown fixture kind %q", f.Kind)
		}
	}
	return pools
}

// TestReserves_RealFixturesDecode decodes all 12 real storage entries of both mainnet pools and pins one
// reserve end to end: supplied/borrowed from ResData, the curve from ResConfig, bstop_rate from the instance.
func TestReserves_RealFixturesDecode(t *testing.T) {
	pools := decodeStorageFixtures(t)
	if len(pools) != 2 {
		t.Fatalf("decoded %d pools, want 2", len(pools))
	}
	for pool, p := range pools {
		if p.bstop != 2_000_000 || len(p.data) != 3 || len(p.configs) != 2 {
			t.Errorf("%s: bstop=%d data=%d configs=%d, want 2000000/3/2", pool, p.bstop, len(p.data), len(p.configs))
		}
	}
	const usdc = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	p := pools[MainnetFixedPool]
	rd, rc := p.data[usdc], p.configs[usdc]
	if rc.Decimals != 7 || rc.Util != 8_000_000 || rc.RBase != 300_000 || rc.ROne != 400_000 || rc.RTwo != 1_200_000 || !rc.Enabled {
		t.Errorf("ResConfig = %+v", rc)
	}
	m := Metrics(rd, rc, p.bstop)
	util := rd.Utilization()
	borrow := rc.BorrowRate(util, rd.IRMod)
	for _, c := range []struct{ name, got, want string }{
		{"supplied", m.SuppliedUnderlying.String(), "1985903398374"},
		{"borrowed", m.BorrowedUnderlying.String(), "1599586439842"},
		{"util", util.String(), "8054705"},
		{"borrow rate", borrow.String(), "384507"},
		// gross floor(384507*0.8054705)=309709; fee floor(309709*0.002)=619; backstop floor(309090*0.2)=61818.
		{"supply rate", SupplyRate(borrow, util, p.bstop).String(), "247272"},
		{"blend supply rate", blend.SupplyRate(borrow, util, p.bstop).String(), "247767"},
	} {
		if c.got != c.want {
			t.Errorf("%s = %s, want %s", c.name, c.got, c.want)
		}
	}
	if !m.HasAPR || m.BorrowAPR != 0.0384507 || m.SupplyAPR != 0.0247272 {
		t.Errorf("metrics APRs = %+v, want borrow 0.0384507 supply 0.0247272", m)
	}
}

// TestSupplyRate_ProtocolFeeBeforeBackstop pins the contract's accrual order (reserve.rs accrue): fee =
// floor(gross*20_000/1e7), then backstop = floor(remaining*bstop/1e7), suppliers keep the rest.
func TestSupplyRate_ProtocolFeeBeforeBackstop(t *testing.T) {
	full := big.NewInt(10_000_000)
	for _, c := range []struct {
		name          string
		borrow, util  *big.Int
		bstop         uint32
		want, blendSR int64
	}{
		// fee 2000, remaining 998000, backstop 199600.
		{"round", big.NewInt(1_000_000), full, 2_000_000, 798_400, 800_000},
		// fee floor(1075.422)=1075, remaining 536636, backstop floor(53663.6)=53663 -> 482973. One
		// multiply by (1-bstop) after the fee would floor to 482972: the order and rounding are the contract's.
		{"rounding", big.NewInt(537_711), full, 1_000_000, 482_973, 483_939},
		{"no backstop", big.NewInt(1_000_000), full, 0, 998_000, 1_000_000},
		{"idle", big.NewInt(100_000), big.NewInt(0), 2_000_000, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := SupplyRate(c.borrow, c.util, c.bstop); got.Int64() != c.want {
				t.Errorf("SupplyRate = %s, want %d", got, c.want)
			}
			if got := blend.SupplyRate(c.borrow, c.util, c.bstop); got.Int64() != c.blendSR {
				t.Errorf("blend.SupplyRate = %s, want %d", got, c.blendSR)
			}
		})
	}
}
