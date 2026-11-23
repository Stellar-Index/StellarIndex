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
	"strings"
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

// An unwatched asset outside the batch a running lake refresh is summing is answered by the per-asset reader /supply uses:
// the detail never joins another batch's flight, reads clean, and agrees with the listing.
func TestClassicSupply_DetailIgnoresAForeignLakeFlight(t *testing.T) {
	asset, err := canonical.ParseAsset(cetesAsset)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct{ lake, wantCirc string }{
		"asset has a lake net":  {cetesTrueSupply, cetesTrueSupply},
		"asset has no lake net": {"", cetesTrustlineOnly},
	} {
		t.Run(name, func(t *testing.T) {
			s, stub := surfacesServer(t, tc.lake, nil, "")
			s.lakeSupplyFlight, s.lakeSupplyAttemptAt = make(chan struct{}), time.Now() // never lands
			detail := AssetDetail{AssetID: cetesAsset, Decimals: 7}
			s.applyF2Fields(context.Background(), &detail, asset)
			if detail.f2ReadFailed || strOr(detail.CirculatingSupply) != tc.wantCirc {
				t.Errorf("circ=%q failed=%v, want %q read clean", strOr(detail.CirculatingSupply), detail.f2ReadFailed, tc.wantCirc)
			}
			if len(stub.asked) != 0 {
				t.Errorf("detail asked the bulk lake read for %v, want the per-asset reader", stub.asked)
			}
		})
	}
}

// A trustline sum taken before a burn can exceed true supply, so a sum older than classicFloorMaxLag neither marks the net nor
// stands in as the figure, on any surface; the read is flagged. A sum a healthy rollup cycle old is still a floor.
func TestClassicSupply_OldTrustlineSumIsNoFloor(t *testing.T) {
	const net = "500000000" // below cetesTrustlineOnly
	for name, tc := range map[string]struct {
		age            time.Duration
		lake, wantCirc string
		wantLB         bool
	}{
		"old sum, net present":   {classicFloorMaxLag + 30*time.Minute, net, net, false},
		"old sum, no net":        {classicFloorMaxLag + 30*time.Minute, "", "null", false},
		"healthy sum, net below": {45 * time.Minute, net, cetesTrustlineOnly, true},
	} {
		t.Run(name, func(t *testing.T) {
			sums := &trustlineOnlyExplorer{supply: map[string]string{cetesAsset: cetesTrustlineOnly}, computedAt: time.Now().Add(-tc.age)}
			s, _ := surfacesServer(t, tc.lake, sums, "")
			got := readClassicSurfaces(t, s, cetesAsset)
			for surface, d := range map[string]AssetDetail{"listing": got.listing, "detail": got.detail} {
				if strOr(d.CirculatingSupply) != tc.wantCirc || d.CirculatingSupplyLowerBound != tc.wantLB {
					t.Errorf("%s: circ=%q lb=%v, want %q lb=%v", surface, strOr(d.CirculatingSupply), d.CirculatingSupplyLowerBound, tc.wantCirc, tc.wantLB)
				}
			}
			if got.detail.f2ReadFailed == tc.wantLB {
				t.Errorf("detail failed=%v, want %v", got.detail.f2ReadFailed, !tc.wantLB)
			}
			if tc.lake != "" && (got.supply.CirculatingSupplyLowerBound != tc.wantLB || got.supplyFlags.Degraded == tc.wantLB) {
				t.Errorf("/supply: lb=%v degraded=%v, want lb=%v", got.supply.CirculatingSupplyLowerBound, got.supplyFlags.Degraded, tc.wantLB)
			}
		})
	}
}

// detailExplorer is the trustline sum plus the issuer-account read the detail handler makes; the issuer has no lake state.
type detailExplorer struct{ trustlineOnlyExplorer }

func (*detailExplorer) AccountStateCached(context.Context, string) (clickhouse.AccountState, bool, error) {
	return clickhouse.AccountState{}, false, nil
}

