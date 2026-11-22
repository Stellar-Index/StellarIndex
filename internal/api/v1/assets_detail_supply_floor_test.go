// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1_test

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// floorNetSupply serves one contract's event net with flows, so the detail falls back to it.
type floorNetSupply struct{ net *big.Int }

func (s floorNetSupply) TokenSupply(_ context.Context, id string) (clickhouse.TokenSupply, error) {
	return clickhouse.TokenSupply{ContractID: id, Total: s.net, Mint: s.net, Burn: big.NewInt(0), Clawback: big.NewInt(0), FlowCount: 12}, nil
}

func (floorNetSupply) NativeTotalCoins(context.Context) (int64, uint32, error) {
	return 0, 0, errors.New("the asset detail must not read the native total supply")
}

type floorStorageSupply struct{ err error }

func (s floorStorageSupply) ContractStorageSupply(context.Context, string) (clickhouse.ContractStorageSupply, error) {
	return clickhouse.ContractStorageSupply{}, s.err
}

// GET /v1/assets/{id} values a Soroban token's event net when no observer snapshot exists. When the storage check that would
// prove that net short could not run, the cap over it is a lower bound; a SAC refusal leaves nothing unchecked (the control).
func TestAssetDetail_CapOverAnUncheckedEventNetIsALowerBound(t *testing.T) {
	net := new(big.Int).Mul(big.NewInt(1000), big.NewInt(10_000_000)) // 1000 tokens at 7 dp
	for _, tc := range []struct {
		name  string
		err   error
		floor bool
	}{
		{"undecodable entry", fmt.Errorf("%w: %s", clickhouse.ErrStorageSupplyUndecodable, flaggedAsset), true},
		{"holder set past the reader's cap", fmt.Errorf("%w: %s (>25000)", clickhouse.ErrStorageSupplyTooManyEntries, flaggedAsset), true},
		{"no instance entry captured", fmt.Errorf("%w: %s", clickhouse.ErrStorageSupplyNoInstance, flaggedAsset), true},
		{"stellar asset contract", fmt.Errorf("%w: %s", clickhouse.ErrStorageSupplyIsStellarAsset, flaggedAsset), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := v1.New(v1.Options{
				Prices: &stubPriceReader{snapshots: map[string]v1.PriceSnapshot{flaggedAsset + "/fiat:USD": {
					AssetID: flaggedAsset, Quote: "fiat:USD", Price: "2", PriceType: "vwap",
				}}},
				TokenDecimals:         &lockstepDecStub{d: 7, found: true},
				TokenSupply:           floorNetSupply{net: net},
				ContractStorageSupply: floorStorageSupply{err: tc.err},
			})
			body := lockstepGet(t, srv)
			if !strings.Contains(body, `"circulating_supply":"`+net.String()+`"`) || !strings.Contains(body, `"market_cap_usd":"2000.00"`) {
				t.Fatalf("want the event net valued at 2000.00: %s", body)
			}
			if got := strings.Contains(body, `"market_cap_lower_bound":true`); got != tc.floor {
				t.Errorf("market_cap_lower_bound = %v, want %v: %s", got, tc.floor, body)
			}
			if got := strings.Contains(body, `"circulating_supply_lower_bound":true`); got != tc.floor {
				t.Errorf("circulating_supply_lower_bound = %v, want %v: %s", got, tc.floor, body)
			}
		})
	}
}

// With no price there is no cap to carry the floor, so the supply itself must say it is one.
func TestAssetDetail_UncheckedEventNetIsAFloorWithoutACap(t *testing.T) {
	net := new(big.Int).Mul(big.NewInt(1000), big.NewInt(10_000_000))
	for _, tc := range []struct {
		err   error
		floor bool
	}{
		{fmt.Errorf("%w: %s (>25000)", clickhouse.ErrStorageSupplyTooManyEntries, flaggedAsset), true},
		{fmt.Errorf("%w: %s", clickhouse.ErrStorageSupplyIsStellarAsset, flaggedAsset), false},
	} {
		srv := v1.New(v1.Options{
			Prices:                &stubPriceReader{},
			TokenDecimals:         &lockstepDecStub{d: 7, found: true},
			TokenSupply:           floorNetSupply{net: net},
			ContractStorageSupply: floorStorageSupply{err: tc.err},
		})
		body := lockstepGet(t, srv)
		if !strings.Contains(body, `"circulating_supply":"`+net.String()+`"`) || strings.Contains(body, `"market_cap_usd":"`) {
			t.Fatalf("want the event net served with no cap: %s", body)
		}
		if got := strings.Contains(body, `"circulating_supply_lower_bound":true`); got != tc.floor {
			t.Errorf("%v: circulating_supply_lower_bound = %v, want %v: %s", tc.err, got, tc.floor, body)
		}
	}
}
