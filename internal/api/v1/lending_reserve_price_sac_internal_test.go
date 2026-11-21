// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

const sacReserveIssuer = "GBNZILSTVQZ4R7IKQDGHYGY2QXL5QOFJYQMXPKWRRM5PAV7Y4M67AQUA"

// sacReserveFixture is a classic asset's SAC that is not in the verified-catalogue SAC map, so only the lake's SAC check
// unwraps it, and a catalogue row pricing the classic asset at 2.5.
func sacReserveFixture(t *testing.T) (sac string, classic canonical.Asset, explorer *sacStubReader, assets *truthAssetsReader) {
	t.Helper()
	classic, err := canonical.NewClassicAsset("AQUA", sacReserveIssuer)
	if err != nil {
		t.Fatal(err)
	}
	sac, ok := xdrjson.SACContractID(classic.String(), pubnetPassphrase)
	if !ok {
		t.Fatal("SAC id")
	}
	price := "2.5"
	explorer = &sacStubReader{name: "AQUA:" + sacReserveIssuer, found: true}
	assets = &truthAssetsReader{byID: map[string]timescale.AssetRow{classic.String(): {AssetID: classic.String(), PriceUSD: &price}}}
	return sac, classic, explorer, assets
}

func sacReserveState(sac string) clickhouse.BlendReserveState {
	return clickhouse.BlendReserveState{
		Asset: sac, Decimals: 7, DecimalsFound: true,
		Metrics: blend.ReserveMetrics{SuppliedUnderlying: big.NewInt(100_000_000), BorrowedUnderlying: big.NewInt(40_000_000)},
	}
}

// errDirectory fails every curated-directory lookup.
type errDirectory struct{ fixedDirectory }

func (errDirectory) DirectoryEntryByAddress(context.Context, string) (timescale.DirectoryEntry, bool, error) {
	return timescale.DirectoryEntry{}, false, errors.New("directory down")
}

// A SAC reserve outside the catalogue SAC map unwraps through the lake to its classic asset and is valued at exactly that asset's
// catalogue-row price: the path every tokenised-treasury reserve takes.
func TestReserveValue_UnmappedSACUnwrapsToItsClassicCatalogueRow(t *testing.T) {
	sac, _, explorer, assets := sacReserveFixture(t)
	s := New(Options{Explorer: explorer, AssetsReader: assets, Prices: withheldUSDReader{}})
	if _, mapped := s.resolveSACAsset(sac); mapped {
		t.Fatal("fixture SAC is in the catalogue SAC map; the lake unwrap path is not exercised")
	}
	rv, supplied, stale := s.buildReserveView(context.Background(), sacReserveState(sac))
	if strPtrOr(rv.SuppliedUSD) != "25.00" || strPtrOr(rv.BorrowedUSD) != "10.00" || strPtrOr(rv.PriceUSD) != "2.5" {
		t.Fatalf("supplied_usd=%s borrowed_usd=%s price_usd=%s, want 25.00 / 10.00 / 2.5",
			strPtrOr(rv.SuppliedUSD), strPtrOr(rv.BorrowedUSD), strPtrOr(rv.PriceUSD))
	}
	if supplied == nil || supplied.Cmp(big.NewRat(25, 1)) != 0 || stale || rv.usdExclusion != "" {
		t.Errorf("TVL contribution = %v stale=%v exclusion=%q, want exactly 25, fresh, none", supplied, stale, rv.usdExclusion)
	}
}

