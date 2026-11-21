package v1_test

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/xdrjson"
)

type stubBLNTAuctionReader struct {
	stubBLNTLendingReader
	auctions []timescale.LendingAuction
}

func (r *stubBLNTAuctionReader) LendingAuctions(context.Context, string, int, timescale.LendingConfigCursor) ([]timescale.LendingAuction, bool, error) {
	return r.auctions, false, nil
}

// TestLendingPoolAuctions_BLNTShareBidWithheld: the mainnet BLNT interest auction bids the BLNT/XLM Comet pool share. With a
// price row present for that share it is still withheld as lp_share_unvalued, because its tier is an LP-share tier; a bid leg in
// the plain XLM tier's token is valued, so the side is a lower bound; a token no tier names is leg_kind_unknown.
func TestLendingPoolAuctions_BLNTShareBidWithheld(t *testing.T) {
	const share = "CA5HX3D47PQZWC2E7VWXCKH4534P2NOJEOIXAWU47MI4QV2FQUYY3FLC" // BLNT/XLM Comet V1.1 pool (docs/protocols/blnt.md)
	xlmSAC, ok := xdrjson.SACContractID("native", "Public Global Stellar Network ; September 2015")
	if !ok {
		t.Fatal("native SAC")
	}
	stray := mkCStrkey(t, 9)
	unknown := blend.PoolVersionUnknown
	ex := &stubBackstopExplorer{stubExplorerReader: &stubExplorerReader{}, state: clickhouse.LendingBackstopState{
		Instance: &clickhouse.PoolInstance{Backstop: blnt.MainnetBackstop},
		Tiers: []clickhouse.BackstopTierState{
			{Tier: "FirstLoss", Asset: "Xlm", Token: xlmSAC},
			{Tier: "SecondLoss", Asset: "BlntXlm", Token: share},
		},
	}}
	lr := &stubBLNTAuctionReader{
		stubBLNTLendingReader: stubBLNTLendingReader{stubLendingReader: stubLendingReader{version: &unknown}, blntAssets: []string{mkCStrkey(t, 3)}},
		auctions: []timescale.LendingAuction{{
			LendingEventPos: timescale.LendingEventPos{Ts: time.Unix(1_700_000_000, 0).UTC(), Ledger: 10, TxHash: "aa"}, AuctionType: 2,
			User: mkCStrkey(t, 6), Bid: map[string]string{share: "3073138401", xlmSAC: "10000000", stray: "1"}, Lot: map[string]string{},
		}},
	}
	srv := v1.New(v1.Options{
		Explorer: ex, Lending: lr,
		Prices: &assetKeyedPriceReader{usd: map[string]string{"native": "0.3", share: "5"}},
	})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lending/pools/"+blnt.MainnetFixedPool+"/auctions", nil))
	var body struct{ Data v1.LendingAuctionsView }
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || len(body.Data.Auctions) != 1 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	a := body.Data.Auctions[0]
	// 1 XLM at 0.3; the share and the stray leg are left out.
	if strOrNil(a.BidUSD) != "0.30" || !a.BidUSDLowerBound {
		t.Fatalf("bid_usd = %v lower_bound = %v, want 0.30 as a lower bound", strOrNil(a.BidUSD), a.BidUSDLowerBound)
	}
	got := map[string]string{}
	for _, l := range a.BidUnvalued {
		got[l.Asset] = l.Kind + "/" + l.Reason
	}
	if len(got) != 2 || got[share] != "backstop_token/lp_share_unvalued" || got[stray] != "backstop_token/leg_kind_unknown" {
		t.Errorf("bid_unvalued = %+v, want the share as lp_share_unvalued and the stray token as leg_kind_unknown", a.BidUnvalued)
	}
}

// stubBLNTReserveBackstopExplorer serves a BLNT pool's backstop and its reserve states as stored.
type stubBLNTReserveBackstopExplorer struct {
	*stubBackstopExplorer
	reserves []clickhouse.BlendReserveState
}

func (e *stubBLNTReserveBackstopExplorer) BLNTPoolReserves(context.Context, string, []string, map[string]blnt.ReserveConfig) (clickhouse.BlendPoolState, error) {
	return clickhouse.BlendPoolState{Reserves: e.reserves}, nil
}

