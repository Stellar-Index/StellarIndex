package v1

import (
	"context"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pricingguard"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend/oracleagg"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

const (
	hpPool     = "CCVTVW2CVA7JLH4ROQGP3CU4T3EXVCK66AZGSM4MUQPXAI4QHCZPOATS"
	hpBackstop = "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7"
	hpA        = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA" // collateral reserve, price 1.0
	hpB        = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75" // debt reserve, price 2.0
	hpLedger   = uint32(1_000)
)

var hpNow = time.Unix(1_800_000_000, 0).UTC()

func hpAccount(t *testing.T, n byte) string {
	t.Helper()
	var raw [32]byte
	raw[31] = n
	a, err := strkey.Encode(strkey.VersionByteAccountID, raw[:])
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// hpPos is an account holding collateral b-tokens in A and d-tokens in B.
func hpPos(acct string, collA, debtB int64) clickhouse.LendingPoolPosition {
	p := clickhouse.LendingPoolPosition{
		Account: acct, Collateral: map[string]*big.Int{}, NonCollateral: map[string]*big.Int{},
		Supply: map[string]*big.Int{}, Borrow: map[string]*big.Int{}, Ledger: hpLedger - 1,
	}
	if collA > 0 {
		p.Collateral[hpA], p.Supply[hpA] = big.NewInt(collA), big.NewInt(collA)
	}
	if debtB > 0 {
		p.Borrow[hpB] = big.NewInt(debtB)
	}
	return p
}

// hpReserve's ledger-entry config is c_factor 0.8; its event-sourced Config (c_factor 1.0) must never value anything.
func hpReserve(asset string, lastTime uint64) clickhouse.BlendReserveState {
	cfg := blend.ReserveConfig{Decimals: 7, CFactor: 8_000_000, LFactor: 10_000_000, Util: 5_000_000, RBase: 100_000, ROne: 500_000, RTwo: 5_000_000, RThree: 15_000_000}
	events := cfg
	events.CFactor = 10_000_000
	return clickhouse.BlendReserveState{
		Asset: asset, Decimals: 7, DecimalsFound: true, Config: &events, LakeConfig: &cfg, ConfigLedger: hpLedger - 20, Ledger: hpLedger - 5,
		Data: blend.ReserveData{
			DRate: big.NewInt(1_000_000_000_000), BRate: big.NewInt(1_000_000_000_000), IRMod: big.NewInt(10_000_000),
			BSupply: new(big.Int), DSupply: new(big.Int), BackstopCredit: new(big.Int), LastTime: lastTime, Version: blend.PoolV2,
		},
	}
}

func hpPrice(raw int64) LendingOraclePrice {
	usd, _ := canonical.NewFiatAsset("USD")
	return LendingOraclePrice{Price: blend.OraclePrice{Price: big.NewInt(raw), Decimals: 7, Timestamp: uint64(hpNow.Unix()) - 60}, Quote: usd}
}

// hpSnapshot is a V2 pool snapshot at hpLedger whose reserve supplies equal the positions' sums, so it reconciles.
func hpSnapshot(accts ...clickhouse.LendingPoolPosition) poolTopAccounts {
	a, b := hpReserve(hpA, uint64(hpNow.Unix())), hpReserve(hpB, uint64(hpNow.Unix()))
	bCfg := *b.LakeConfig
	bCfg.Index = 1 // B is ResList slot 1
	b.LakeConfig = &bCfg
	for _, p := range accts {
		if v := p.Supply[hpA]; v != nil {
			a.Data.BSupply.Add(a.Data.BSupply, v)
		}
		if v := p.Borrow[hpB]; v != nil {
			b.Data.DSupply.Add(b.Data.DSupply, v)
		}
	}
	return poolTopAccounts{
		protocol: "blend", version: blend.PoolV2, instance: &clickhouse.PoolInstance{Backstop: hpBackstop, Ledger: hpLedger - 50},
		positions: clickhouse.LendingPoolPositions{Readable: true, Reserves: []string{hpA, hpB}, Accounts: accts},
		states:    map[string]clickhouse.BlendReserveState{hpA: a, hpB: b},
		health: &poolHealthRead{
			prices:   map[string]LendingOraclePrice{hpA: hpPrice(10_000_000), hpB: hpPrice(20_000_000)},
			basis:    LendingOracleBasis{Oracle: "CORACLE", Program: oracleagg.ProgramV2, Ledger: hpLedger, CloseTime: hpNow},
			auctions: clickhouse.LendingPoolAuctions{Auctions: []clickhouse.LendingAuctionUser{}},
		},
	}
}

// hpDerived is b as the fill caches it: derived once.
func hpDerived(b poolTopAccounts) poolTopAccounts {
	b.health.view, b.health.rows = deriveHealth(hpPool, b)
	return b
}

func hpByAccount(rows []healthRow) map[string]LendingHealthAccount {
	out := map[string]LendingHealthAccount{}
	for _, r := range rows {
		out[r.view.Account] = r.view
	}
	return out
}

// Worked by hand: price A = 1.0 and B = 2.0 at 7 decimals, rates 1.0, c_factor 0.8, l_factor 1.0. Collateral X b-tokens
// in A is floor(X·0.8) in the base; Y d-tokens of B is 2Y. So (1000, 100) is 800/200 = 4.0000; (1000, 500) is 800/1000
// = 0.8000; (0, 10) has zero raw collateral: bad debt, HF 0.0000.
func TestDeriveHealth_ClassesAndCallerObligations(t *testing.T) {
	healthy, liq, bad, auct, noDebt := hpAccount(t, 1), hpAccount(t, 2), hpAccount(t, 3), hpAccount(t, 4), hpAccount(t, 5)
	b := hpSnapshot(hpPos(healthy, 1000, 100), hpPos(liq, 1000, 500), hpPos(bad, 0, 10), hpPos(auct, 1000, 500),
		hpPos(hpBackstop, 1000, 500), hpPos(noDebt, 50, 0))
	b.health.auctions.Auctions = []clickhouse.LendingAuctionUser{{AuctionType: 0, User: auct, Ledger: hpLedger - 3}, {AuctionType: 1, User: liq, Ledger: hpLedger - 3}}
	view, rows := deriveHealth(hpPool, b)
	got := hpByAccount(rows)
	for acct, want := range map[string][2]string{
		healthy: {healthClassHealthy, "4.0000"}, liq: {healthClassLiquidatable, "0.8000"}, bad: {healthClassBadDebt, "0.0000"},
		auct: {healthClassInAuction, "0.8000"}, hpBackstop: {healthClassExempt, "0.8000"},
	} {
		if g := got[acct]; g.Class != want[0] || g.HF == nil || *g.HF != want[1] {
			t.Errorf("%s = %+v, want %v", acct, g, want)
		}
	}
	if _, listed := got[noDebt]; listed {
		t.Error("an account without debt has no health factor and is not listed")
	}
	s := view.Summary
	if s.AccountsTotal != 6 || s.AccountsWithDebt != 5 || s.NoDebt != 1 || s.Healthy != 1 || s.Liquidatable != 1 || s.BadDebt != 1 ||
		s.InAuction != 1 || s.LiquidationExempt != 1 || s.LowerBound || len(s.Excluded) != 0 {
		t.Fatalf("summary = %+v", s)
	}
	// 800·4 collateral; 200 + 1000·3 + 20 liabilities, the backstop's included.
	if s.CollateralBase != "3200" || s.LiabilitiesBase != "3220" {
		t.Fatalf("bases = %s / %s", s.CollateralBase, s.LiabilitiesBase)
	}
	if !got[auct].InAuction || !got[hpBackstop].PoolOrBackstop {
		t.Fatal("flags not set")
	}
	// The backstop is never in a band: [0,1) holds bad, liq and auct (20 + 1000 + 1000), [2,∞) holds healthy.
	if view.Bands[0].Accounts != 3 || view.Bands[0].LiabilitiesBase != "2020" || view.Bands[6].Accounts != 1 || *view.Bands[0].To != "1.00" {
		t.Fatalf("bands = %+v", view.Bands)
	}
	order := []string{}
	for _, r := range rows {
		order = append(order, r.view.Account)
	}
	tie := []string{liq, auct, hpBackstop}
	slices.Sort(tie)
	if want := append(append([]string{bad}, tie...), healthy); !slices.Equal(order, want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
}

// TestDeriveHealth_UnpricedNeverValued: an account that cannot be valued at the ledger is never liquidatable, bad debt
// or in a total, carries no HF, and makes the summary a lower bound naming why.
func TestDeriveHealth_UnpricedNeverValued(t *testing.T) {
	acct := hpAccount(t, 9)
	for _, c := range []struct {
		reason string
		mutate func(*poolTopAccounts)
	}{
		{string(oracleagg.VerdictPriceStale), func(b *poolTopAccounts) {
			b.health.prices[hpA] = LendingOraclePrice{Verdict: oracleagg.VerdictPriceStale}
		}},
		{string(oracleagg.VerdictUnsupported), func(b *poolTopAccounts) {
			b.health.prices[hpB] = LendingOraclePrice{Verdict: oracleagg.VerdictUnsupported}
		}},
		{healthReasonStateNewer, func(b *poolTopAccounts) { b.positions.Accounts[0].Ledger = hpLedger + 1 }},
		{healthReasonStateNewer, func(b *poolTopAccounts) { st := b.states[hpB]; st.Ledger = hpLedger + 1; b.states[hpB] = st }},
		{healthReasonStateNewer, func(b *poolTopAccounts) {
			b.health.auctions.Auctions = []clickhouse.LendingAuctionUser{{AuctionType: 1, User: acct, Ledger: hpLedger + 2}}
		}},
		{healthReasonRateUnread, func(b *poolTopAccounts) { delete(b.states, hpA) }},
		// An event-sourced config is not the ledger-entry config: without the entry the reserve is unread.
		{healthReasonConfigUnread, func(b *poolTopAccounts) { st := b.states[hpA]; st.LakeConfig = nil; b.states[hpA] = st }},
		{healthReasonStateNewer, func(b *poolTopAccounts) { st := b.states[hpB]; st.ConfigLedger = hpLedger + 1; b.states[hpB] = st }},
		{healthReasonConfigUnread, func(b *poolTopAccounts) { // a config naming another ResList slot
			st := b.states[hpB]
			cfg := *st.LakeConfig
			cfg.Index = 0
			st.LakeConfig = &cfg
			b.states[hpB] = st
		}},
		{healthReasonStateNewer, func(b *poolTopAccounts) { b.instance.Ledger = hpLedger + 1 }},
		{healthReasonStateNewer, func(b *poolTopAccounts) { // a fill after the ledger removed the auction entry
			b.health.auctions.Gone = []clickhouse.LendingAuctionUser{{AuctionType: 0, User: acct, Ledger: hpLedger + 1}}
		}},
		{healthReasonPriceTooOld, func(b *poolTopAccounts) {
			p := b.health.prices[hpA]
			p.Price.Timestamp = uint64(hpNow.Unix()) - 86_401
			b.health.prices[hpA] = p
		}},
		{healthReasonFormula, func(b *poolTopAccounts) { b.version, b.protocol = blend.PoolVersionUnknown, "blnt" }},
		{healthReasonOracleUnknown, func(b *poolTopAccounts) { b.health.basis.Oracle = "" }},
	} {
		b := hpSnapshot(hpPos(acct, 1000, 500))
		c.mutate(&b)
		view, rows := deriveHealth(hpPool, b)
		s := view.Summary
		if len(rows) != 1 || rows[0].view.Class != healthClassUnpriced || rows[0].view.HF != nil || *rows[0].view.Reason != c.reason {
			t.Fatalf("%s: rows = %+v", c.reason, rows)
		}
		if s.Liquidatable != 0 || s.BadDebt != 0 || s.Unpriced[c.reason] != 1 || s.CollateralBase != "0" || s.LiabilitiesBase != "0" {
			t.Fatalf("%s: summary = %+v", c.reason, s)
		}
		if !s.LowerBound || !slices.Contains(s.Excluded, LendingHealthExclusion{Reason: c.reason, Accounts: 1}) {
			t.Fatalf("%s: lower bound not named: %+v", c.reason, s)
		}
	}
}

// TestDeriveHealth_AuctionsUnreadNeverLiquidatable: without the auction read, an account the gate admits is indeterminate.
func TestDeriveHealth_AuctionsUnreadNeverLiquidatable(t *testing.T) {
	liq := hpAccount(t, 2)
	b := hpSnapshot(hpPos(liq, 1000, 500))
	b.health.auctionsErr = true
	view, rows := deriveHealth(hpPool, b)
	if rows[0].view.Class != healthClassIndeterminate || *rows[0].view.Reason != healthReasonAuctionsUnread || view.Summary.Liquidatable != 0 {
		t.Fatalf("row = %+v summary = %+v", rows[0].view, view.Summary)
	}
	if !view.Summary.LowerBound {
		t.Fatal("unread auctions must make the counts a lower bound")
	}
}

// TestDeriveHealth_TruncatedServesNoRows: a capped read cannot rank worst first.
func TestDeriveHealth_TruncatedServesNoRows(t *testing.T) {
	b := hpSnapshot(hpPos(hpAccount(t, 1), 1000, 500))
	b.positions.Truncated = true
	view, rows := deriveHealth(hpPool, b)
	if rows != nil || !view.Summary.LowerBound || view.Summary.Excluded[0].Reason != topAccountsReasonTruncated {
		t.Fatalf("rows = %v summary = %+v", rows, view.Summary)
	}
}

// TestDeriveHealth_AccrualBracket: 30 days unaccrued at the reserve's stored rate. An account a hair above the gate could
// cross it once interest accrues, so it is indeterminate; one far from it keeps its class. A V1 pool has no rate bound.
func TestDeriveHealth_AccrualBracket(t *testing.T) {
	near, far := hpAccount(t, 1), hpAccount(t, 2)
	// (1000, 399): 800 / 798, HF 1.0025. (1000, 100): HF 4.0000.
	b := hpSnapshot(hpPos(near, 1000, 399), hpPos(far, 1000, 100))
	late := uint64(hpNow.Unix()) - 30*86_400
	for _, a := range []string{hpA, hpB} {
		st := b.states[a]
		st.Data.LastTime = late
		b.states[a] = st
	}
	view, rows := deriveHealth(hpPool, b)
	got := hpByAccount(rows)
	if g := got[near]; g.Class != healthClassIndeterminate || *g.Reason != healthReasonAccrualPending || *g.HF != "1.0025" {
		t.Fatalf("near = %+v", g)
	}
	if g := got[far]; g.Class != healthClassHealthy {
		t.Fatalf("far = %+v", g)
	}
	if view.Accrual.Reproduced || view.Accrual.PendingSecondsMax != 30*86_400 || view.Summary.Indeterminate != 1 {
		t.Fatalf("accrual = %+v summary = %+v", view.Accrual, view.Summary)
	}
	// The totals are on stored rates, so with interest unaccrued the debt total is a floor.
	if s := view.Summary; !s.LowerBound || !slices.Contains(s.Excluded, LendingHealthExclusion{Reason: healthReasonAccrualPending}) {
		t.Fatalf("unaccrued totals not a named lower bound: %+v", s)
	}
	b.version = blend.PoolV1
	for _, a := range []string{hpA, hpB} {
		st := b.states[a]
		st.Data.Version = blend.PoolV1
		b.states[a] = st
	}
	_, rows = deriveHealth(hpPool, b)
	for _, r := range rows {
		if r.view.Class != healthClassIndeterminate || *r.view.Reason != healthReasonAccrualUnbounded {
			t.Fatalf("v1 %s = %+v", r.view.Account, r.view)
		}
	}
}

// TestDeriveHealth_AccrualBracketPairsTheEnds: collateral and debt both accrue, so the bracket's ends are (collateral up,
// debt stored) and (collateral stored, debt up). Each fixture splits on exactly one end; pairing the ends the other way
// (both up, both stored) would call the first healthy and the second liquidatable.
func TestDeriveHealth_AccrualBracketPairsTheEnds(t *testing.T) {
	const x = int64(1_000_000_000_000)
	acct, filler := hpAccount(t, 1), hpAccount(t, 99)
	build := func(y int64) poolTopAccounts {
		f := clickhouse.LendingPoolPosition{
			Account: filler, Collateral: map[string]*big.Int{hpA: big.NewInt(9 * x)}, NonCollateral: map[string]*big.Int{hpB: big.NewInt(10 * x)},
			Supply: map[string]*big.Int{hpA: big.NewInt(9 * x), hpB: big.NewInt(10 * x)},
			Borrow: map[string]*big.Int{hpA: big.NewInt(5 * x), hpB: big.NewInt(5*x - y)}, Ledger: hpLedger - 1,
		}
		b := hpSnapshot(hpPos(acct, x, y), f)
		for _, a := range []string{hpA, hpB} {
			st := b.states[a]
			st.Data.LastTime = uint64(hpNow.Unix()) - 30*86_400
			b.states[a] = st
		}
		a, d := b.states[hpA], b.states[hpB]
		a.Data.DSupply, d.Data.BSupply = big.NewInt(5*x), big.NewInt(10*x) // both reserves half borrowed: both rates move
		b.states[hpA], b.states[hpB] = a, d
		return b
	}
	ends := func(y int64) (cLo, cHi, lLo, lHi *big.Int) {
		b := build(y)
		in := newHealthInputs(hpPool, b)
		p := hpPos(acct, x, y)
		pos := blend.Positions{Collateral: p.Collateral, Liabilities: p.Borrow, Supply: p.NonCollateral}
		lo, err1 := blend.AccountHealth(pos, in.reserves, in.prices, blend.PoolV2, in.now)
		hi, err2 := blend.AccountHealth(pos, in.hi, in.prices, blend.PoolV2, in.now)
		if err1 != nil || err2 != nil {
			t.Fatal(err1, err2)
		}
		return lo.CollateralBase, hi.CollateralBase, lo.LiabilityBase, hi.LiabilityBase
	}
	gt := func(a, b *big.Int) bool { return a.Cmp(b) > 0 }
	// Only (stored collateral, accrued debt) is below the gate: L_lo ≤ C_lo < L_hi ≤ C_hi.
	y1 := int64(sort.Search(int(5*x), func(y int) bool { cLo, _, _, lHi := ends(int64(y)); return gt(lHi, cLo) }))
	// Only (accrued collateral, stored debt) is at or above it: C_lo < L_lo ≤ C_hi < L_hi.
	y2 := int64(sort.Search(int(5*x), func(y int) bool { cLo, _, lLo, _ := ends(int64(y)); return gt(lLo, cLo) }))
	if cLo, cHi, lLo, lHi := ends(y1); gt(lLo, cLo) || gt(lHi, cHi) || !gt(lHi, cLo) {
		t.Fatalf("fixture 1 y=%d: C %s..%s L %s..%s", y1, cLo, cHi, lLo, lHi)
	}
	if cLo, cHi, lLo, lHi := ends(y2); !gt(lLo, cLo) || gt(lLo, cHi) || !gt(lHi, cHi) {
		t.Fatalf("fixture 2 y=%d: C %s..%s L %s..%s", y2, cLo, cHi, lLo, lHi)
	}
	for _, y := range []int64{y1, y2} {
		_, rows := deriveHealth(hpPool, build(y))
		if g := hpByAccount(rows)[acct]; g.Class != healthClassIndeterminate || g.Reason == nil || *g.Reason != healthReasonAccrualPending {
			t.Fatalf("y=%d: %+v", y, g)
		}
	}
}

// TestDeriveHealth_FactorsAndRoundingAtOracleDecimals14: A has c_factor 0.8 / l_factor 0.9, B 0.6 / 0.7; prices at 14
// decimals. Valued by hand (collateral b·c floored, debt d/l ceiled, then price·e/1e7):
//
//	swap: 1000 A against 275 B at 2.0: C = 8e9·1e7 = 8e16; L = 2e7·ceil(2.75e9/0.7 = 3928571428.57) = 78571428580000000,
//	      HF 1.0181, healthy. Applying l to collateral and c to debt gives 9e16 against 2e7·4583333334: liquidatable.
//	round: 14 A against 8 B at 1.0 (stroops): C = 1e7·floor(11.2) = 1.1e8; L = 1e7·ceil(8/0.7 = 11.43) = 1.2e8, HF 0.9166,
//	      liquidatable. Flooring the debt (11) or ceiling the collateral (12) gives L = C: healthy.
func TestDeriveHealth_FactorsAndRoundingAtOracleDecimals14(t *testing.T) {
	px := func(raw int64) LendingOraclePrice {
		p := hpPrice(raw)
		p.Price.Decimals = 14
		return p
	}
	cases := []struct {
		x, y, pB        int64
		class, hf, c, l string
	}{
		{1000 * 10_000_000, 275 * 10_000_000, 2e14, healthClassHealthy, "1.0181", "80000000000000000", "78571428580000000"},
		{14, 8, 1e14, healthClassLiquidatable, "0.9166", "110000000", "120000000"},
	}
	for _, tc := range cases {
		acct := hpAccount(t, 1)
		b := hpSnapshot(hpPos(acct, tc.x, tc.y))
		for a, f := range map[string][2]uint32{hpA: {8_000_000, 9_000_000}, hpB: {6_000_000, 7_000_000}} {
			st := b.states[a]
			cfg := *st.LakeConfig
			cfg.CFactor, cfg.LFactor = f[0], f[1]
			st.LakeConfig = &cfg
			b.states[a] = st
		}
		b.health.prices[hpA], b.health.prices[hpB] = px(1e14), px(tc.pB)
		_, rows := deriveHealth(hpPool, b)
		g := hpByAccount(rows)[acct]
		if g.Class != tc.class || g.HF == nil || *g.HF != tc.hf || *g.CollateralBase != tc.c || *g.LiabilitiesBase != tc.l {
			t.Fatalf("%d/%d = %+v, want %s %s %s/%s", tc.x, tc.y, g, tc.class, tc.hf, tc.c, tc.l)
		}
	}
}

// TestDeriveHealth_CollateralValuedZeroIsNotBadDebt: the contract's bad debt is debt with an empty collateral map; collateral
// that is held but values to zero at the oracle is not it.
func TestDeriveHealth_CollateralValuedZeroIsNotBadDebt(t *testing.T) {
	dust, none := hpAccount(t, 1), hpAccount(t, 2)
	b := hpSnapshot(hpPos(dust, 1, 10), hpPos(none, 0, 10))
	b.health.prices[hpA] = hpPrice(1) // 1 b-token at 1e-7 floors to 0 in the base
	view, rows := deriveHealth(hpPool, b)
	got := hpByAccount(rows)
	if g := got[dust]; g.Class != healthClassIndeterminate || *g.Reason != healthReasonCollateralValueZero {
		t.Fatalf("dust = %+v", g)
	}
	if g := got[none]; g.Class != healthClassBadDebt || view.Summary.BadDebt != 1 {
		t.Fatalf("none = %+v summary %+v", g, view.Summary)
	}
}

// TestDeriveHealth_InstanceUnreadNeverLiquidatable: without the pool instance the backstop is unknown, so no account the
// gate admits may be called liquidatable, and the answer is not cached.
func TestDeriveHealth_InstanceUnreadNeverLiquidatable(t *testing.T) {
	liq := hpAccount(t, 2)
	for reason, mutate := range map[string]func(*poolTopAccounts){
		healthReasonInstanceUnread: func(b *poolTopAccounts) { b.instance = nil },
		healthReasonBackstopUnread: func(b *poolTopAccounts) { b.instance.Backstop = "" },
	} {
		b := hpSnapshot(hpPos(liq, 1000, 500), hpPos(hpBackstop, 1000, 500))
		mutate(&b)
		view, rows := deriveHealth(hpPool, b)
		for _, r := range rows {
			if r.view.Class != healthClassIndeterminate || *r.view.Reason != reason {
				t.Fatalf("%s: %s = %+v", reason, r.view.Account, r.view)
			}
		}
		s := view.Summary
		if s.Liquidatable != 0 || !s.LowerBound || !slices.Contains(s.Excluded, LendingHealthExclusion{Reason: reason, Accounts: 2}) {
			t.Fatalf("%s: summary = %+v", reason, s)
		}
		// A lapsed instance stays lapsed: cached briefly rather than re-read per request, and served no-store.
		_, why := healthBackstop(b)
		b.health.backstopUnknown = why != ""
		if why != reason || b.health.transient() || poolTopAccountsTTL(b) != topAccountsUnreconciledTTL {
			t.Fatalf("%s: why %q ttl %v", reason, why, poolTopAccountsTTL(b))
		}
	}
}

// TestDeriveHealth_SummaryIdentities pins what the OpenAPI text says: state_newer accounts are in neither no_debt nor
// accounts_with_debt, and every other account with debt is in exactly one class or unpriced reason.
func TestDeriveHealth_SummaryIdentities(t *testing.T) {
	newer, liq, unpriced, free := hpAccount(t, 1), hpAccount(t, 2), hpAccount(t, 3), hpAccount(t, 4)
	for name, c := range map[string]struct {
		mutate    func(*poolTopAccounts)
		wantNewer int
	}{
		"position": {func(b *poolTopAccounts) { b.positions.Accounts[0].Ledger = hpLedger + 1 }, 1},
		// newer and liq hold B debt; unpriced holds only A.
		"reserve":  {func(b *poolTopAccounts) { st := b.states[hpB]; st.Ledger = hpLedger + 1; b.states[hpB] = st }, 2},
		"instance": {func(b *poolTopAccounts) { b.instance.Ledger = hpLedger + 1 }, 3},
	} {
		b := hpSnapshot(hpPos(newer, 1000, 100), hpPos(liq, 1000, 500), hpPos(free, 10, 0), hpPos(unpriced, 1000, 100))
		b.positions.Accounts[3].Borrow = map[string]*big.Int{hpA: big.NewInt(10)}
		b.health.prices[hpA] = LendingOraclePrice{Verdict: oracleagg.VerdictPriceStale}
		c.mutate(&b)
		view, _ := deriveHealth(hpPool, b)
		s := view.Summary
		unp := 0
		for _, n := range s.Unpriced {
			unp += n
		}
		classes := s.Healthy + s.Liquidatable + s.BadDebt + s.InAuction + s.Indeterminate + s.LiquidationExempt
		if s.AccountsTotal != s.NoDebt+s.AccountsWithDebt+s.Unpriced[healthReasonStateNewer] ||
			s.AccountsWithDebt != classes+unp-s.Unpriced[healthReasonStateNewer] || s.Unpriced[healthReasonStateNewer] != c.wantNewer {
			t.Fatalf("%s: summary = %+v", name, s)
		}
	}
}

// TestPageHealthRows_StableAcrossTies walks every page by cursor and gets the full order exactly once, ties by address.
func TestPageHealthRows_StableAcrossTies(t *testing.T) {
	var accts []clickhouse.LendingPoolPosition
	for i := byte(1); i <= 7; i++ {
		debt := int64(500) // HF 0.8000, a seven-way tie
		if i%3 == 0 {
			debt = 100
		}
		accts = append(accts, hpPos(hpAccount(t, i), 1000, debt))
	}
	b := hpSnapshot(accts...)
	st := b.states[hpA]
	st.Ledger = hpLedger + 1 // every account holds A: all newer, none priced
	_, all := deriveHealth(hpPool, b)
	b.states[hpA] = st
	_, unpriced := deriveHealth(hpPool, b)
	for _, rows := range [][]healthRow{all, unpriced} {
		var walked []string
		cur := healthCursor{}
		for range 10 {
			page, next := pageHealthRows(rows, cur, 2, hpLedger)
			for _, r := range page {
				walked = append(walked, r.view.Account)
			}
			if next == "" {
				break
			}
			var ok bool
			if cur, ok = parseHealthCursor(next); !ok || cur.ledger != hpLedger {
				t.Fatalf("cursor %q does not parse to ledger %d", next, hpLedger)
			}
		}
		want := []string{}
		for _, r := range rows {
			want = append(want, r.view.Account)
		}
		if !slices.Equal(walked, want) || len(want) != 7 {
			t.Fatalf("walked %v, want %v", walked, want)
		}
		for i := 1; i < len(rows); i++ {
			if compareHealthKey(rows[i-1].c, rows[i-1].l, rows[i-1].view.Account, rows[i].c, rows[i].l, rows[i].view.Account) >= 0 {
				t.Fatalf("rows %d and %d out of order", i-1, i)
			}
		}
	}
	for _, bad := range []string{
		"x", "h.9.1.0." + hpAccount(t, 1), "h.9.-1.2." + hpAccount(t, 1), "n.9.notakey", "h.9.1.2",
		"h.1.2." + hpAccount(t, 1), "n." + hpAccount(t, 1), "n.x." + hpAccount(t, 1),
	} {
		if _, ok := parseHealthCursor(bad); ok {
			t.Errorf("cursor %q accepted", bad)
		}
	}
}

type hpExclusions []timescale.SourceExclusion

func (e hpExclusions) UnliftedSourceExclusions(context.Context) ([]timescale.SourceExclusion, error) {
	return e, nil
}

// TestLendingHealth_ExcludedSourceWithholdsOraclePrice: a reserve price reproduced from an excluded feed source values
// nothing (source_excluded), so its holders are unpriced; prices outside the exclusion are kept.
func TestLendingHealth_ExcludedSourceWithholdsOraclePrice(t *testing.T) {
	usd, _ := canonical.NewFiatAsset("USD")
	excl := hpExclusions{{ID: 1, Source: reflector.SourceCEX, From: hpNow.Add(-time.Hour)}}
	s := &Server{Options: Options{Exclusions: pricingguard.NewSourceExclusionGate(excl, pricingguard.SourceExclusionGateOptions{})}}
	prices := map[string]LendingOraclePrice{hpA: hpPrice(10_000_000), hpB: hpPrice(20_000_000)}
	old := hpPrice(20_000_000)
	old.Price.Timestamp, old.Quote = uint64(hpNow.Add(-2*time.Hour).Unix()), usd
	prices[hpB] = old
	s.withholdExcludedOracle(context.Background(), prices)
	if prices[hpA].Verdict != healthVerdictSourceExcluded || prices[hpB].Verdict != "" {
		t.Fatalf("prices = %+v", prices)
	}
	b := hpSnapshot(hpPos(hpAccount(t, 1), 1000, 500))
	b.health.prices = prices
	view, _ := deriveHealth(hpPool, b)
	if view.Summary.Unpriced[string(healthVerdictSourceExcluded)] != 1 || view.Summary.Liquidatable != 0 {
		t.Fatalf("summary = %+v", view.Summary)
	}
}

// TestLendingHealth_PagesSliceTheFillsRows: a request pages the rows the fill derived, never re-deriving the pool.
func TestLendingHealth_PagesSliceTheFillsRows(t *testing.T) {
	b := hpDerived(hpSnapshot(hpPos(hpAccount(t, 1), 1000, 500)))
	b.positions.Accounts = nil // a per-request derivation would now list nothing
	w := httptest.NewRecorder()
	writeLendingHealth(w, b, healthServe{at: hpNow, limit: 10})
	var body struct {
		Data LendingPoolHealthView `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || len(body.Data.Accounts) != 1 || body.Data.Basis.Ledger != hpLedger || body.Data.ViewChanged {
		t.Fatalf("err %v body %s", err, w.Body)
	}
	// A cursor minted on a view at an older ledger still pages by key, and says the view changed.
	cur, _ := parseHealthCursor("n." + "999." + hpAccount(t, 0))
	w = httptest.NewRecorder()
	writeLendingHealth(w, b, healthServe{at: hpNow, limit: 10, cur: cur})
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || !body.Data.ViewChanged || len(body.Data.Accounts) != 0 {
		t.Fatalf("err %v body %s", err, w.Body)
	}
}

func TestLendingHealth_DisabledIs404(t *testing.T) {
	s := &Server{}
	r := httptest.NewRequest(http.MethodGet, "/v1/lending/pools/"+hpPool+"/health", nil)
	r.SetPathValue("pool", hpPool)
	w := httptest.NewRecorder()
	s.handleLendingPoolHealth(w, r)
	if w.Code != http.StatusNotFound || !json.Valid(w.Body.Bytes()) || !strings.Contains(w.Body.String(), "lending-health-not-enabled") {
		t.Fatalf("code %d body %s", w.Code, w.Body)
	}
}

// TestLendingHealth_TransientIsNoStore: a failed read on our side is served degraded and never stored; a settled verdict
// (the chain's answer) is cacheable.
func TestLendingHealth_TransientIsNoStore(t *testing.T) {
	b := hpSnapshot(hpPos(hpAccount(t, 1), 1000, 100))
	if b.health.transient() || poolTopAccountsTTL(b) != lendingReservesTTL {
		t.Fatal("a settled snapshot is cached for the full TTL")
	}
	b.health.prices[hpA] = LendingOraclePrice{Verdict: oracleagg.VerdictPriceStale}
	if b.health.transient() {
		t.Fatal("a settled verdict is cacheable")
	}
	b.health.prices[hpB] = LendingOraclePrice{Verdict: oracleagg.VerdictReadFailed}
	if !b.health.transient() || poolTopAccountsTTL(b) != topAccountsUnreconciledTTL {
		t.Fatal("oracle_read_failed must not be held for the TTL")
	}
	// Flags.Degraded never reaches the wire; no-store is how a degraded answer is kept out of caches.
	w := httptest.NewRecorder()
	writeLendingHealth(w, hpDerived(b), healthServe{at: hpNow, noStore: true, limit: 10})
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" || !json.Valid(w.Body.Bytes()) {
		t.Fatalf("code %d headers %v", w.Code, w.Header())
	}
	w = httptest.NewRecorder()
	writeLendingHealth(w, hpDerived(hpSnapshot(hpPos(hpAccount(t, 1), 1000, 100))), healthServe{at: hpNow, limit: 10})
	if w.Header().Get("Cache-Control") == "no-store" {
		t.Fatal("a settled snapshot is cacheable")
	}
}