// Each way the unwrapped classic asset's price must not be served: the reason names why and no figure is valued.
func TestReserveValue_UnwrappedSACPriceRefusals(t *testing.T) {
	sac, classic, explorer, assets := sacReserveFixture(t)
	for _, tc := range []struct {
		name string
		opts Options
		want string
	}{
		{
			"issuer flagged as a scam in the directory",
			Options{Explorer: explorer, AssetsReader: assets, Directory: fixedDirectory{sacReserveIssuer: {Tags: []string{"malicious"}}}},
			exclusionPriceWithheld,
		},
		{
			"directory read failed",
			Options{Explorer: explorer, AssetsReader: assets, Directory: errDirectory{}},
			exclusionPriceReadFailed,
		},
		{
			"canonical read withheld, so the catalogue row is never reached",
			Options{Explorer: explorer, AssetsReader: assets, Prices: withheldUSDReader{
				classic.String() + "/" + defaultPriceQuote.String(): {err: ErrPriceWithheld},
			}},
			exclusionPriceWithheld,
		},
		{
			"SAC check failed, so the wrapped issuer is unchecked",
			Options{Explorer: errSACReader{}, AssetsReader: assets},
			exclusionPriceReadFailed,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rv, supplied, _ := New(tc.opts).buildReserveView(context.Background(), sacReserveState(sac))
			if rv.SuppliedUSD != nil || rv.BorrowedUSD != nil || rv.PriceUSD != nil || supplied != nil {
				t.Fatalf("refused price valued: supplied_usd=%s price_usd=%s", strPtrOr(rv.SuppliedUSD), strPtrOr(rv.PriceUSD))
			}
			if rv.usdExclusion != tc.want {
				t.Errorf("exclusion = %q, want %q", rv.usdExclusion, tc.want)
			}
		})
	}
}

// sacPoolLending is one V2 pool holding a single reserve, with no rate configs.
type sacPoolLending struct {
	LendingReader
	asset string
}

func (sacPoolLending) BlendPoolVersion(context.Context, string) (blend.PoolVersion, error) {
	return blend.PoolV2, nil
}

func (l sacPoolLending) BlendPoolAssets(context.Context, string) ([]string, error) {
	return []string{l.asset}, nil
}

func (sacPoolLending) BlendReserveConfigs(context.Context, string) (map[string]blend.ReserveConfig, error) {
	return map[string]blend.ReserveConfig{}, nil
}

// sacPoolExplorer serves the pool's reserve state and answers the SAC check.
type sacPoolExplorer struct {
	*sacStubReader
	state clickhouse.BlendReserveState
}

func (e sacPoolExplorer) BlendPoolReserves(context.Context, string, blend.PoolVersion, []string, map[string]blend.ReserveConfig) (clickhouse.BlendPoolState, error) {
	return clickhouse.BlendPoolState{Reserves: []clickhouse.BlendReserveState{e.state}}, nil
}

// A pool build with a reserve left unvalued by a failed price read is degraded; the same build with the read answered is not.
func TestBuildPoolReserves_FailedPriceReadDegrades(t *testing.T) {
	sac, _, explorer, assets := sacReserveFixture(t)
	for _, tc := range []struct {
		name         string
		assets       AssetsReader
		wantDegraded bool
	}{
		{"price read answered", assets, false},
		{"catalogue row read failed", &truthAssetsReader{byIDErr: errors.New("db down")}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Options{
				Lending:      sacPoolLending{asset: sac},
				Explorer:     sacPoolExplorer{sacStubReader: explorer, state: sacReserveState(sac)},
				AssetsReader: tc.assets,
			})
			got, err := s.buildPoolReserves(context.Background(), "CPOOL")
			if err != nil {
				t.Fatal(err)
			}
			if got.flags.Degraded != tc.wantDegraded {
				t.Errorf("degraded = %v, want %v", got.flags.Degraded, tc.wantDegraded)
			}
			if len(got.view.Reserves) != 1 {
				t.Fatalf("reserves = %d, want 1", len(got.view.Reserves))
			}
			if tv := got.view.TVLUSD; tc.wantDegraded == (tv != nil) {
				t.Errorf("tvl_usd = %s with degraded=%v", strPtrOr(tv), tc.wantDegraded)
			} else if !tc.wantDegraded && *tv != "25.00" {
				t.Errorf("tvl_usd = %s, want 25.00", *tv)
			}
		})
	}
}
