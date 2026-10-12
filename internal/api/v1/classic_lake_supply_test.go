// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"errors"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

// Guards that stellar.ledger_entries_current fills `asset` for trustlines ONLY, so the broad classic supply map cannot see
// supply in claimable balances, liquidity-pool reserves or a SAC's contract_data. Fixtures: trustline sum 100.0 against true
// supply 136.6.
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

// lakeSupplyServer wires the token-supply reader on `tokenSupply` and the trustline sum behind `explorer` as main.go does.
// assetsReader stays nil so latestPreciseSupply yields nothing; RWA assets are outside watched_classic_assets and fall to these
// readers.
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
		Options: Options{TokenSupply: stub, Explorer: &trustlineOnlyExplorer{supply: map[string]string{cetesAsset: cetesTrustlineOnly}}},
		logger:  slog.Default(),
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

// TestListingSupplySeesSupplyOutsideTrustlines runs the production listing fill (fillMarketCapsFromSupply) over an asset whose
// supply is 36.6% invisible to the trustline sum and asserts the served circulating supply is the whole of it.
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

// TestRWAFillMissingSupplySeesSupplyOutsideTrustlines: an RWA row with no served price skips the market-cap fill, and
// rwaFillMissingSupply must attach the same complete supply figure.
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

// TestLakeSupplyNeverFallsBelowTheTrustlineFloor: the trustline sum is a provable lower bound on issued supply, so a lower lake
// reading means incompletely seeded flows; the served figure must not drop.
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

// failingClassicExplorer is the broad-coverage reader after its GROUP BY
// has started failing: every refresh errors, so the last good map is all
// the cache holds.
type failingClassicExplorer struct {
	ExplorerReader // nil embedded — only ClassicCirculatingSupply is called
}

func (failingClassicExplorer) ClassicCirculatingSupply(context.Context) (map[string]string, error) {
	return nil, errors.New("classic supply GROUP BY exceeded its budget")
}

// TestStaleTrustlineSumCannotOutrankALowerLakeReading: the trustline map is max()ed against live readings, so if a failed
// refresh served the last map forever, a pre-burn sum would publish burned supply. Past classicSupplyMaxAge it is not served.
func TestStaleTrustlineSumCannotOutrankALowerLakeReading(t *testing.T) {
	const postBurnLake = "400000000"
	for _, tc := range []struct {
		name      string
		attemptAt time.Time
	}{
		{"retry gap still open", time.Now()},
		{"refresh attempted and failed", time.Time{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := lakeSupplyServer(t, postBurnLake)
			s.Explorer = failingClassicExplorer{}
			s.classicSupplyCache = map[string]string{cetesAsset: cetesTrueSupply}
			s.classicSupplyAt = time.Now().Add(-classicSupplyMaxAge - time.Minute)
			s.classicSupplyAttemptAt = tc.attemptAt
			rows := cetesRow()

			s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{})

			if rows[0].CirculatingSupply == nil || rows[0].SupplyBasis == nil {
				t.Fatalf("no supply served; row=%+v", rows[0])
			}
			if got := *rows[0].CirculatingSupply; got != postBurnLake {
				t.Errorf("circulating_supply = %s, want the live lake total %s — the trustline "+
					"sum %s is older than classicSupplyMaxAge and predates the burn",
					got, postBurnLake, cetesTrueSupply)
			}
			if got := *rows[0].SupplyBasis; got != supply.BasisClassicLakeFlows.String() {
				t.Errorf("supply_basis = %s, want %s", got, supply.BasisClassicLakeFlows)
			}
		})
	}
}

// TestFreshTrustlineSumStillFloorsTheLake — the age bound must not disarm
// the floor guard: within classicSupplyMaxAge the cached map is served
// while its refresh fails, and a lake figure below it is still refused.
func TestFreshTrustlineSumStillFloorsTheLake(t *testing.T) {
	s, _ := lakeSupplyServer(t, "400000000")
	s.Explorer = failingClassicExplorer{}
	s.classicSupplyCache = map[string]string{cetesAsset: cetesTrueSupply}
	s.classicSupplyAt = time.Now().Add(-classicSupplyTTL - time.Minute)
	s.classicSupplyAttemptAt = time.Now()
	rows := cetesRow()

	s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{})

	if rows[0].CirculatingSupply == nil || *rows[0].CirculatingSupply != cetesTrueSupply {
		t.Fatalf("circulating_supply = %v, want the in-bound trustline floor %s", rows[0].CirculatingSupply, cetesTrueSupply)
	}
}

// TestLakeSupplyRefusesIncompleteFlows: Σ(burn+clawback) > Σmint means incompletely seeded flows, not negative supply. The
// listing must refuse the reading and keep the trustline sum, as /v1/assets/{asset_id}/supply and fillContractMarketCaps do.
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

// TestClassicLakeSupplyCachesNegativeAnswers: an asset the lake has nothing for must not be re-queried every request, or the
// long tail of classic assets costs a supply_flows read per listing page forever.
func TestClassicLakeSupplyCachesNegativeAnswers(t *testing.T) {
	s, stub := lakeSupplyServer(t, "") // no flows for the SAC
	rows := cetesRow()

	s.fillMarketCapsFromSupply(context.Background(), rows, map[string]int{})
	first := len(stub.asked)
	if first != 1 {
		t.Fatalf("first fill asked about %d contracts, want 1", first)
	}
	// The entry must EXIST and be empty. Asserting only "no second query"
	// would pass for the wrong reason: classicLakeSupplyRetryGap alone would
	// suppress the second read whether or not anything was cached.
	s.lakeSupplyMu.Lock()
	entry, cached := s.lakeSupply[cetesAsset]
	s.lakeSupplyAttemptAt = time.Time{} // reopen the retry gap
	s.lakeSupplyMu.Unlock()
	if !cached {
		t.Fatalf("no cache entry for an asset the lake could not answer for; "+
			"cache=%v", s.lakeSupply)
	}
	if entry.value != "" {
		t.Errorf("cached value = %q, want empty (the lake had no usable reading)", entry.value)
	}

	s.fillMarketCapsFromSupply(context.Background(), cetesRow(), map[string]int{})
	if len(stub.asked) != first {
		t.Errorf("second fill re-queried the lake (%d asks total) with the retry gap "+
			"reopened; an asset with no usable reading must be negative-cached until "+
			"the TTL lapses", len(stub.asked))
	}
}

// TestClassicLakeSupplySkipsNativeAndNonClassic: XLM's SAC supply is the amount wrapped, not the header total_coins, and
// asset_supply.go excludes it. Fiat and contract rows have no classic SAC. None may reach the lake read.
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
		got, basis := higherClassicSupply(tt.lake, tt.trustline)
		if got != tt.want {
			t.Errorf("%s: higherClassicSupply(%q, %q) = %q, want %q", tt.name, tt.lake, tt.trustline, got, tt.want)
		}
		// The basis must name the arm the value actually came from — a
		// figure whose provenance is re-derived by comparing it back
		// against the inputs mislabels every tie.
		wantBasis := supply.BasisClassicLakeFlows
		switch {
		case tt.want == "":
			wantBasis = ""
		case tt.want == tt.trustline && tt.want != tt.lake:
			wantBasis = supply.BasisClassicTrustlineSum
		}
		if basis != wantBasis {
			t.Errorf("%s: basis = %q, want %q", tt.name, basis, wantBasis)
		}
	}
}
