// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// classicSurfaces is one classic asset as /v1/assets, /v1/assets/{id} and /v1/assets/{id}/supply serve it.
type classicSurfaces struct {
	listing       AssetDetail
	listingUnread bool
	detail        AssetDetail
	supplyCode    int
	supply        AssetSupply
	supplyFlags   Flags
}

func readClassicSurfaces(t *testing.T, s *Server, assetID string) classicSurfaces {
	t.Helper()
	asset, err := canonical.ParseAsset(assetID)
	if err != nil {
		t.Fatal(err)
	}
	var out classicSurfaces
	rows := []AssetDetail{{AssetID: assetID, Type: string(asset.Type), Decimals: 7}}
	out.listingUnread = s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{})
	out.listing = rows[0]
	out.detail = AssetDetail{AssetID: assetID, Decimals: 7}
	s.applyF2Fields(context.Background(), &out.detail, asset)

	req := httptest.NewRequest(http.MethodGet, "/v1/assets/"+assetID+"/supply", nil)
	req.SetPathValue("asset_id", assetID)
	rec := httptest.NewRecorder()
	s.handleAssetSupply(rec, req)
	out.supplyCode = rec.Code
	if rec.Code == http.StatusOK {
		var env struct {
			Data  AssetSupply `json:"data"`
			Flags Flags       `json:"flags"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("decode /supply: %v: %s", err, rec.Body.String())
		}
		out.supply, out.supplyFlags = env.Data, env.Flags
		// Degraded is never on the wire; it reaches a client as no-store.
		out.supplyFlags.Degraded = rec.Header().Get("Cache-Control") == "no-store"
	}
	return out
}

// surfacesServer wires the lake-flows reader, the trustline sum and (optionally) the observer as main.go does.
func surfacesServer(t *testing.T, lakeTotal string, explorer ExplorerReader, observed string) (*Server, *lakeSupplyStub) {
	t.Helper()
	s, stub := lakeSupplyServer(t, lakeTotal)
	s.logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	if explorer != nil {
		s.Explorer = explorer
	}
	if observed != "" {
		circ, _ := new(big.Int).SetString(observed, 10)
		s.AssetsReader = &observedAssets{obs: map[string]timescale.SupplyObservation{
			cetesAsset: {CirculatingSupply: observed, Basis: string(supply.BasisIssuerExclusion), ObservedAt: time.Now()},
		}}
		s.Supply = capSupplyStub{snap: supply.Supply{
			CirculatingSupply: circ, TotalSupply: circ, Basis: supply.BasisIssuerExclusion, ObservedAt: time.Now(),
		}}
	}
	return s, stub
}

type observedAssets struct {
	AssetsReader
	obs map[string]timescale.SupplyObservation
}

func (o *observedAssets) LatestSupplyObservations(context.Context, time.Duration) (map[string]timescale.SupplyObservation, error) {
	return o.obs, nil
}

// For one classic asset the listing, the detail and /supply agree on the figure's semantics: the trustline floor is never
// presented as a total, a net below that floor is marked a lower bound, and a read that did not answer is flagged.
func TestClassicSupply_ThreeSurfacesAgree(t *testing.T) {
	const (
		lakeBelowFloor = "500000000" // below cetesTrustlineOnly
		observed       = "1200000000"
	)
	t.Run("trustline floor above the lake net", func(t *testing.T) {
		s, _ := surfacesServer(t, lakeBelowFloor, nil, "")
		got := readClassicSurfaces(t, s, cetesAsset)
		for name, d := range map[string]AssetDetail{"listing": got.listing, "detail": got.detail} {
			if strOr(d.CirculatingSupply) != cetesTrustlineOnly || !d.CirculatingSupplyLowerBound ||
				strOr(d.SupplyBasis) != supply.BasisClassicTrustlineSum.String() {
				t.Errorf("%s: circ=%q lb=%v basis=%q, want the trustline floor marked a lower bound",
					name, strOr(d.CirculatingSupply), d.CirculatingSupplyLowerBound, strOr(d.SupplyBasis))
			}
		}
		if got.detail.TotalSupply != nil {
			t.Errorf("detail total_supply = %q, want absent for a floor", *got.detail.TotalSupply)
		}
		if got.supplyCode != http.StatusOK || got.supply.TotalSupply != lakeBelowFloor || !got.supply.CirculatingSupplyLowerBound {
			t.Errorf("/supply: code=%d total=%q lb=%v, want the net kept and marked a lower bound",
				got.supplyCode, got.supply.TotalSupply, got.supply.CirculatingSupplyLowerBound)
		}
	})

	t.Run("lake net at or above the floor", func(t *testing.T) {
		s, _ := surfacesServer(t, cetesTrueSupply, nil, "")
		got := readClassicSurfaces(t, s, cetesAsset)
		for name, d := range map[string]AssetDetail{"listing": got.listing, "detail": got.detail} {
			if strOr(d.CirculatingSupply) != cetesTrueSupply || d.CirculatingSupplyLowerBound ||
				strOr(d.SupplyBasis) != supply.BasisClassicLakeFlows.String() {
				t.Errorf("%s: circ=%q lb=%v basis=%q, want the lake net",
					name, strOr(d.CirculatingSupply), d.CirculatingSupplyLowerBound, strOr(d.SupplyBasis))
			}
		}
		if strOr(got.detail.TotalSupply) != cetesTrueSupply || got.detail.f2ReadFailed {
			t.Errorf("detail total_supply=%q failed=%v, want the net, read clean", strOr(got.detail.TotalSupply), got.detail.f2ReadFailed)
		}
		if got.supply.TotalSupply != cetesTrueSupply || got.supply.CirculatingSupplyLowerBound || got.supplyFlags.Degraded {
			t.Errorf("/supply: total=%q lb=%v degraded=%v, want the net unmarked",
				got.supply.TotalSupply, got.supply.CirculatingSupplyLowerBound, got.supplyFlags.Degraded)
		}
	})

	t.Run("observed asset", func(t *testing.T) {
		s, _ := surfacesServer(t, cetesTrueSupply, nil, observed)
		got := readClassicSurfaces(t, s, cetesAsset)
		for name, d := range map[string]AssetDetail{"listing": got.listing, "detail": got.detail} {
			if strOr(d.CirculatingSupply) != observed || d.CirculatingSupplyLowerBound ||
				strOr(d.SupplyBasis) != supply.BasisIssuerExclusion.String() {
				t.Errorf("%s: circ=%q lb=%v basis=%q, want the observer's figure",
					name, strOr(d.CirculatingSupply), d.CirculatingSupplyLowerBound, strOr(d.SupplyBasis))
			}
		}
	})

	t.Run("lake read fails", func(t *testing.T) {
		s, stub := surfacesServer(t, cetesTrueSupply, nil, "")
		stub.err = errLakeDown
		got := readClassicSurfaces(t, s, cetesAsset)
		for name, d := range map[string]AssetDetail{"listing": got.listing, "detail": got.detail} {
			if strOr(d.CirculatingSupply) != cetesTrustlineOnly || !d.CirculatingSupplyLowerBound || d.TotalSupply != nil {
				t.Errorf("%s: circ=%q lb=%v total=%q, want only the floor, marked", name,
					strOr(d.CirculatingSupply), d.CirculatingSupplyLowerBound, strOr(d.TotalSupply))
			}
		}
		if !got.detail.f2ReadFailed {
			t.Error("detail: an unanswered lake read must be flagged")
		}
		if got.supplyCode != http.StatusBadGateway {
			t.Errorf("/supply code = %d, want 502", got.supplyCode)
		}
	})

	t.Run("trustline read fails", func(t *testing.T) {
		s, _ := surfacesServer(t, cetesTrueSupply, failingClassicExplorer{}, "")
		got := readClassicSurfaces(t, s, cetesAsset)
		if !got.listingUnread || !got.detail.f2ReadFailed || !got.supplyFlags.Degraded || !got.supplyFlags.Stale {
			t.Errorf("unchecked floor must be flagged: listing=%v detail=%v supply degraded=%v stale=%v",
				got.listingUnread, got.detail.f2ReadFailed, got.supplyFlags.Degraded, got.supplyFlags.Stale)
		}
		if strOr(got.detail.CirculatingSupply) != cetesTrueSupply || got.supply.TotalSupply != cetesTrueSupply {
			t.Errorf("detail circ=%q /supply total=%q, want the net", strOr(got.detail.CirculatingSupply), got.supply.TotalSupply)
		}
	})

	t.Run("every arm fails", func(t *testing.T) {
		s, stub := surfacesServer(t, cetesTrueSupply, failingClassicExplorer{}, "")
		stub.err = errLakeDown
		got := readClassicSurfaces(t, s, cetesAsset)
		if got.detail.CirculatingSupply != nil || got.listing.CirculatingSupply != nil || !got.detail.f2ReadFailed {
			t.Errorf("want no figure and a flagged detail: listing=%q detail=%q failed=%v",
				strOr(got.listing.CirculatingSupply), strOr(got.detail.CirculatingSupply), got.detail.f2ReadFailed)
		}
	})
}

var errLakeDown = errors.New("supply_flows sum failed")

// Native and Soroban details keep their own arms: the classic reading never touches them.
func TestClassicSupply_DetailLeavesNativeAndContractAssetsAlone(t *testing.T) {
	s, stub := surfacesServer(t, cetesTrueSupply, nil, "")
	native := AssetDetail{AssetID: "native", Decimals: 7}
	s.applyF2Fields(context.Background(), &native, canonical.Asset{Type: canonical.AssetNative})
	if native.CirculatingSupply != nil || len(stub.asked) != 0 {
		t.Errorf("native: circ=%q lake asked %v, want neither", strOr(native.CirculatingSupply), stub.asked)
	}

	sac, err := mustSACFor(cetesAsset)
	if err != nil {
		t.Fatal(err)
	}
	stub.byContract[sac] = clickhouse.TokenSupply{
		ContractID: sac, Total: big.NewInt(77), Mint: big.NewInt(77), Burn: big.NewInt(0), Clawback: big.NewInt(0), FlowCount: 1,
	}
	contract := AssetDetail{AssetID: sac, Decimals: 7}
	s.applyF2Fields(context.Background(), &contract, canonical.Asset{Type: canonical.AssetSoroban, ContractID: sac})
	if strOr(contract.SupplyBasis) == supply.BasisClassicTrustlineSum.String() || strOr(contract.SupplyBasis) == supply.BasisClassicLakeFlows.String() {
		t.Errorf("contract: basis %q, want its own arm", strOr(contract.SupplyBasis))
	}
}
