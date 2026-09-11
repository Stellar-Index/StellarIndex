// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"log/slog"
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// The measured defect this file guards (2026-09-11).
//
// stellar.ledger_entries_current populates its `asset` column for
// entry_type='trustline' ONLY, so the broad classic supply map — a GROUP BY on
// that column — cannot see the supply held in claimable balances, liquidity-pool
// reserves, or a Stellar Asset Contract's own contract_data balances. Against
// Horizon the hidden remainder was CETES +36.605%, TESOURO +10.594%, USTRY
// +10.272%, USDY +1.274%, with 99.9% of it SAC-held.
//
// The numbers below are that CETES shape at a round scale: a trustline sum of
// 100.0000000 units against a true supply of 136.6000000 — a 36.6% uplift, the
// extra sitting where a trustline query structurally cannot look.
const (
	cetesIssuer = "GCRYUGD5NVARGXT56XEZI5CIFCQETYHAPQQTHO2O3IQZTHDH4LATMYWC"
	cetesAsset  = "CETES-" + cetesIssuer

	// Raw 7dp stroops. cetesTrustlineOnly is what the lake's trustline GROUP
	// BY sees; cetesTrueSupply is Σmint−Σburn−Σclawback over the asset's SAC,
	// which does not care WHERE the tokens came to rest.
	cetesTrustlineOnly = "1000000000"
	cetesTrueSupply    = "1366000000"
)

// lakeSupplyStub answers the bulk lake-flows read. It records every contract
// id it is asked about so a test can prove the SAC address was derived the way
// production derives it, rather than trusting a hand-written constant.
type lakeSupplyStub struct {
	byContract map[string]clickhouse.TokenSupply
	asked      []string
	err        error
}

func (l *lakeSupplyStub) TokenSupply(_ context.Context, contractID string) (clickhouse.TokenSupply, error) {
	if l.err != nil {
		return clickhouse.TokenSupply{}, l.err
	}
	return l.byContract[contractID], nil
}

func (l *lakeSupplyStub) NativeTotalCoins(context.Context) (int64, uint32, error) {
	return 0, 0, errors.New("not used by these tests")
}

func (l *lakeSupplyStub) TokenSupplyForContracts(_ context.Context, ids []string) (map[string]clickhouse.TokenSupply, error) {
	l.asked = append(l.asked, ids...)
	if l.err != nil {
		return nil, l.err
	}
	out := make(map[string]clickhouse.TokenSupply, len(ids))
	for _, id := range ids {
		if sup, ok := l.byContract[id]; ok {
			out[id] = sup
		}
	}
	return out, nil
}

// trustlineOnlyExplorer is the broad-coverage supply reader as it exists today:
// a trustline-balance sum, blind to the other three holding domains.
type trustlineOnlyExplorer struct {
	ExplorerReader // nil embedded — only ClassicCirculatingSupply is called
	supply         map[string]string
}

func (e *trustlineOnlyExplorer) ClassicCirculatingSupply(context.Context) (map[string]string, error) {
	return e.supply, nil
}

// lakeSupplyServer wires the two readers onto a Server the way
// cmd/stellarindex-api/main.go does: the token-supply reader on `tokenSupply`
// (production: *clickhouse.SupplyReader via NewSupplyReaderAuth) and the
// trustline sum behind `explorer` (production: *clickhouse.ExplorerReader).
// assetsReader is left nil so latestPreciseSupply yields nothing — CETES and
// every other RWA asset are outside the operator's watched_classic_assets set,
// which is exactly why they fall through to these two readers at all.
func lakeSupplyServer(t *testing.T, lakeTotal string) (*Server, *lakeSupplyStub) {
	t.Helper()
	sac, err := mustSACFor(cetesAsset)
	if err != nil {
		t.Fatalf("derive CETES SAC: %v", err)
	}
	stub := &lakeSupplyStub{byContract: map[string]clickhouse.TokenSupply{}}
	if lakeTotal != "" {
		total, ok := new(big.Int).SetString(lakeTotal, 10)
		if !ok {
			t.Fatalf("bad lake total %q", lakeTotal)
		}
		stub.byContract[sac] = clickhouse.TokenSupply{
			ContractID: sac,
			Total:      total,
			Mint:       total,
			Burn:       big.NewInt(0),
			Clawback:   big.NewInt(0),
			FlowCount:  42,
		}
	}
	return &Server{
		logger:      slog.Default(),
		tokenSupply: stub,
		explorer:    &trustlineOnlyExplorer{supply: map[string]string{cetesAsset: cetesTrustlineOnly}},
	}, stub
}

func mustSACFor(assetID string) (string, error) {
	asset, err := canonical.ParseAsset(assetID)
	if err != nil {
		return "", err
	}
	return asset.SacContractID()
}