// failedXLMPriceAuction serves one BLNT interest auction on the mainnet fixed pool while every XLM price read fails;
// reserves are the pool's reserve states. It returns the auction and whether the response was served no-store (degraded).
func failedXLMPriceAuction(t *testing.T, reserves []clickhouse.BlendReserveState, blntAssets []string, bid, lot map[string]string) (v1.LendingAuctionView, bool) {
	t.Helper()
	unknown := blend.PoolVersionUnknown
	ex := &stubBLNTReserveBackstopExplorer{reserves: reserves, stubBackstopExplorer: &stubBackstopExplorer{
		stubExplorerReader: &stubExplorerReader{}, state: clickhouse.LendingBackstopState{
			Instance: &clickhouse.PoolInstance{Backstop: blnt.MainnetBackstop},
			Tiers:    []clickhouse.BackstopTierState{{Tier: "FirstLoss", Asset: "Xlm", Token: nativeSAC(t)}},
		},
	}}
	lr := &stubBLNTAuctionReader{
		stubBLNTLendingReader: stubBLNTLendingReader{stubLendingReader: stubLendingReader{version: &unknown}, blntAssets: blntAssets},
		auctions: []timescale.LendingAuction{{
			LendingEventPos: timescale.LendingEventPos{Ts: time.Unix(1_700_000_000, 0).UTC(), Ledger: 10, TxHash: "aa"}, AuctionType: 2,
			User: mkCStrkey(t, 6), Bid: bid, Lot: lot,
		}},
	}
	srv := v1.New(v1.Options{
		Explorer: ex, Lending: lr,
		Prices: &stubPriceReader{errByPair: map[string]error{"native/fiat:USD": errors.New("price store down")}},
	})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/lending/pools/"+blnt.MainnetFixedPool+"/auctions", nil))
	var body struct{ Data v1.LendingAuctionsView }
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil || len(body.Data.Auctions) != 1 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body)
	}
	return body.Data.Auctions[0], rec.Header().Get("Cache-Control") == "no-store"
}

// A backstop-token leg whose price read failed is price_read_failed, not unpriced, and degrades the response.
func TestLendingPoolAuctions_BackstopPriceReadFailedDegrades(t *testing.T) {
	sac := nativeSAC(t)
	a, degraded := failedXLMPriceAuction(t, nil, []string{mkCStrkey(t, 3)}, map[string]string{sac: "10000000"}, map[string]string{})
	if a.BidUSD != nil || len(a.BidUnvalued) != 1 || a.BidUnvalued[0].Reason != "price_read_failed" {
		t.Fatalf("bid_usd = %v bid_unvalued = %+v, want null with the XLM leg price_read_failed", strOrNil(a.BidUSD), a.BidUnvalued)
	}
	if !degraded {
		t.Error("a failed backstop price read did not degrade the auctions response")
	}
}

// A reserve leg whose reserve price read failed leaves the leg unvalued and degrades the response.
func TestLendingPoolAuctions_ReservePriceReadFailedDegrades(t *testing.T) {
	sac := nativeSAC(t)
	reserves := []clickhouse.BlendReserveState{{
		Pool: blnt.MainnetFixedPool, Asset: sac, Decimals: 7, DecimalsFound: true,
		Metrics: blend.ReserveMetrics{SuppliedUnderlying: big.NewInt(1_000 * xlm), BorrowedUnderlying: big.NewInt(0)},
	}}
	a, degraded := failedXLMPriceAuction(t, reserves, []string{sac}, map[string]string{}, map[string]string{sac: "10000000"})
	if a.LotUSD != nil || len(a.LotUnvalued) != 1 || a.LotUnvalued[0].Asset != sac || a.LotUnvalued[0].Reason != "unpriced" {
		t.Fatalf("lot_usd = %v lot_unvalued = %+v, want null with the read XLM reserve's leg unpriced", strOrNil(a.LotUSD), a.LotUnvalued)
	}
	if !degraded {
		t.Error("a failed reserve price read did not degrade the auctions response")
	}
}
