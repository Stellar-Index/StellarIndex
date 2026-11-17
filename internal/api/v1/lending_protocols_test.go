package v1_test

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"sync/atomic"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/currency"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/supply"
)

const (
	blndAssetID = "BLND-GDJEHTBE6ZHUXSWFI642DCGLUOECLHPF3KSXHPXTSTJ7E3JF6MQ5EZYY"
	blntAssetID = "BLNT-GAP2ACGH3O6TRFAERGM4USJ6XWU3U2BFIK224JPV6NI3CUNEEEGTBTFB"
)

// protocolsExplorer adds a backstop read to the rates fixture: one tier of 100 tokens per pool.
type protocolsExplorer struct {
	*ratesExplorer
	panicPool, failBackstop string
	backstopReads           atomic.Int32
}

func (e *protocolsExplorer) LendingBackstop(_ context.Context, pool string, tiered bool) (clickhouse.LendingBackstopState, error) {
	e.backstopReads.Add(1)
	switch pool {
	case e.panicPool:
		panic("backstop reader bug")
	case e.failBackstop:
		return clickhouse.LendingBackstopState{}, errors.New("lake down")
	}
	st := clickhouse.LendingBackstopState{Instance: &clickhouse.PoolInstance{Backstop: mkBackstopID}}
	tier := clickhouse.BackstopTierState{Balance: &clickhouse.BackstopBalance{Tokens: big.NewInt(100), Shares: big.NewInt(90), Q4W: big.NewInt(0)}}
	if tiered {
		tier.Tier, tier.Token = "FirstLoss", "CBLNTTIERTOKEN"
	} else {
		st.Token = "CBLENDLPTOKEN"
	}
	st.Tiers = []clickhouse.BackstopTierState{tier}
	return st, nil
}

// protocolsLending can mark Blend pools V1 and fail the BLNT pool list.
type protocolsLending struct {
	*ratesLendingReader
	v1Pools      map[string]bool
	blntListFail bool
}

func (r *protocolsLending) BlendPoolVersion(ctx context.Context, pool string) (blend.PoolVersion, error) {
	if r.v1Pools[pool] {
		return blend.PoolV1, nil
	}
	return r.ratesLendingReader.BlendPoolVersion(ctx, pool)
}

func (r *protocolsLending) ListBLNTPools(ctx context.Context) ([]timescale.BlendPoolSummary, error) {
	if r.blntListFail {
		return nil, errors.New("blnt list down")
	}
	return r.ratesLendingReader.ListBLNTPools(ctx)
}

const mkBackstopID = "CBACKSTOP"

// protocolsCase shapes the fixture: clean drops the failing pool and prices every reserve and both tokens.
type protocolsCase struct {
	clean             bool
	noCatalogue       bool
	panicPool         func(ratesFixture) string
	failBackstop      func(ratesFixture) string
	v1Pool            func(ratesFixture) string
	blntListFail      bool
	blntListsBlendOne bool
}

type protocolsFixture struct {
	ratesFixture
	explorer *protocolsExplorer
	srv      *v1.Server
}

func newProtocolsServer(t *testing.T, c protocolsCase) protocolsFixture {
	t.Helper()
	f := newRatesFixture(t)
	prices := map[string]string{"native": "0.3", blndAssetID: "0.05"}
	if c.clean {
		f.lending.pools = f.lending.pools[:2]
		frozen := f.explorer.pools[f.frozenPool]
		frozen.Reserves = append(frozen.Reserves, rateReserve(f.frozenPool, f.otherToken, 10_000_000, 0.20, 0.25, true))
		f.explorer.pools[f.frozenPool] = frozen
		prices[f.otherToken], prices[f.usdc], prices[blntAssetID] = "2", "1", "0.01"
	}
	if c.blntListsBlendOne {
		f.lending.blntPools = append(f.lending.blntPools, timescale.BlendPoolSummary{Pool: f.activePool})
	}
	ex := &protocolsExplorer{ratesExplorer: f.explorer}
	lending := &protocolsLending{ratesLendingReader: f.lending, v1Pools: map[string]bool{}, blntListFail: c.blntListFail}
	for _, pick := range []struct {
		fn  func(ratesFixture) string
		set func(string)
	}{
		{c.panicPool, func(p string) { ex.panicPool = p }},
		{c.failBackstop, func(p string) { ex.failBackstop = p }},
		{c.v1Pool, func(p string) { lending.v1Pools[p] = true }},
	} {
		if pick.fn != nil {
			pick.set(pick.fn(f))
		}
	}
	var cat *currency.Catalogue
	if !c.noCatalogue {
		var err error
		if cat, err = currency.LoadEmbedded(); err != nil {
			t.Fatal(err)
		}
	}
	srv := v1.New(v1.Options{
		Explorer:           ex,
		Lending:            lending,
		Prices:             &assetKeyedPriceReader{usd: prices},
		Supply:             &stubSupplyLooker{hit: true, snap: supply.Supply{CirculatingSupply: big.NewInt(1e14), TotalSupply: big.NewInt(2e14)}},
		VerifiedCurrencies: cat,
	})
	return protocolsFixture{ratesFixture: f, explorer: ex, srv: srv}
}