func cetesRow() []AssetDetail {
	price := "1.00"
	return []AssetDetail{{
		Kind:     "stellar_asset",
		AssetID:  cetesAsset,
		Code:     "CETES",
		Type:     string(canonical.AssetClassic),
		Decimals: 7,
		PriceUSD: &price,
	}}
}

// TestListingSupplySeesSupplyOutsideTrustlines is the regression that would
// have caught the defect. It runs the production listing fill —
// Server.fillMarketCapsFromSupply, the function handleAssets and the RWA
// classic arm both call — over an asset whose supply is 36.6% invisible to the
// trustline sum, and asserts the served circulating supply is the whole of it.
//
// Red before the fix: fillRowMarketCap read `broad` alone, so it served
// 1000000000 and a $100.00 market cap against a real $136.60.
func TestListingSupplySeesSupplyOutsideTrustlines(t *testing.T) {
	s, stub := lakeSupplyServer(t, cetesTrueSupply)
	rows := cetesRow()

	s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{})

	if rows[0].CirculatingSupply == nil {
		t.Fatalf("no circulating supply served at all; row=%+v", rows[0])
	}
	if got := *rows[0].CirculatingSupply; got != cetesTrueSupply {
		t.Errorf("circulating_supply = %s, want %s — the trustline sum (%s) cannot see "+
			"claimable-balance, LP-reserve or SAC-held supply, and serving it publishes "+
			"a 36.6%% understatement", got, cetesTrueSupply, cetesTrustlineOnly)
	}
	if rows[0].MarketCapUSD == nil {
		t.Fatalf("no market cap served; row=%+v", rows[0])
	}
	if got := *rows[0].MarketCapUSD; got != "136.60" {
		t.Errorf("market_cap_usd = %s, want 136.60 (the cap is supply x price, so the "+
			"understatement propagates straight into the headline valuation)", got)
	}

	// The contract asked about must be the asset's deterministically derived
	// Stellar Asset Contract — the same resolution asset_supply.go performs
	// for GET /v1/assets/{asset_id}/supply. A hand-rolled or mis-derived
	// address would silently return nothing and degrade to today's behaviour.
	sac, err := mustSACFor(cetesAsset)
	if err != nil {
		t.Fatal(err)
	}
	if len(stub.asked) != 1 || stub.asked[0] != sac {
		t.Errorf("lake read asked about %v, want exactly [%s] (the derived SAC address)", stub.asked, sac)
	}
}

// TestRWAFillMissingSupplySeesSupplyOutsideTrustlines covers the other fill on
// the measured surface: an RWA row with NO served price never reaches the
// market-cap fill, and rwaFillMissingSupply attaches the raw supply fact the
// reference basis values. It must attach the same complete figure.
func TestRWAFillMissingSupplySeesSupplyOutsideTrustlines(t *testing.T) {
	s, _ := lakeSupplyServer(t, cetesTrueSupply)
	rows := cetesRow()
	rows[0].PriceUSD = nil // no price → the market-cap fill returns early

	s.rwaFillMissingSupply(context.Background(), rows)

	if rows[0].CirculatingSupply == nil {
		t.Fatalf("no circulating supply served at all; row=%+v", rows[0])
	}
	if got := *rows[0].CirculatingSupply; got != cetesTrueSupply {
		t.Errorf("circulating_supply = %s, want %s", got, cetesTrueSupply)
	}
}

// TestLakeSupplyNeverFallsBelowTheTrustlineFloor is the safety property the
// whole change rests on: every trustline balance was minted, so the trustline
// sum is a PROVABLE LOWER BOUND on issued supply. A lake reading below it is
// proof the contract's flows are incompletely seeded — never evidence that the
// trustline sum is too high — so the served figure must not drop.
//
// Without this guard a partially-seeded supply_flows row would silently
// REPLACE a correct figure with a smaller one, turning a fix for an
// understatement into a new understatement.
func TestLakeSupplyNeverFallsBelowTheTrustlineFloor(t *testing.T) {
	s, _ := lakeSupplyServer(t, "3") // absurdly under-seeded
	rows := cetesRow()

	s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{})

	if rows[0].CirculatingSupply == nil {
		t.Fatalf("no circulating supply served at all; row=%+v", rows[0])
	}
	if got := *rows[0].CirculatingSupply; got != cetesTrustlineOnly {
		t.Errorf("circulating_supply = %s, want the trustline floor %s — a lake total "+
			"below the floor is under-seeding, and must never lower the served figure",
			got, cetesTrustlineOnly)
	}
}

