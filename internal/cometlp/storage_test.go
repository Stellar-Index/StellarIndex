package cometlp_test

import (
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"slices"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/cometlp"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
	"github.com/Stellar-Index/StellarIndex/internal/wasmaudit"
)

const (
	usdcSAC = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	xlmSAC  = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	blndSAC = "CD25MNVTZDL4Y3XBCPCJXGXATV5WUHHOWMYFF4YBEGU5FCPGMYTVG5JY"
	blntSAC = "CCBNWP5L7ZSXA3DBWCMFAVKFVRW3ZHWO7B5J557W42O64DDZNPJP7VDS"

	blendBackstopV2 = "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7"
	blntBackstop    = "CAHCUKBPTW5DR5RNXOHCTJD5U76A6EEZXYZAQFRQEEQRXQ3CRUZ6Z3MO"
)

type fixtureEntry struct {
	Pool     string `json:"pool"`
	Kind     string `json:"kind"`
	Holder   string `json:"holder"`
	Ledger   uint32 `json:"ledger_seq"`
	KeyXDR   string `json:"key_xdr"`
	EntryXDR string `json:"entry_xdr"`
}

func fixtureEntries(t *testing.T) []fixtureEntry {
	t.Helper()
	raw, err := os.ReadFile("../../test/fixtures/comet_storage/pool_entries.json")
	if err != nil {
		t.Fatal(err)
	}
	var out []fixtureEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// fixtureVal is the freshly decoded value of pool's kind entry (holder for a Balance), so a test may mutate it.
func fixtureVal(t *testing.T, pool, kind, holder string) xdr.ScVal {
	t.Helper()
	for _, e := range fixtureEntries(t) {
		if e.Pool != pool || e.Kind != kind || e.Holder != holder {
			continue
		}
		var le xdr.LedgerEntry
		if err := xdr.SafeUnmarshalBase64(e.EntryXDR, &le); err != nil {
			t.Fatal(err)
		}
		return le.Data.MustContractData().Val
	}
	t.Fatalf("no fixture %s %s %s", pool, kind, holder)
	return xdr.ScVal{}
}

func fixtureState(t *testing.T, pool string) cometlp.PoolState {
	t.Helper()
	s, err := cometlp.NewPoolState(pool, fixtureVal(t, pool, "AllRecordData", ""), fixtureVal(t, pool, "TotalShares", ""), fixtureVal(t, pool, "Instance", ""))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func fixtureHolding(t *testing.T, pool, holder string) *big.Int {
	t.Helper()
	b, err := cometlp.DecodeTotalShares(fixtureVal(t, pool, "Balance", holder)) // an LP balance is the same bare i128
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The real entries of the three pools decode to the values verified on chain at the lake tip.
func TestNewPoolState_RealEntries(t *testing.T) {
	for _, tc := range []struct {
		pool, wasm, controller, shares string
		tokens, balances               [2]string
	}{
		{
			comet.MainnetBackstopPool, cometlp.WASMV100, "GABQT4DOTOJDRMQ6UIEH6I3JVYB4E33KZ7DHL4LGTI7PPJ36BZEQQ246",
			"1862083261311633634135993981",
			[2]string{blndSAC, usdcSAC},
			[2]string{"4274375264", "4351707"},
		},
		{
			comet.MainnetBLNTUSDCPool, cometlp.WASMV110, "GAFN3MT4JMFDMMXFCIO54Y532VJQK43YQC6FCVFWZNGH26AXYQK76QR4",
			"3796905613114",
			[2]string{blntSAC, usdcSAC},
			[2]string{"39514394520268", "38919029596"},
		},
		{
			comet.MainnetBLNTXLMPool, cometlp.WASMV110, "GAFN3MT4JMFDMMXFCIO54Y532VJQK43YQC6FCVFWZNGH26AXYQK76QR4",
			"6753401388209",
			[2]string{blntSAC, xlmSAC},
			[2]string{"66148438226880", "525560728083"},
		},
	} {
		s := fixtureState(t, tc.pool)
		inst := s.Instance
		if inst.WASM != tc.wasm || inst.Controller != tc.controller || inst.SwapFee.Int64() != 30_000 || inst.Freeze {
			t.Errorf("%s instance = %+v", tc.pool, inst)
		}
		if s.TotalShares.String() != tc.shares {
			t.Errorf("%s TotalShares = %s, want %s", tc.pool, s.TotalShares, tc.shares)
		}
		if len(s.Records) != 2 {
			t.Fatalf("%s records = %d", tc.pool, len(s.Records))
		}
		for i, r := range s.Records {
			weight := []int64{8_000_000, 2_000_000}[i]
			if r.Index != uint32(i) || r.Token != tc.tokens[i] || r.Balance.String() != tc.balances[i] || //nolint:gosec // i < 2
				r.Weight.Int64() != weight || r.Scalar.Int64() != 100_000_000_000 {
				t.Errorf("%s record %d = {%s %s w%s s%s i%d}", tc.pool, i, r.Token, r.Balance, r.Weight, r.Scalar, r.Index)
			}
		}
	}
}

func recordFields(t *testing.T, v xdr.ScVal) *xdr.ScMap {
	t.Helper()
	m := *v.Map
	return (*m)[0].Val.MustMap()
}

func TestDecodeAllRecordData_IsStrict(t *testing.T) {
	pool := comet.MainnetBLNTUSDCPool
	extra := fixtureVal(t, pool, "AllRecordData", "")
	f := recordFields(t, extra)
	sym := xdr.ScSymbol("bound")
	tru := true
	*f = append(*f, xdr.ScMapEntry{Key: xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}, Val: xdr.ScVal{Type: xdr.ScValTypeScvBool, B: &tru}})
	missing := fixtureVal(t, pool, "AllRecordData", "")
	g := recordFields(t, missing)
	*g = (*g)[:3]
	for name, v := range map[string]xdr.ScVal{"unknown field": extra, "missing field": missing, "not a map": fixtureVal(t, pool, "TotalShares", "")} {
		if _, err := cometlp.DecodeAllRecordData(v); !errors.Is(err, cometlp.ErrMalformedStorage) {
			t.Errorf("%s: err = %v, want ErrMalformedStorage", name, err)
		}
	}
}

func TestDecodeInstance_KeysAndFreeze(t *testing.T) {
	pool := comet.MainnetBLNTXLMPool
	withKey := func(sym string, val xdr.ScVal) xdr.ScVal {
		v := fixtureVal(t, pool, "Instance", "")
		s := xdr.ScSymbol(sym)
		vec := &xdr.ScVec{{Type: xdr.ScValTypeScvSymbol, Sym: &s}}
		*v.Instance.Storage = append(*v.Instance.Storage, xdr.ScMapEntry{Key: xdr.ScVal{Type: xdr.ScValTypeScvVec, Vec: &vec}, Val: val})
		return v
	}
	tru := true
	frozen, err := cometlp.DecodeInstance(withKey("Freeze", xdr.ScVal{Type: xdr.ScValTypeScvBool, B: &tru}))
	if err != nil || !frozen.Freeze {
		t.Fatalf("Freeze=true instance = %+v, %v", frozen, err)
	}
	if _, err := cometlp.DecodeInstance(withKey("PublicSwap", xdr.ScVal{Type: xdr.ScValTypeScvBool, B: &tru})); !errors.Is(err, cometlp.ErrMalformedStorage) {
		t.Errorf("unknown instance key: err = %v, want ErrMalformedStorage", err)
	}
}

// The same entries under a contract outside the curated set, or a curated pool whose instance runs another WASM, are refused.
func TestNewPoolState_GatesIdentityAndWASM(t *testing.T) {
	src := comet.MainnetBLNTUSDCPool
	rec, shares, inst := fixtureVal(t, src, "AllRecordData", ""), fixtureVal(t, src, "TotalShares", ""), fixtureVal(t, src, "Instance", "")
	const foreign = "CAUTPSQDAJEUV5XEU3EXFVYRGDJLEJGVTFJCFZCSPTDCREAMZ573VF4D"
	if _, err := cometlp.NewPoolState(foreign, rec, shares, inst); !errors.Is(err, cometlp.ErrNotGatedPool) {
		t.Errorf("foreign contract: err = %v, want ErrNotGatedPool", err)
	}
	if _, err := cometlp.NewPoolState(comet.MainnetBackstopPool, rec, shares, inst); !errors.Is(err, cometlp.ErrUnauditedWASM) {
		t.Errorf("v1.1 instance under the v1.0 pool: err = %v, want ErrUnauditedWASM", err)
	}
}

// Every curated pool's WASM is attested for source comet in the audited-WASM manifest.
func TestAuditedPoolWASM_IsInTheManifest(t *testing.T) {
	m, err := wasmaudit.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cometlp.Pools(), slices.Sorted(slices.Values(comet.MainnetGatedSet())); !slices.Equal(got, want) {
		t.Fatalf("cometlp.Pools() = %v, want the curated set %v", got, want)
	}
	for _, pool := range comet.MainnetGatedSet() {
		h, ok := cometlp.AuditedPoolWASM(pool)
		if e, in := m[h]; !ok || !in || !e.Covers(comet.SourceName) {
			t.Errorf("%s: hash %q (known %v) has no comet attestation in audited_wasm.json", pool, h, ok)
		}
	}
}
