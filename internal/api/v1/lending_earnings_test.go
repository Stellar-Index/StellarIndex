package v1_test

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const earningsAccount = "GAAZI4TCR3TY5OJHCTJC2A4QSY6CJWJH5IAJTGKIN2ER7LBNVKOCCWN7"

type stubEarningsPositions struct {
	*stubPositionsReader
	flows timescale.LendingAccountFlows
}

func (*stubEarningsPositions) BLNTPositionsByUser(context.Context, string) ([]timescale.BlendPositionFold, error) {
	return nil, nil
}

func (*stubEarningsPositions) BLNTBackstopByUser(context.Context, string) ([]timescale.BLNTBackstopFold, error) {
	return nil, nil
}

func (s *stubEarningsPositions) LendingAccountFlows(context.Context, string) (timescale.LendingAccountFlows, error) {
	return s.flows, nil
}

type stubEarningsExplorer struct {
	*stubExplorerReader
	positions clickhouse.LendingAccountPositions
	backstop  clickhouse.LendingBackstopState
	held      clickhouse.LendingAccountBackstop
}

func (e *stubEarningsExplorer) LendingAccountPositions(context.Context, string, string) (clickhouse.LendingAccountPositions, error) {
	return e.positions, nil
}

func (e *stubEarningsExplorer) LendingBackstop(context.Context, string, bool) (clickhouse.LendingBackstopState, error) {
	return e.backstop, nil
}

func (e *stubEarningsExplorer) LendingAccountBackstop(context.Context, string, string, string, []string) (clickhouse.LendingAccountBackstop, error) {
	return e.held, nil
}

func getEarnings(t *testing.T, srv *v1.Server, account string, wantStatus int) v1.LendingEarningsView {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lending/accounts/"+account+"/earnings", nil))
	if rec.Code != wantStatus {
		t.Fatalf("status = %d, want %d: %s", rec.Code, wantStatus, rec.Body)
	}
	var body struct{ Data v1.LendingEarningsView }
	if wantStatus == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
	}
	return body.Data
}

func bigStr(t *testing.T, s string) *big.Int {
	t.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("bad int %q", s)
	}
	return n
}

func strOrNull(s *string) string {
	if s == nil {
		return "null"
	}
	return *s
}

// TestLendingAccountEarnings_ExactAndWithheld: a supply leg whose folded b-tokens equal the on-chain ones (above 2^63) is
// exact; a borrow leg one d-token off is withheld with its raw components still served; a reconciled backstop is valued at
// its share price; claims are a lower bound.
func TestLendingAccountEarnings_ExactAndWithheld(t *testing.T) {
	pool, asset, token := mkCStrkey(t, 1), mkCStrkey(t, 2), mkCStrkey(t, 3)
	ex := &stubEarningsExplorer{
		stubExplorerReader: &stubExplorerReader{reserves: []clickhouse.BlendReserveState{{
			Pool: pool, Asset: asset, Decimals: 7, DecimalsFound: true,
			Data: blend.ReserveData{
				BRate: big.NewInt(1_100_000_000_000), DRate: big.NewInt(1_500_000_000_000),
				IRMod: new(big.Int), BSupply: new(big.Int), DSupply: new(big.Int), BackstopCredit: new(big.Int), Version: blend.PoolV2,
			},
		}}},
		positions: clickhouse.LendingAccountPositions{
			Readable: true,
			Supply:   map[string]*big.Int{asset: bigStr(t, "18446744073709551616")},
			Borrow:   map[string]*big.Int{asset: big.NewInt(401)},
		},
		backstop: clickhouse.LendingBackstopState{
			Instance: &clickhouse.PoolInstance{Backstop: mkCStrkey(t, 4)}, Token: token,
			Tiers: []clickhouse.BackstopTierState{{Balance: &clickhouse.BackstopBalance{Shares: big.NewInt(200), Tokens: big.NewInt(300), Q4W: new(big.Int)}}},
		},
		held: clickhouse.LendingAccountBackstop{Held: map[string]*big.Int{"": big.NewInt(100)}},
	}
	pr := &stubEarningsPositions{
		stubPositionsReader: &stubPositionsReader{
			blend: []timescale.BlendPositionFold{{
				Pool: pool, Asset: asset,
				HasSupplyLeg: true, SupplyNet: "18446744073709551616", SupplyTokens: "18446744073709551616",
				HasBorrowLeg: true, BorrowNet: "500", BorrowTokens: "400",
			}},
			backstop: []timescale.BlendBackstopFold{{Pool: pool, SharesNet: "100"}},
		},
		flows: timescale.LendingAccountFlows{
			BlendClaims:   []timescale.LendingClaimFold{{Pool: pool, Claimed: "9223372036854775808"}},
			BlendBackstop: []timescale.LendingBackstopTokenFold{{Pool: pool, NetDeposited: "120"}},
		},
	}
	srv := v1.New(v1.Options{Explorer: ex, Positions: pr, Lending: &stubLendingReader{assets: []string{asset}}})
	got := getEarnings(t, srv, earningsAccount, http.StatusOK)
	if len(got.Legs) != 3 {
		t.Fatalf("legs = %+v", got.Legs)
	}
	supply, borrow, bstop := got.Legs[0], got.Legs[1], got.Legs[2]
	if supply.Side != "supply" || !supply.Reconciled || strOrNull(supply.CurrentValue) != "20291418481080506777" ||
		strOrNull(supply.Accrued) != "1844674407370955161" || supply.Decimals == nil || *supply.Decimals != 7 {
		t.Fatalf("supply = %+v", supply)
	}
	if borrow.Side != "borrow" || borrow.Reconciled || borrow.Accrued != nil || strOrNull(borrow.Reason) != "balance_mismatch" ||
		borrow.NetFlow != "500" || strOrNull(borrow.FoldedTokens) != "400" || strOrNull(borrow.OnchainTokens) != "401" ||
		strOrNull(borrow.CurrentValue) != "602" {
		t.Fatalf("borrow = %+v", borrow)
	}
	if bstop.Side != "backstop" || !bstop.Reconciled || strOrNull(bstop.Asset) != token || strOrNull(bstop.CurrentValue) != "150" ||
		strOrNull(bstop.Accrued) != "30" {
		t.Fatalf("backstop = %+v", bstop)
	}
	if len(got.Emissions) != 1 || got.Emissions[0].Claimed != "9223372036854775808" || !got.Emissions[0].LowerBound {
		t.Fatalf("emissions = %+v", got.Emissions)
	}
}