// TestLakeSupplyDegradesToTrustlineSumOnReadFailure — the lake read is
// best-effort like every other supply overlay on this path. A ClickHouse
// failure must leave the surface exactly as it is today, not blank it.
func TestLakeSupplyDegradesToTrustlineSumOnReadFailure(t *testing.T) {
	s, stub := lakeSupplyServer(t, cetesTrueSupply)
	stub.err = errors.New("clickhouse down")
	rows := cetesRow()

	s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{})

	if rows[0].CirculatingSupply == nil {
		t.Fatalf("a failed lake read must not blank the trustline fallback; row=%+v", rows[0])
	}
	if got := *rows[0].CirculatingSupply; got != cetesTrustlineOnly {
		t.Errorf("circulating_supply = %s, want the trustline sum %s", got, cetesTrustlineOnly)
	}
}

// TestLakeSupplyRefusesIncompleteFlows — Σ(burn+clawback) > Σmint means the
// contract's flows are incompletely seeded, NOT that supply is negative. The
// listing must refuse the reading and keep the trustline sum, mirroring the
// refusal /v1/assets/{asset_id}/supply and fillContractMarketCaps already make.
func TestLakeSupplyRefusesIncompleteFlows(t *testing.T) {
	s, stub := lakeSupplyServer(t, "")
	sac, err := mustSACFor(cetesAsset)
	if err != nil {
		t.Fatal(err)
	}
	stub.byContract[sac] = clickhouse.TokenSupply{
		ContractID: sac,
		Total:      big.NewInt(-500),
		Incomplete: true,
	}
	rows := cetesRow()

	s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{})

	if rows[0].CirculatingSupply == nil || *rows[0].CirculatingSupply != cetesTrustlineOnly {
		t.Errorf("an Incomplete lake reading must be refused and the trustline sum kept; got %v", rows[0].CirculatingSupply)
	}
}

// TestClassicLakeSupplyCachesNegativeAnswers — an asset the lake has nothing
// for must not be re-queried on every request. Without the negative cache the
// long tail of classic assets would each cost a supply_flows read per listing
// page, forever.
func TestClassicLakeSupplyCachesNegativeAnswers(t *testing.T) {
	s, stub := lakeSupplyServer(t, "") // no flows for the SAC
	rows := cetesRow()

	s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{})
	first := len(stub.asked)
	if first != 1 {
		t.Fatalf("first fill asked about %d contracts, want 1", first)
	}
	s.fillMarketCapsFromSupply(context.Background(), cetesRow(), map[string]int{})
	if len(stub.asked) != first {
		t.Errorf("second fill re-queried the lake (%d asks total); an asset with no "+
			"usable reading must be negative-cached until the TTL lapses", len(stub.asked))
	}
}

// TestClassicLakeSupplySkipsNativeAndNonClassic — XLM's SAC supply is how much
// XLM is currently WRAPPED, a different quantity from the ledger header's
// total_coins, and asset_supply.go excludes it for exactly that reason. Fiat
// and contract rows have no classic SAC at all. None may reach the lake read.
func TestClassicLakeSupplySkipsNativeAndNonClassic(t *testing.T) {
	for _, assetID := range []string{"native", "XLM", "fiat:USD", "crypto:BTC"} {
		if got, ok := classicSACContractID(assetID); ok {
			t.Errorf("classicSACContractID(%q) = (%s, true), want no contract", assetID, got)
		}
	}
	if _, ok := classicSACContractID(cetesAsset); !ok {
		t.Errorf("classicSACContractID(%q) must resolve a classic asset's SAC", cetesAsset)
	}
}

// TestHigherClassicSupply pins the floor comparison itself, including the
// above-int64 case: these are raw 7dp integers that routinely exceed int64, so
// a numeric comparison that degraded to a string one would pick the wrong side.
func TestHigherClassicSupply(t *testing.T) {
	huge := new(big.Int).Lsh(big.NewInt(1), 70).String()
	tests := []struct {
		name            string
		lake, trustline string
		want            string
	}{
		{"lake above floor wins", "1366050000", "1000000000", "1366050000"},
		{"lake below floor loses", "3", "1000000000", "1000000000"},
		{"equal keeps lake", "100", "100", "100"},
		{"no lake reading", "", "1000000000", "1000000000"},
		{"no trustline reading", "1366050000", "", "1366050000"},
		{"neither", "", "", ""},
		{"lexically smaller but numerically larger", huge, "9999999999999999999", huge},
	}
	for _, tt := range tests {
		if got := higherClassicSupply(tt.lake, tt.trustline); got != tt.want {
			t.Errorf("%s: higherClassicSupply(%q, %q) = %q, want %q", tt.name, tt.lake, tt.trustline, got, tt.want)
		}
	}
}
