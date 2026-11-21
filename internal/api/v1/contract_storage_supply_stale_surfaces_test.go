// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"errors"
	"math/big"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// switchableStorageSupply serves a fixed level per contract and fails every read while fail is set.
type switchableStorageSupply struct {
	fail  atomic.Bool
	total map[string]*big.Int
}

func (s *switchableStorageSupply) ContractStorageSupply(_ context.Context, id string) (clickhouse.ContractStorageSupply, error) {
	if s.fail.Load() {
		return clickhouse.ContractStorageSupply{}, errors.New("storage scan exceeded its budget")
	}
	return clickhouse.ContractStorageSupply{ContractID: id, Total: new(big.Int).Set(s.total[id]), BalanceEntries: 3}, nil
}

// primeStorageSupply fills c for each id and leaves it as the next request finds it: long idle when healthy, or barely
// expired with its refresh failed, so only the refresh outcome, not the age, can tell the two apart.
func primeStorageSupply(t *testing.T, c *v1.CachedContractStorageSupplyReader, up *switchableStorageSupply, failRefresh bool, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := c.ContractStorageSupply(context.Background(), id); err != nil {
			t.Fatalf("fill %s: %v", id, err)
		}
	}
	up.fail.Store(failRefresh)
	for _, id := range ids {
		if !failRefresh {
			c.ExpireForTest(id, v1.StorageSupplyIdleAge)
			continue
		}
		c.ExpireForTest(id, v1.StorageSupplyTTL+time.Second)
		if _, err := c.ContractStorageSupply(context.Background(), id); err != nil {
			t.Fatalf("refresh trigger %s: %v", id, err)
		}
		c.AwaitRefreshForTest(id)
	}
}

// GET /v1/assets/{id}: a supply read kept by a failed storage refresh is served stale and not cached; an idle healthy key is clean.
func TestAssetDetail_FailedStorageRefreshIsStaleAndUncached(t *testing.T) {
	net := new(big.Int).Mul(big.NewInt(1000), big.NewInt(10_000_000))
	for _, fail := range []bool{false, true} {
		up := &switchableStorageSupply{total: map[string]*big.Int{flaggedAsset: net}}
		c := v1.NewCachedContractStorageSupplyReader(up)
		primeStorageSupply(t, c, up, fail, flaggedAsset)
		srv := v1.New(v1.Options{
			Prices: &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{flaggedAsset + "/fiat:USD": {
				AssetID: flaggedAsset, Quote: "fiat:USD", Price: "2", PriceType: "vwap",
			}}},
			TokenDecimals:         &lockstepDecStub{d: 7, found: true},
			TokenSupply:           floorNetSupply{net: net},
			ContractStorageSupply: c,
		})
		body := lockstepGet(t, srv)
		if !strings.Contains(body, `"circulating_supply":"`+net.String()+`"`) || !strings.Contains(body, `"market_cap_usd":"2000.00"`) {
			t.Fatalf("fail=%v: want the event net valued at 2000.00 either way: %s", fail, body)
		}
		if got := strings.Contains(body, `"stale":true`); got != fail {
			t.Errorf("refresh failed=%v: flags.stale = %v, want %v: %s", fail, got, fail, body)
		}
		if !fail {
			continue
		}
		// The cause clears: the next detail must re-read, not replay the stale body from the detail cache.
		up.fail.Store(false)
		if _, err := c.ContractStorageSupply(context.Background(), flaggedAsset); err != nil {
			t.Fatal(err)
		}
		c.AwaitRefreshForTest(flaggedAsset)
		if body := lockstepGet(t, srv); strings.Contains(body, `"stale":true`) {
			t.Errorf("stale after the storage refresh recovered: the stale detail was cached: %s", body)
		}
	}
}

func testContractIDs(t *testing.T, n int) []string {
	t.Helper()
	ids := make([]string, 0, n)
	for i := range n {
		var raw [32]byte
		raw[0], raw[31] = 0xC5, byte(i+1)
		id, err := strkey.Encode(strkey.VersionByteContract, raw[:])
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// GET /v1/rwa/assets: a full build over several idle-but-healthy contracts is clean; a failed storage refresh makes the view
// stale and keeps it for the degraded TTL only, so it is neither cached as clean nor pinned stale after the cause clears.
func TestRWAAssets_FailedStorageRefreshIsStaleIdleHealthyIsNot(t *testing.T) {
	ids := testContractIDs(t, 4)
	for _, fail := range []bool{false, true} {
		var (
			entries  []timescale.DirectoryEntry
			rows     = map[string]timescale.AssetRow{}
			symbols  = map[string]string{}
			supplies = map[string]string{}
			decimals = map[string]uint32{}
			levels   = map[string]*big.Int{}
		)
		for i, id := range ids {
			entries = append(entries, recognisedContract(id, "Example Treasury Fund "+string(rune('A'+i))))
			rows[id] = rwaContractRow(id, sptr("1.0740000000"))
			symbols[id] = "USTRY"
			supplies[id] = "1000000000000"
			decimals[id] = 6
			levels[id] = big.NewInt(1_000_000_000_000)
		}
		up := &switchableStorageSupply{total: levels}
		c := v1.NewCachedContractStorageSupplyReader(up)
		primeStorageSupply(t, c, up, fail, ids...)
		srv := v1.New(v1.Options{
			Sep1Cache: &stubSep1BoundReader{},
			Directory: &stubDirectoryReader{},
			AssetsReader: &rwaListStub{
				stubAssetsReaderExt: &stubAssetsReaderExt{},
				byIssuer:            map[string][]timescale.AssetRow{},
			},
			RWAContracts:          &stubRWAContractReader{contracts: entries, accounts: 18000},
			ContractCatalogue:     &stubContractCatalogue{rows: rows},
			TokenSymbol:           &stubTokenSymbols{byID: symbols},
			TokenSupply:           &stubTokenSupplies{byID: supplies},
			TokenDecimals:         &stubTokenDecimalsRdr{byID: decimals},
			NonstandardDecimals:   confirmedNonstandardDecimals(t, decimals),
			ContractStorageSupply: c,
			MinMarketCapVolumeUSD: 1000,
		})
		env, _ := getRWAEnvelope(t, srv)
		view := getRWA(t, srv)
		if len(view.Assets) != len(ids) {
			t.Fatalf("fail=%v: assets = %d, want %d: every contract must reach the storage read\n%s", fail, len(view.Assets), len(ids), funnelDump(view))
		}
		for _, a := range view.Assets {
			if a.CirculatingSupply == nil || *a.CirculatingSupply != "1000000000000" {
				t.Errorf("fail=%v: %s circulating_supply = %v, want 1000000000000 either way", fail, a.ContractID, a.CirculatingSupply)
			}
		}
		if env.Flags.Stale != fail {
			t.Errorf("refresh failed=%v: flags.stale = %v, want %v", fail, env.Flags.Stale, fail)
		}
		if degradedTTL := srv.RWAAssetsViewLifetimeForTest() <= v1.RWAAssetsDegradedTTL; degradedTTL != fail {
			t.Errorf("refresh failed=%v: kept for the degraded TTL = %v, want %v", fail, degradedTTL, fail)
		}
	}
}