// TestLendingAccountEarnings_UnknownAndInvalidAccount: an account with no lending history is a 200 with empty lists, as the
// account positions endpoint serves it; a malformed account is a 400.
func TestLendingAccountEarnings_UnknownAndInvalidAccount(t *testing.T) {
	srv := v1.New(v1.Options{
		Explorer:  &stubEarningsExplorer{stubExplorerReader: &stubExplorerReader{}},
		Positions: &stubEarningsPositions{stubPositionsReader: &stubPositionsReader{}},
		Lending:   &stubLendingReader{},
	})
	got := getEarnings(t, srv, earningsAccount, http.StatusOK)
	if got.Account != earningsAccount || len(got.Legs) != 0 || len(got.Emissions) != 0 || got.Legs == nil || got.Emissions == nil {
		t.Fatalf("view = %+v", got)
	}
	getEarnings(t, srv, "not-an-account", http.StatusBadRequest)
}

// TestLendingAccountEarnings_ProjectionBehind: a pool entry or backstop balance the lake shows modified after the newest folded
// event is withheld even though the token counts agree; the raw components stay.
func TestLendingAccountEarnings_ProjectionBehind(t *testing.T) {
	pool, asset := mkCStrkey(t, 1), mkCStrkey(t, 2)
	ex := &stubEarningsExplorer{
		stubExplorerReader: &stubExplorerReader{reserves: []clickhouse.BlendReserveState{{
			Pool: pool, Asset: asset,
			Data: blend.ReserveData{
				BRate: big.NewInt(1_000_000_000_000), DRate: big.NewInt(1_000_000_000_000),
				IRMod: new(big.Int), BSupply: new(big.Int), DSupply: new(big.Int), BackstopCredit: new(big.Int), Version: blend.PoolV2,
			},
		}}},
		positions: clickhouse.LendingAccountPositions{Readable: true, Supply: map[string]*big.Int{asset: big.NewInt(100)}, Ledger: 501},
		backstop: clickhouse.LendingBackstopState{
			Instance: &clickhouse.PoolInstance{Backstop: mkCStrkey(t, 4)}, Token: mkCStrkey(t, 3),
			Tiers: []clickhouse.BackstopTierState{{Balance: &clickhouse.BackstopBalance{Shares: big.NewInt(100), Tokens: big.NewInt(100), Q4W: new(big.Int)}}},
		},
		held: clickhouse.LendingAccountBackstop{Held: map[string]*big.Int{"": big.NewInt(10)}, Ledger: map[string]uint32{"": 700}},
	}
	pr := &stubEarningsPositions{
		stubPositionsReader: &stubPositionsReader{
			blend: []timescale.BlendPositionFold{{
				Pool: pool, Asset: asset, HasSupplyLeg: true, SupplyNet: "90", SupplyTokens: "100", SupplyLastLedger: 500,
			}},
			backstop: []timescale.BlendBackstopFold{{Pool: pool, SharesNet: "10"}},
		},
		flows: timescale.LendingAccountFlows{BlendBackstop: []timescale.LendingBackstopTokenFold{{Pool: pool, NetDeposited: "8", LastLedger: 699}}},
	}
	srv := v1.New(v1.Options{Explorer: ex, Positions: pr, Lending: &stubLendingReader{assets: []string{asset}}})
	got := getEarnings(t, srv, earningsAccount, http.StatusOK)
	if len(got.Legs) != 2 {
		t.Fatalf("legs = %+v", got.Legs)
	}
	for _, leg := range got.Legs {
		if leg.Reconciled || leg.Accrued != nil || strOrNull(leg.Reason) != "projection_behind" || leg.FoldedTokens == nil || leg.OnchainTokens == nil {
			t.Fatalf("leg = %+v", leg)
		}
	}
	ex.positions.Ledger, ex.held.Ledger[""] = 500, 699
	got = getEarnings(t, srv, earningsAccount, http.StatusOK)
	for _, leg := range got.Legs {
		if !leg.Reconciled {
			t.Fatalf("caught-up leg = %+v", leg)
		}
	}
}