// Through the real detail handler: a priced trustline floor values a cap marked a lower bound and no FDV; a healthy read is
// cached; a failed lake read is served stale and no-store and never cached.
func TestClassicSupply_DetailHandlerValuesAndCachesTheReading(t *testing.T) {
	asset, err := canonical.ParseAsset(cetesAsset)
	if err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		lakeErr error
		cached  bool
	}{"healthy": {nil, true}, "lake read fails": {errLakeDown, false}} {
		t.Run(name, func(t *testing.T) {
			_, stub := lakeSupplyServer(t, "500000000") // below cetesTrustlineOnly
			stub.err = tc.lakeErr
			s := New(Options{
				Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
				TokenSupply: stub,
				Explorer:    &detailExplorer{trustlineOnlyExplorer{supply: map[string]string{cetesAsset: cetesTrustlineOnly}}},
				Prices:      fixedPriceReader{snap: PriceSnapshot{AssetID: cetesAsset, Quote: "fiat:USD", Price: "2", PriceType: "vwap"}},
			})
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/assets/"+cetesAsset, nil))
			body := rec.Body.String()
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d: %s", rec.Code, body)
			}
			for _, want := range []string{
				`"circulating_supply":"` + cetesTrustlineOnly + `"`, `"circulating_supply_lower_bound":true`,
				`"market_cap_usd":"200.00"`, `"market_cap_lower_bound":true`,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("body lacks %s: %s", want, body)
				}
			}
			if strings.Contains(body, `"fdv_usd":"`) || strings.Contains(body, `"total_supply":"`) {
				t.Errorf("a floor valued an FDV or served a total: %s", body)
			}
			_, cached := s.assetDetailCache.get(assetDetailCacheKey(asset, false))
			if cached != tc.cached || noStore(rec) == tc.cached || decodeFlags(t, rec).Stale == tc.cached {
				t.Errorf("cached=%v no-store=%v stale=%v, want cached=%v", cached, noStore(rec), decodeFlags(t, rec).Stale, tc.cached)
			}
		})
	}
}

// sacExplorer is the trustline sum plus the lake's SAC instance lookup (contract id to "CODE:ISSUER").
type sacExplorer struct {
	trustlineOnlyExplorer
	wraps map[string]string
}

func (e *sacExplorer) SACClassicAssetName(_ context.Context, contractID string) (string, bool, error) {
	name, ok := e.wraps[contractID]
	return name, ok, nil
}

// /supply marks a classic asset's net below its trustline floor whichever id form names it; a Soroban-native contract is not
// checked against any trustline sum.
func TestClassicSupply_SupplyFloorMarkedForTheSACForm(t *testing.T) {
	sac, err := mustSACFor(cetesAsset)
	if err != nil {
		t.Fatal(err)
	}
	native, err := mustSACFor("OTHER-" + cetesIssuer)
	if err != nil {
		t.Fatal(err)
	}
	s, stub := surfacesServer(t, "500000000", nil, "")
	stub.byContract[native] = clickhouse.TokenSupply{
		ContractID: native, Total: big.NewInt(5), Mint: big.NewInt(5), Burn: big.NewInt(0), Clawback: big.NewInt(0), FlowCount: 1,
	}
	s.Explorer = &sacExplorer{
		trustlineOnlyExplorer: trustlineOnlyExplorer{supply: map[string]string{cetesAsset: cetesTrustlineOnly}},
		wraps:                 map[string]string{sac: "CETES:" + cetesIssuer},
	}
	for id, wantLB := range map[string]bool{cetesAsset: true, sac: true, native: false} {
		got := readClassicSurfaces(t, s, id)
		if got.supplyCode != http.StatusOK || got.supply.CirculatingSupplyLowerBound != wantLB || got.supplyFlags.Degraded {
			t.Errorf("%s: code=%d lb=%v degraded=%v, want lb=%v clean", id, got.supplyCode,
				got.supply.CirculatingSupplyLowerBound, got.supplyFlags.Degraded, wantLB)
		}
	}
}

// A watched asset whose observer reading is past preciseSupplyMaxAge keeps that reading, flagged stale, with its operator
// max-supply override: the lake fallback is for assets the observer does not watch.
func TestClassicSupply_StaleWatchedAssetKeepsItsObserverReading(t *testing.T) {
	const observed = "1200000000"
	s, _ := surfacesServer(t, cetesTrueSupply, nil, "")
	circ, _ := new(big.Int).SetString(observed, 10)
	s.Supply = capSupplyStub{snap: supply.Supply{
		AssetKey: cetesAsset, CirculatingSupply: circ, TotalSupply: circ, MaxSupply: big.NewInt(9_000_000_000),
		Basis: supply.BasisIssuerExclusion, ObservedAt: time.Now().Add(-preciseSupplyMaxAge - time.Hour),
	}}
	asset, err := canonical.ParseAsset(cetesAsset)
	if err != nil {
		t.Fatal(err)
	}
	detail := AssetDetail{AssetID: cetesAsset, Decimals: 7}
	s.applyF2Fields(context.Background(), &detail, asset)
	if strOr(detail.CirculatingSupply) != observed || !detail.SupplyStale || strOr(detail.MaxSupply) != "9000000000" ||
		strOr(detail.SupplyBasis) != supply.BasisIssuerExclusion.String() {
		t.Errorf("circ=%q stale=%v max=%q basis=%q, want the observer reading flagged stale with its override",
			strOr(detail.CirculatingSupply), detail.SupplyStale, strOr(detail.MaxSupply), strOr(detail.SupplyBasis))
	}
}

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