func getProtocols(t *testing.T, srv *v1.Server) ([]v1.LendingProtocolStats, *http.Response) {
	t.Helper()
	return getProtocolsAt(t, httpTestServer(t, srv).URL)
}

func getProtocolsAt(t *testing.T, base string) ([]v1.LendingProtocolStats, *http.Response) {
	t.Helper()
	resp := mustGet(t, base+"/v1/lending/protocols")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body struct {
		Data []v1.LendingProtocolStats `json:"data"`
	}
	mustDecode(t, resp, &body)
	if len(body.Data) != 2 || body.Data[0].Protocol != "blend" || body.Data[1].Protocol != "blnt" {
		t.Fatalf("protocols = %+v", body.Data)
	}
	return body.Data, resp
}

func hasProtocolExclusion(ex []v1.LendingStatsExclusion, kind, pool, asset, reason string) bool {
	deref := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	for _, e := range ex {
		if e.Kind == kind && deref(e.Pool) == pool && deref(e.Asset) == asset && e.Reason == reason {
			return true
		}
	}
	return false
}

func assertProtocolsNoStore(t *testing.T, resp *http.Response, want bool) {
	t.Helper()
	if got := resp.Header.Get("Cache-Control"); (got == "no-store") != want || got == "" {
		t.Errorf("cache-control = %q, want no-store %v", got, want)
	}
}

// TestLendingProtocols_TotalsEqualReservesViews — each protocol's totals are the exact sums of its pools' /reserves figures.
func TestLendingProtocols_TotalsEqualReservesViews(t *testing.T) {
	f := newProtocolsServer(t, protocolsCase{})
	stats, _ := getProtocols(t, f.srv)
	pools := map[string][]string{"blend": {f.activePool, f.frozenPool}, "blnt": {f.blntPool}}
	for _, st := range stats {
		supplied, borrowed := new(big.Rat), new(big.Rat)
		priced, reserves := 0, 0
		for _, pool := range pools[st.Protocol] {
			view := getReserves(t, f.srv, pool)
			reserves += len(view.OmittedReserves)
			for _, rv := range view.Reserves {
				reserves++
				if rv.SuppliedUSD == nil || rv.BorrowedUSD == nil {
					continue
				}
				s, _ := new(big.Rat).SetString(*rv.SuppliedUSD)
				b, _ := new(big.Rat).SetString(*rv.BorrowedUSD)
				supplied.Add(supplied, s)
				borrowed.Add(borrowed, b)
				priced++
			}
		}
		if st.ReserveCount != reserves {
			t.Errorf("%s: reserve_count = %d, want %d", st.Protocol, st.ReserveCount, reserves)
		}
		if priced == 0 {
			if st.TotalSuppliedUSD != nil || st.TotalBorrowedUSD != nil || st.AvailableUSD != nil {
				t.Errorf("%s: nothing priced but totals %+v", st.Protocol, st)
			}
			continue
		}
		avail := new(big.Rat).Sub(supplied, borrowed)
		if strOrNil(st.TotalSuppliedUSD) != supplied.FloatString(2) || strOrNil(st.TotalBorrowedUSD) != borrowed.FloatString(2) ||
			strOrNil(st.AvailableUSD) != avail.FloatString(2) {
			t.Errorf("%s: totals %v/%v/%v, want %s/%s/%s", st.Protocol, strOrNil(st.TotalSuppliedUSD), strOrNil(st.TotalBorrowedUSD),
				strOrNil(st.AvailableUSD), supplied.FloatString(2), borrowed.FloatString(2), avail.FloatString(2))
		}
	}
	blend := stats[0]
	if strOrNil(blend.TotalSuppliedUSD) != "1800.00" || strOrNil(blend.TotalBorrowedUSD) != "900.00" || strOrNil(blend.AvailableUSD) != "900.00" {
		t.Errorf("blend totals = %+v", blend)
	}
	if blend.PoolCount != 3 || stats[1].PoolCount != 1 {
		t.Errorf("pool counts = %d, %d", blend.PoolCount, stats[1].PoolCount)
	}
	if len(blend.Backstop) != 1 || blend.Backstop[0].BackstopToken != "CBLENDLPTOKEN" || blend.Backstop[0].Tokens != "200" {
		t.Errorf("blend backstop = %+v", blend.Backstop)
	}
	if b := stats[1].Backstop; len(b) != 1 || b[0].BackstopToken != "CBLNTTIERTOKEN" || b[0].Tokens != "100" {
		t.Errorf("blnt backstop = %+v", b)
	}
}

