package blnt

import (
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
)

const realPool = "CACSQWCMKG45TUHCQ3LXHOB7YEEJH4W376ZHQPXH2IZ3TE4NJHFZMCY7"

func realBLNTPositions(t *testing.T) xdr.ScVal {
	t.Helper()
	b, err := os.ReadFile("../../../test/fixtures/lending_storage/positions_entries.json")
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Protocol string `json:"protocol"`
		Pool     string `json:"pool"`
		EntryXDR string `json:"entry_xdr"`
	}
	if err := json.Unmarshal(b, &entries); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Protocol == "blnt" && e.Pool == realPool {
			return contractDataVal(t, e.EntryXDR)
		}
	}
	t.Fatal("no BLNT Positions fixture for the pool")
	return xdr.ScVal{}
}

// unmappedIndex keys a reserve index that no fixture ResConfig names (the fixtures hold no ResList).
func unmappedIndex(i uint32) string { return fmt.Sprintf("reserve-index-%d", i) }

func indexedBalances(t *testing.T, v xdr.ScVal, field string, byIndex map[uint32]string) map[string]*big.Int {
	t.Helper()
	m, ok := v.GetMap()
	if !ok || m == nil {
		t.Fatal("positions is not a map")
	}
	for _, e := range *m {
		if e.Key.Sym == nil || string(*e.Key.Sym) != field {
			continue
		}
		inner, ok := e.Val.GetMap()
		if !ok || inner == nil {
			t.Fatalf("%s is not a map", field)
		}
		out := map[string]*big.Int{}
		for _, kv := range *inner {
			if kv.Key.U32 == nil || kv.Val.I128 == nil {
				t.Fatalf("%s entry is not u32 -> i128", field)
			}
			asset, ok := byIndex[uint32(*kv.Key.U32)]
			if !ok {
				asset = unmappedIndex(uint32(*kv.Key.U32))
			}
			p := kv.Val.I128
			out[asset] = canonical.FromInt128Parts(int64(p.Hi), uint64(p.Lo)).BigInt()
		}
		return out
	}
	t.Fatalf("positions has no %s", field)
	return nil
}

// TestHealth_RealBLNTFixture runs a real BLNT Positions entry (collateral at reserves 0 and 1,
// a liability at reserve 2) through blend.AccountHealth with the pool's real ResData/ResConfig.
// The fixtures carry no ResList, no ResConfig naming reserve 2 and no oracle price, so the account
// must be refused naming reserve 2: the stand-in prices exist only to get past the price check.
func TestHealth_RealBLNTFixture(t *testing.T) {
	pool := decodeStorageFixtures(t)[realPool]
	byIndex := map[uint32]string{}
	for asset, rc := range pool.configs {
		byIndex[rc.Index] = asset
	}
	pv := realBLNTPositions(t)
	pos := blend.Positions{
		Collateral:  indexedBalances(t, pv, "collateral", byIndex),
		Liabilities: indexedBalances(t, pv, "liabilities", byIndex),
		Supply:      indexedBalances(t, pv, "supply", byIndex),
	}
	if len(pos.Collateral) != 2 || len(pos.Liabilities) != 1 {
		t.Fatalf("decoded %d collateral / %d liability reserves, want 2 / 1", len(pos.Collateral), len(pos.Liabilities))
	}
	for asset := range pos.Collateral {
		if _, ok := pool.configs[asset]; !ok {
			t.Fatalf("collateral reserve %s has no fixture config", asset)
		}
	}
	reserves := map[string]blend.HealthReserve{}
	var now uint64
	for asset := range pool.data {
		rd := pool.data[asset]
		h := blend.HealthReserve{Data: &rd}
		if rc, ok := pool.configs[asset]; ok {
			h.Config = &rc
		}
		reserves[asset] = h
		now = max(now, rd.LastTime)
	}
	prices := map[string]blend.OraclePrice{}
	for asset := range pool.data {
		prices[asset] = blend.OraclePrice{Price: big.NewInt(1), Decimals: 14, Timestamp: now}
	}
	liabilityAsset := unmappedIndex(2)
	if _, ok := pos.Liabilities[liabilityAsset]; !ok {
		t.Fatal("fixtures now map reserve 2 to an asset: replace this refusal with a valued case")
	}
	_, err := blend.AccountHealth(pos, reserves, prices, blend.PoolV2, now)
	if !errors.Is(err, blend.ErrMissingReserve) || !strings.Contains(err.Error(), liabilityAsset) {
		t.Fatalf("err = %v, want ErrMissingReserve naming %s", err, liabilityAsset)
	}
}
