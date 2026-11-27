package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const earningsAccount = "GAAZI4TCR3TY5OJHCTJC2A4QSY6CJWJH5IAJTGKIN2ER7LBNVKOCCWN7"

type stubEarningsPositions struct {
	*stubPositionsReader
	blntFolds []timescale.BlendPositionFold
	flows     timescale.LendingAccountFlows
	flowsErr  error
}

func (s *stubEarningsPositions) BLNTPositionsByUser(context.Context, string) ([]timescale.BlendPositionFold, error) {
	return s.blntFolds, nil
}

func (*stubEarningsPositions) BLNTBackstopByUser(context.Context, string) ([]timescale.BLNTBackstopFold, error) {
	return nil, nil
}

func (s *stubEarningsPositions) LendingAccountFlows(context.Context, string) (timescale.LendingAccountFlows, error) {
	return s.flows, s.flowsErr
}

type stubEarningsExplorer struct {
	*stubExplorerReader
	positions clickhouse.LendingAccountPositions
	backstop  clickhouse.LendingBackstopState
	held      clickhouse.LendingAccountBackstop
	posErr    error
}

func (e *stubEarningsExplorer) LendingAccountPositions(context.Context, string, string) (clickhouse.LendingAccountPositions, error) {
	return e.positions, e.posErr
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
				LastTime: 1_700_000_000,
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
	srv := v1.New(v1.Options{Explorer: ex, Positions: pr, Lending: &stubLendingReader{assets: []string{asset}}, CompletenessReader: verifiedLendingHistory()})
	got := getEarnings(t, srv, earningsAccount, http.StatusOK)
	if len(got.Legs) != 3 {
		t.Fatalf("legs = %+v", got.Legs)
	}
	supply, borrow, bstop := got.Legs[0], got.Legs[1], got.Legs[2]
	if supply.Side != "supply" || !supply.Reconciled || strOrNull(supply.CurrentValue) != "20291418481080506777" ||
		strOrNull(supply.Accrued) != "1844674407370955161" || supply.Decimals == nil || *supply.Decimals != 7 {
		t.Fatalf("supply = %+v", supply)
	}
	if supply.RateAsOf == nil || time.Time(*supply.RateAsOf).Unix() != 1_700_000_000 {
		t.Fatalf("supply rate_as_of = %v", supply.RateAsOf)
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
// account positions endpoint serves it; a contract (C-strkey) account is accepted; a malformed account is a 400.
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
	contract := mkCStrkey(t, 9)
	if got := getEarnings(t, srv, contract, http.StatusOK); got.Account != contract {
		t.Fatalf("contract account view = %+v", got)
	}
}

// verifiedLendingHistory is a verdict set whose Blend projections are verified from their genesis.
func verifiedLendingHistory() *stubCompletenessReader {
	now := time.Now().UTC()
	return &stubCompletenessReader{snaps: []timescale.CompletenessSnapshot{
		{Source: "blend", Genesis: 100, ProjectionOK: true, ProjectionVerifiedFrom: 100, ComputedAt: now, ProjectionEvidencedAt: now},
		{Source: "blend_backstop", Genesis: 100, ProjectionOK: true, ProjectionVerifiedFrom: 100, ComputedAt: now, ProjectionEvidencedAt: now},
	}}
}

// TestLendingAccountEarnings_HistoryUnverified: a leg that reconciles is still withheld unless its source's projection is
// verified from genesis; an unknown floor is never read as "from genesis", and a failed verdict read degrades the response.
func TestLendingAccountEarnings_HistoryUnverified(t *testing.T) {
	pool, asset := mkCStrkey(t, 1), mkCStrkey(t, 2)
	ex := &stubEarningsExplorer{
		stubExplorerReader: &stubExplorerReader{reserves: []clickhouse.BlendReserveState{{
			Pool: pool, Asset: asset,
			Data: blend.ReserveData{
				BRate: big.NewInt(1_000_000_000_000), DRate: big.NewInt(1_000_000_000_000),
				IRMod: new(big.Int), BSupply: new(big.Int), DSupply: new(big.Int), BackstopCredit: new(big.Int), Version: blend.PoolV2,
			},
		}}},
		positions: clickhouse.LendingAccountPositions{Readable: true, Supply: map[string]*big.Int{asset: big.NewInt(100)}, Ledger: 500},
	}
	pr := &stubEarningsPositions{stubPositionsReader: &stubPositionsReader{blend: []timescale.BlendPositionFold{{
		Pool: pool, Asset: asset, HasSupplyLeg: true, SupplyNet: "90", SupplyTokens: "100", SupplyLastLedger: 500,
	}}}}
	snap := func(ok bool, from uint32, computed time.Time) *stubCompletenessReader {
		return &stubCompletenessReader{snaps: []timescale.CompletenessSnapshot{{
			Source: "blend", Genesis: 100, ProjectionOK: ok, ProjectionVerifiedFrom: from, ComputedAt: computed, ProjectionEvidencedAt: computed,
		}}}
	}
	now := time.Now().UTC()
	unrelatedStale := snap(true, 100, now)
	unrelatedStale.snaps = append(unrelatedStale.snaps, timescale.CompletenessSnapshot{Source: "aquarius", ProjectionOK: true, ProjectionVerifiedFrom: 1})
	cases := []struct {
		name               string
		reader             *stubCompletenessReader
		exact, fail, stale bool
		reason             string
	}{
		{name: "verified from genesis", reader: snap(true, 100, now), exact: true},
		{name: "an unrelated source's verdict is stale", reader: unrelatedStale, exact: true},
		{name: "verdict past its freshness gate", reader: snap(true, 100, time.Time{}), reason: "history_verdict_stale", stale: true},
		{name: "no reader"},
		{name: "no verdict for the source", reader: &stubCompletenessReader{}},
		{name: "floor after genesis", reader: snap(true, 101, now)},
		{name: "floor unknown", reader: snap(true, 0, now)},
		{name: "projection not ok", reader: snap(false, 100, now)},
		{name: "verdict read fails", reader: &stubCompletenessReader{err: errors.New("pg down")}, fail: true, reason: "history_verdict_unread"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// An undeclared scale, read: an unwired reader is an unread scale, which degrades the response by itself.
			opts := v1.Options{Explorer: ex, Positions: pr, Lending: &stubLendingReader{assets: []string{asset}}, TokenDecimals: &lockstepDecStub{}}
			if tc.reader != nil {
				opts.CompletenessReader = tc.reader
			}
			rec := httptest.NewRecorder()
			v1.New(opts).Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lending/accounts/"+earningsAccount+"/earnings", nil))
			var body struct {
				Data  v1.LendingEarningsView
				Flags struct{ Stale bool }
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK || len(body.Data.Legs) != 1 {
				t.Fatalf("status %d err %v body %s", rec.Code, err, rec.Body)
			}
			leg := body.Data.Legs[0]
			if body.Flags.Stale != tc.stale {
				t.Fatalf("flags.stale = %v, want %v", body.Flags.Stale, tc.stale)
			}
			if tc.exact {
				if !leg.Reconciled || strOrNull(leg.Accrued) != "10" {
					t.Fatalf("leg = %+v", leg)
				}
				return
			}
			want := tc.reason
			if want == "" {
				want = "history_unverified"
			}
			if leg.Reconciled || leg.Accrued != nil || strOrNull(leg.Reason) != want || strOrNull(leg.CurrentValue) != "100" || leg.NetFlow != "90" {
				t.Fatalf("leg = %+v", leg)
			}
			if noStore := strings.Contains(rec.Header().Get("Cache-Control"), "no-store"); noStore != tc.fail {
				t.Fatalf("no-store = %v, want %v", noStore, tc.fail)
			}
		})
	}
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
	srv := v1.New(v1.Options{Explorer: ex, Positions: pr, Lending: &stubLendingReader{assets: []string{asset}}, CompletenessReader: verifiedLendingHistory()})
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

// TestLendingAccountEarnings_RateBehindFold: a reserve rate stored before the leg's newest folded event cannot value the
// tokens that event minted, so accrued is withheld as rate_behind even though the balances agree.
func TestLendingAccountEarnings_RateBehindFold(t *testing.T) {
	pool, asset := mkCStrkey(t, 1), mkCStrkey(t, 2)
	ex := &stubEarningsExplorer{
		stubExplorerReader: &stubExplorerReader{reserves: []clickhouse.BlendReserveState{{
			Pool: pool, Asset: asset,
			Data: blend.ReserveData{
				BRate: big.NewInt(1_000_000_000_000), DRate: big.NewInt(1_000_000_000_000),
				IRMod: new(big.Int), BSupply: new(big.Int), DSupply: new(big.Int), BackstopCredit: new(big.Int), Version: blend.PoolV2,
				LastTime: 1_700_000_000,
			},
		}}},
		positions: clickhouse.LendingAccountPositions{Readable: true, Supply: map[string]*big.Int{asset: big.NewInt(100)}, Ledger: 500},
	}
	fold := timescale.BlendPositionFold{
		Pool: pool, Asset: asset, HasSupplyLeg: true, SupplyNet: "90", SupplyTokens: "100", SupplyLastLedger: 500,
		SupplyLastActivity: time.Unix(1_700_000_005, 0).UTC(),
	}
	pr := &stubEarningsPositions{stubPositionsReader: &stubPositionsReader{blend: []timescale.BlendPositionFold{fold}}}
	srv := v1.New(v1.Options{Explorer: ex, Positions: pr, Lending: &stubLendingReader{assets: []string{asset}}, CompletenessReader: verifiedLendingHistory()})
	got := getEarnings(t, srv, earningsAccount, http.StatusOK)
	if len(got.Legs) != 1 {
		t.Fatalf("legs = %+v", got.Legs)
	}
	if leg := got.Legs[0]; leg.Reconciled || leg.Accrued != nil || strOrNull(leg.Reason) != "rate_behind" || strOrNull(leg.CurrentValue) != "100" {
		t.Fatalf("leg = %+v", leg)
	}
	pr.blend[0].SupplyLastActivity = time.Unix(1_700_000_000, 0).UTC()
	srv = v1.New(v1.Options{Explorer: ex, Positions: pr, Lending: &stubLendingReader{assets: []string{asset}}, CompletenessReader: verifiedLendingHistory()})
	if leg := getEarnings(t, srv, earningsAccount, http.StatusOK).Legs[0]; !leg.Reconciled || strOrNull(leg.Accrued) != "10" {
		t.Fatalf("rate stored at the newest event: leg = %+v", leg)
	}

	// A fully exited leg holds no tokens for a stale rate to misvalue, so accrued is exact: 0 - (-5).
	exited := fold
	exited.SupplyNet, exited.SupplyTokens = "-5", "0"
	exPos := ex.positions
	ex.positions = clickhouse.LendingAccountPositions{Readable: true, Supply: map[string]*big.Int{}, Ledger: 500}
	pr.blend[0] = exited
	srv = v1.New(v1.Options{Explorer: ex, Positions: pr, Lending: &stubLendingReader{assets: []string{asset}}, CompletenessReader: verifiedLendingHistory()})
	if leg := getEarnings(t, srv, earningsAccount, http.StatusOK).Legs[0]; !leg.Reconciled || strOrNull(leg.Accrued) != "5" {
		t.Fatalf("zero on-chain balance with the rate behind: leg = %+v", leg)
	}

	// A BLNT leg is gated the same way.
	ex.positions = exPos
	blntFold := fold
	blntFold.Pool = blnt.MainnetFixedPool
	ex.reserves[0].Pool = blnt.MainnetFixedPool
	pr = &stubEarningsPositions{stubPositionsReader: &stubPositionsReader{}, blntFolds: []timescale.BlendPositionFold{blntFold}}
	verdicts := verifiedLendingHistory()
	now := time.Now().UTC()
	verdicts.snaps = append(verdicts.snaps, timescale.CompletenessSnapshot{
		Source: blnt.SourceName, Genesis: 100, ProjectionOK: true, ProjectionVerifiedFrom: 100, ComputedAt: now, ProjectionEvidencedAt: now,
	})
	srv = v1.New(v1.Options{Explorer: ex, Positions: pr, Lending: &stubLendingReader{assets: []string{asset}}, CompletenessReader: verdicts})
	got = getEarnings(t, srv, earningsAccount, http.StatusOK)
	if len(got.Legs) != 1 || got.Legs[0].Protocol != blnt.SourceName || got.Legs[0].Reconciled ||
		strOrNull(got.Legs[0].Reason) != "rate_behind" {
		t.Fatalf("blnt legs = %+v", got.Legs)
	}
}

// TestLendingAccountEarnings_PoolReadFailureDegrades: a failed lake read withholds that pool's legs as onchain_unread and sets
// marks the response degraded (no-store); it is still a 200. A backstop fold with a NULL amount is withheld as fold_inconsistent.
func TestLendingAccountEarnings_PoolReadFailureDegrades(t *testing.T) {
	pool, asset := mkCStrkey(t, 1), mkCStrkey(t, 2)
	ex := &stubEarningsExplorer{
		stubExplorerReader: &stubExplorerReader{},
		posErr:             errors.New("lake down"),
		backstop: clickhouse.LendingBackstopState{
			Instance: &clickhouse.PoolInstance{Backstop: mkCStrkey(t, 4)}, Token: mkCStrkey(t, 3),
			Tiers: []clickhouse.BackstopTierState{{Balance: &clickhouse.BackstopBalance{Shares: big.NewInt(100), Tokens: big.NewInt(100), Q4W: new(big.Int)}}},
		},
		held: clickhouse.LendingAccountBackstop{Held: map[string]*big.Int{"": big.NewInt(10)}},
	}
	pr := &stubEarningsPositions{
		stubPositionsReader: &stubPositionsReader{
			blend:    []timescale.BlendPositionFold{{Pool: pool, Asset: asset, HasSupplyLeg: true, SupplyNet: "90", SupplyTokens: "100"}},
			backstop: []timescale.BlendBackstopFold{{Pool: pool, SharesNet: "10"}},
		},
		flows: timescale.LendingAccountFlows{BlendBackstop: []timescale.LendingBackstopTokenFold{{Pool: pool, NetDeposited: "8", NullAmounts: 1}}},
	}
	srv := v1.New(v1.Options{Explorer: ex, Positions: pr, Lending: &stubLendingReader{assets: []string{asset}}, CompletenessReader: verifiedLendingHistory()})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lending/accounts/"+earningsAccount+"/earnings", nil))
	var body struct{ Data v1.LendingEarningsView }
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || len(body.Data.Legs) != 2 ||
		!strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	if supply := body.Data.Legs[0]; supply.Reconciled || strOrNull(supply.Reason) != "onchain_unread" || supply.NetFlow != "90" {
		t.Fatalf("supply = %+v", supply)
	}
	if bstop := body.Data.Legs[1]; bstop.Reconciled || strOrNull(bstop.Reason) != "fold_inconsistent" {
		t.Fatalf("backstop = %+v", bstop)
	}
}

// TestLendingAccountEarnings_CappedFoldsAreALowerBound: a position fold and a flows read over the per-account row cap keep
// their rows and are served as a degraded lower bound naming both, not as a 500 or as the complete history.
func TestLendingAccountEarnings_CappedFoldsAreALowerBound(t *testing.T) {
	pool, asset := mkCStrkey(t, 1), mkCStrkey(t, 2)
	ex := &stubEarningsExplorer{stubExplorerReader: &stubExplorerReader{}, positions: clickhouse.LendingAccountPositions{Readable: true}}
	pr := &stubEarningsPositions{
		stubPositionsReader: &stubPositionsReader{
			blend:    []timescale.BlendPositionFold{{Pool: pool, Asset: asset, HasSupplyLeg: true, SupplyNet: "90", SupplyTokens: "100"}},
			blendErr: fmt.Errorf("probe: %w", timescale.ErrPositionsTruncated),
		},
		flows:    timescale.LendingAccountFlows{BlendClaims: []timescale.LendingClaimFold{{Pool: pool, Claimed: "7"}}},
		flowsErr: fmt.Errorf("probe: %w", timescale.ErrPositionsTruncated),
	}
	srv := v1.New(v1.Options{Explorer: ex, Positions: pr, Lending: &stubLendingReader{assets: []string{asset}}, CompletenessReader: verifiedLendingHistory()})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lending/accounts/"+earningsAccount+"/earnings", nil))
	var body struct{ Data v1.LendingEarningsView }
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	got := body.Data
	if len(got.Legs) != 1 || got.Legs[0].NetFlow != "90" || len(got.Emissions) != 1 || got.Emissions[0].Claimed != "7" {
		t.Fatalf("capped rows not kept: legs %+v emissions %+v", got.Legs, got.Emissions)
	}
	degraded := strings.Contains(rec.Header().Get("Cache-Control"), "no-store")
	if !got.LowerBound || !degraded ||
		!strings.Contains(got.CoverageNote, "TRUNCATED: blend positions, emission claims and backstop token flows") {
		t.Fatalf("lower_bound %v degraded %v coverage_note %q", got.LowerBound, degraded, got.CoverageNote)
	}
}