// TestLendingProtocols_CompleteIsNotALowerBound — every pool read and every reserve priced: no flag, a CDN-cacheable response,
// and a warm second request reads no backstop.
func TestLendingProtocols_CompleteIsNotALowerBound(t *testing.T) {
	f := newProtocolsServer(t, protocolsCase{clean: true})
	base := httpTestServer(t, f.srv).URL
	stats, resp := getProtocolsAt(t, base)
	for _, st := range stats {
		if st.LowerBound || st.BackstopLowerBound || len(st.Excluded) != 0 || st.AvailableUSD == nil {
			t.Errorf("%s = %+v", st.Protocol, st)
		}
	}
	assertProtocolsNoStore(t, resp, false)
	reads := f.explorer.backstopReads.Load()
	if reads != 3 {
		t.Errorf("cold backstop reads = %d, want 3", reads)
	}
	getProtocolsAt(t, base)
	if got := f.explorer.backstopReads.Load(); got != reads {
		t.Errorf("warm request read %d backstops", got-reads)
	}
}

// TestLendingProtocols_FailingPoolIsALowerBound — a pool whose reserves cannot be read is named and makes the totals a lower bound.
func TestLendingProtocols_FailingPoolIsALowerBound(t *testing.T) {
	f := newProtocolsServer(t, protocolsCase{})
	stats, resp := getProtocols(t, f.srv)
	if !stats[0].LowerBound || !hasProtocolExclusion(stats[0].Excluded, "pool", f.failPool, "", "reserves_read_failed") {
		t.Errorf("blend = %+v", stats[0])
	}
	assertProtocolsNoStore(t, resp, true)
}

// TestLendingProtocols_UnpricedReserveIsALowerBound — a reserve with no USD value, or no decoded state, is named and left out.
func TestLendingProtocols_UnpricedReserveIsALowerBound(t *testing.T) {
	f := newProtocolsServer(t, protocolsCase{})
	stats, _ := getProtocols(t, f.srv)
	if !stats[0].LowerBound || !hasProtocolExclusion(stats[0].Excluded, "reserve", f.activePool, f.otherToken, "no_usd_value") {
		t.Errorf("blend excluded = %+v", stats[0].Excluded)
	}
	if !hasProtocolExclusion(stats[0].Excluded, "reserve", f.frozenPool, f.otherToken, "no_decoded_reserve_state") {
		t.Errorf("omitted reserve not excluded: %+v", stats[0].Excluded)
	}
	if hasProtocolExclusion(stats[0].Excluded, "reserve", f.activePool, f.xlmSAC, "no_usd_value") {
		t.Errorf("priced XLM reserve excluded: %+v", stats[0].Excluded)
	}
}

// TestLendingProtocols_BLNTListFailureIsALowerBound — a failed BLNT pool list is named on BLNT only.
func TestLendingProtocols_BLNTListFailureIsALowerBound(t *testing.T) {
	f := newProtocolsServer(t, protocolsCase{clean: true, blntListFail: true})
	stats, resp := getProtocols(t, f.srv)
	if !stats[1].LowerBound || !hasProtocolExclusion(stats[1].Excluded, "pool_list", "", "", "pool_list_read_failed") || stats[0].LowerBound {
		t.Errorf("stats = %+v", stats)
	}
	assertProtocolsNoStore(t, resp, true)
}

// TestLendingProtocols_BackstopReadFailure — a failed backstop read flags the backstop sums, not the USD totals, and degrades.
func TestLendingProtocols_BackstopReadFailure(t *testing.T) {
	f := newProtocolsServer(t, protocolsCase{clean: true, failBackstop: func(f ratesFixture) string { return f.activePool }})
	stats, resp := getProtocols(t, f.srv)
	if stats[0].LowerBound || !stats[0].BackstopLowerBound || !hasProtocolExclusion(stats[0].Excluded, "backstop", f.activePool, "", "backstop_read_failed") {
		t.Errorf("blend = %+v", stats[0])
	}
	assertProtocolsNoStore(t, resp, true)
}

// TestLendingProtocols_V1BackstopIsStructural — a V1 pool has no backstop read: only backstop_lower_bound, still cacheable.
func TestLendingProtocols_V1BackstopIsStructural(t *testing.T) {
	f := newProtocolsServer(t, protocolsCase{clean: true, v1Pool: func(f ratesFixture) string { return f.frozenPool }})
	stats, resp := getProtocols(t, f.srv)
	if stats[0].LowerBound || !stats[0].BackstopLowerBound || !hasProtocolExclusion(stats[0].Excluded, "backstop", f.frozenPool, "", "blend_v1_unsupported") {
		t.Errorf("blend = %+v", stats[0])
	}
	assertProtocolsNoStore(t, resp, false)
}

// TestLendingProtocols_PanickingPoolReadIsNamed — a pool whose read panics is excluded, not dropped from every total unnamed.
func TestLendingProtocols_PanickingPoolReadIsNamed(t *testing.T) {
	f := newProtocolsServer(t, protocolsCase{clean: true, panicPool: func(f ratesFixture) string { return f.frozenPool }})
	stats, resp := getProtocols(t, f.srv)
	if !stats[0].LowerBound || stats[0].PoolCount != 2 || !hasProtocolExclusion(stats[0].Excluded, "pool", f.frozenPool, "", "pool_read_failed") {
		t.Errorf("blend = %+v", stats[0])
	}
	assertProtocolsNoStore(t, resp, true)
}

// TestLendingProtocols_PoolOnBothListsCountsOnce — a pool both lists name is summed under Blend only.
func TestLendingProtocols_PoolOnBothListsCountsOnce(t *testing.T) {
	f := newProtocolsServer(t, protocolsCase{clean: true, blntListsBlendOne: true})
	stats, _ := getProtocols(t, f.srv)
	if stats[0].PoolCount != 2 || stats[1].PoolCount != 1 {
		t.Errorf("pool counts = %d, %d", stats[0].PoolCount, stats[1].PoolCount)
	}
}

// TestLendingProtocols_TokenValuation — the token figures are /v1/assets/{id}'s; with no price the market cap is absent.
func TestLendingProtocols_TokenValuation(t *testing.T) {
	f := newProtocolsServer(t, protocolsCase{})
	base := httpTestServer(t, f.srv).URL
	stats, _ := getProtocolsAt(t, base)
	blnd, blntTok := stats[0].Token, stats[1].Token
	if blnd == nil || blntTok == nil {
		t.Fatalf("tokens = %+v, %+v", blnd, blntTok)
	}
	if blnd.AssetID != blndAssetID || blnd.Code != "BLND" || blnd.Issuer == "" || strOrNil(blnd.PriceUSD) == "" ||
		strOrNil(blnd.CirculatingSupply) != "100000000000000" || strOrNil(blnd.TotalSupply) != "200000000000000" {
		t.Errorf("blnd token = %+v", blnd)
	}
	if mc, _ := new(big.Rat).SetString(strOrNil(blnd.MarketCapUSD)); mc == nil || mc.Cmp(big.NewRat(500_000, 1)) != 0 {
		t.Errorf("blnd market_cap_usd = %q, want 500000", strOrNil(blnd.MarketCapUSD))
	}
	resp := mustGet(t, base+"/v1/assets/"+blndAssetID)
	var asset struct {
		Data struct {
			PriceUSD     *string `json:"price_usd"`
			MarketCapUSD *string `json:"market_cap_usd"`
		} `json:"data"`
	}
	mustDecode(t, resp, &asset)
	if strOrNil(asset.Data.PriceUSD) != strOrNil(blnd.PriceUSD) || strOrNil(asset.Data.MarketCapUSD) != strOrNil(blnd.MarketCapUSD) {
		t.Errorf("asset route %+v, protocols %+v", asset.Data, blnd)
	}
	if blntTok.AssetID != blntAssetID || blntTok.PriceUSD != nil || blntTok.MarketCapUSD != nil || blntTok.CirculatingSupply == nil {
		t.Errorf("blnt token = %+v", blntTok)
	}
}

// TestLendingProtocols_TokenNotInCatalogue — with no verified entry the token is absent and named, never guessed.
func TestLendingProtocols_TokenNotInCatalogue(t *testing.T) {
	f := newProtocolsServer(t, protocolsCase{clean: true, noCatalogue: true})
	stats, resp := getProtocols(t, f.srv)
	for _, st := range stats {
		if st.Token != nil || !hasProtocolExclusion(st.Excluded, "token", "", map[string]string{"blend": "BLND", "blnt": "BLNT"}[st.Protocol], "token_not_in_catalogue") || st.LowerBound {
			t.Errorf("%s = %+v", st.Protocol, st)
		}
	}
	assertProtocolsNoStore(t, resp, false)
}
