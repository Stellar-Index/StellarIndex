package v1

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

var liqFillTime = time.Date(2026, 3, 4, 10, 30, 0, 0, time.UTC)

const liqFillLedger = 61_000_000

// liqFillTx is the test fill's transaction; its hash is upper-cased on the fill to prove the lake key is lowercase hex.
var liqFillTx = clickhouse.LendingTxRef{Ledger: liqFillLedger, TxHash: "ab"}

func liqRate(s string) *big.Int { n, _ := new(big.Int).SetString(s, 10); return n }

// liqTestMarks: USDC b_rate 1.1 and XLM d_rate 1.25 stored by the fill's transaction; USDC at 0.90 an hour before the fill and
// 1.50 after it, XLM at 0.30 before and 0.60 after.
func liqTestMarks() map[string]*liqMarks {
	return map[string]*liqMarks{
		"CUSDC": {
			scale:  7,
			prices: []liqPrice{{close: liqFillTime.Add(-time.Hour), price: "0.90"}, {close: liqFillTime.Add(time.Hour), price: "1.50"}},
			rates:  map[clickhouse.LendingTxRef]blend.ReserveData{liqFillTx: {Version: blend.PoolV2, BRate: liqRate("1100000000000"), DRate: liqRate("1000000000000")}},
		},
		"CXLM": {
			scale:  7,
			prices: []liqPrice{{close: liqFillTime.Add(-30 * time.Minute), price: "0.30"}, {close: liqFillTime.Add(30 * time.Minute), price: "0.60"}},
			rates:  map[clickhouse.LendingTxRef]blend.ReserveData{liqFillTx: {Version: blend.PoolV2, BRate: liqRate("1000000000000"), DRate: liqRate("1250000000000")}},
		},
	}
}

// liqTestFill is a Blend V2 user liquidation fill: the filler takes 100 USDC b-tokens and repays 100 XLM d-tokens.
func liqTestFill() timescale.LendingLiquidationFill {
	return timescale.LendingLiquidationFill{
		LendingEventPos: timescale.LendingEventPos{Ts: liqFillTime, Ledger: liqFillLedger, TxHash: "AB"},
		AuctionType:     0, User: "GUSER", Filler: "GFILLER", FillPercent: "100", HasAmounts: true,
		Lot: map[string]string{"CUSDC": "1000000000"}, Bid: map[string]string{"CXLM": "1000000000"},
	}
}

func liqDeref(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// TestLiqValuer_ValuesAtFillTimeAndFillRate: 100 bUSDC x 1.1 = 110 USDC at 0.90 (the bucket before the fill, not the 1.50 after
// it) = 99.00; 100 dXLM x 1.25 = 125 XLM at 0.30 = 37.50; profit 61.50.
func TestLiqValuer_ValuesAtFillTimeAndFillRate(t *testing.T) {
	v := liqValuer{protocol: blend.SourceName, marks: liqTestMarks()}.value(liqTestFill())
	if liqDeref(v.LotUSD) != "99.00" || liqDeref(v.BidUSD) != "37.50" || liqDeref(v.FillerProfitUSD) != "61.50" {
		t.Fatalf("lot %s bid %s profit %s, want 99.00 37.50 61.50", liqDeref(v.LotUSD), liqDeref(v.BidUSD), liqDeref(v.FillerProfitUSD))
	}
	lot := v.Lot[0]
	if liqDeref(lot.Underlying) != "1100000000" || liqDeref(lot.PriceUSD) != "0.90" || lot.PriceAt == nil ||
		!time.Time(*lot.PriceAt).Equal(liqFillTime.Add(-time.Hour)) {
		t.Errorf("lot leg = %+v, want underlying 1100000000 at 0.90 closed an hour before the fill", lot)
	}
}

// TestLiqValuer_MissingPriceIsNullWithReason: a price older than the bound is no price — null with a reason, never 0.
func TestLiqValuer_MissingPriceIsNullWithReason(t *testing.T) {
	marks := liqTestMarks()
	marks["CUSDC"].prices = []liqPrice{{close: liqFillTime.Add(-liquidationPriceMaxAge - time.Minute), price: "0.90"}}
	v := liqValuer{protocol: blend.SourceName, marks: marks}.value(liqTestFill())
	if v.LotUSD != nil || v.FillerProfitUSD != nil || v.LotUSDLowerBound || liqDeref(v.Lot[0].Reason) != liqReasonNoPrice {
		t.Fatalf("lot_usd %s profit %s reason %s, want null, null, %s", liqDeref(v.LotUSD), liqDeref(v.FillerProfitUSD),
			liqDeref(v.Lot[0].Reason), liqReasonNoPrice)
	}
	if liqDeref(v.BidUSD) != "37.50" {
		t.Errorf("bid_usd = %s, want 37.50", liqDeref(v.BidUSD))
	}
}

// TestLiqValuer_RateOnlyFromFillTransaction: a state stored by another transaction — an earlier ledger (no fill accrual) or a
// later transaction in the fill's own ledger — is not the fill's rate; the token amount is still served.
func TestLiqValuer_RateOnlyFromFillTransaction(t *testing.T) {
	for name, other := range map[string]clickhouse.LendingTxRef{
		"earlier ledger":        {Ledger: liqFillLedger - 1, TxHash: "ab"},
		"same ledger, other tx": {Ledger: liqFillLedger, TxHash: "cd"},
	} {
		marks := liqTestMarks()
		marks["CUSDC"].rates = map[clickhouse.LendingTxRef]blend.ReserveData{other: {Version: blend.PoolV2, BRate: liqRate("2000000000000")}}
		v := liqValuer{protocol: blend.SourceName, marks: marks}.value(liqTestFill())
		l := v.Lot[0]
		if l.Underlying != nil || l.USD != nil || liqDeref(l.Reason) != liqReasonRateUnrecorded || l.Amount != "1000000000" {
			t.Fatalf("%s: lot leg = %+v, want amount served, underlying and usd null, %s", name, l, liqReasonRateUnrecorded)
		}
	}
}

// liqTxReserves is a lendingTxReserveReader returning fixed states and recording each call.
type liqTxReserves struct {
	ExplorerReader
	states map[clickhouse.LendingTxReserve]blend.ReserveData
	calls  int
	model  string
	txs    []clickhouse.LendingTxRef
}

func (r *liqTxReserves) LendingReservesAtTxs(_ context.Context, _, model string, _ []string, txs []clickhouse.LendingTxRef) (map[clickhouse.LendingTxReserve]blend.ReserveData, error) {
	r.calls++
	r.model, r.txs = model, txs
	return r.states, nil
}

// TestLiquidationRates_OneBatchedReadKeyedByFillTransaction: two fills in two transactions of one ledger each get the state
// their own transaction stored, from one read for the whole page, under the V2 rate model.
func TestLiquidationRates_OneBatchedReadKeyedByFillTransaction(t *testing.T) {
	second := liqTestFill()
	second.TxHash, second.Ts = "EF", liqFillTime.Add(-time.Minute)
	ef := clickhouse.LendingTxRef{Ledger: liqFillLedger, TxHash: "ef"}
	rr := &liqTxReserves{states: map[clickhouse.LendingTxReserve]blend.ReserveData{
		{LendingTxRef: liqFillTx, Asset: "CUSDC"}: {Version: blend.PoolV2, BRate: liqRate("1100000000000")},
		{LendingTxRef: ef, Asset: "CUSDC"}:        {Version: blend.PoolV2, BRate: liqRate("1200000000000")},
	}}
	s := &Server{Explorer: rr, logger: discardLogger()}
	var flags Flags
	model := liquidationStateModel(blend.SourceName, "")
	marks := s.liquidationMarks(t.Context(), "CPOOL", blend.SourceName, model, []timescale.LendingLiquidationFill{liqTestFill(), second}, &flags)
	if rr.calls != 1 || rr.model != clickhouse.LendingBlendV2 || len(rr.txs) != 2 {
		t.Fatalf("calls %d model %q txs %v, want one V2 read naming both transactions", rr.calls, rr.model, rr.txs)
	}
	m := marks["CUSDC"]
	for tx, want := range map[clickhouse.LendingTxRef]string{liqFillTx: "1100000000", ef: "1200000000"} {
		u, reason := m.underlying(auctionLegBToken, tx, liqRate("1000000000"))
		if u == nil || u.String() != want {
			t.Errorf("tx %v: underlying %v (%s), want %s", tx, u, reason, want)
		}
	}
}

// TestLiqValuer_PartialSideIsLowerBound: one unpriced lot leg leaves the side a lower bound naming it, and no profit.
func TestLiqValuer_PartialSideIsLowerBound(t *testing.T) {
	f := liqTestFill()
	f.Lot = map[string]string{"CUSDC": "1000000000", "CODD": "5"}
	v := liqValuer{protocol: blend.SourceName, marks: liqTestMarks()}.value(f)
	if liqDeref(v.LotUSD) != "99.00" || !v.LotUSDLowerBound || v.FillerProfitUSD != nil {
		t.Fatalf("lot_usd %s lower_bound %v profit %s, want 99.00 true null", liqDeref(v.LotUSD), v.LotUSDLowerBound, liqDeref(v.FillerProfitUSD))
	}
	if v.Lot[0].Asset != "CODD" || v.Lot[0].Reason == nil {
		t.Errorf("lot[0] = %+v, want CODD with a reason", v.Lot[0])
	}
}

// TestLiqValuer_AmountAboveInt64IsExact: a 2^65+1 lot keeps every digit through the rate and the USD product.
func TestLiqValuer_AmountAboveInt64IsExact(t *testing.T) {
	marks := liqTestMarks()
	marks["CUSDC"].rates[liqFillTx] = blend.ReserveData{Version: blend.PoolV2, BRate: liqRate("1000000000000"), DRate: liqRate("1000000000000")}
	marks["CUSDC"].prices = []liqPrice{{close: liqFillTime.Add(-time.Hour), price: "1"}}
	f := liqTestFill()
	f.Lot = map[string]string{"CUSDC": "36893488147419103233"}
	v := liqValuer{protocol: blend.SourceName, marks: marks}.value(f)
	if liqDeref(v.Lot[0].Underlying) != "36893488147419103233" || liqDeref(v.LotUSD) != "3689348814741.91" {
		t.Fatalf("underlying %s lot_usd %s, want 36893488147419103233 and 3689348814741.91", liqDeref(v.Lot[0].Underlying), liqDeref(v.LotUSD))
	}
}

// TestLiqValuer_ExclusionAndV1AndUnprovenKinds: a fill after an excluded bucket is withheld; a V1 fill (no amounts on the wire) is
// unvalued as a whole; a BLNT user liquidation's legs are leg_kind_unknown until its leg kinds are proven.
func TestLiqValuer_ExclusionAndV1AndUnprovenKinds(t *testing.T) {
	marks := liqTestMarks()
	marks["CXLM"].withheldFrom = liqFillTime.Add(-2 * time.Hour)
	v := liqValuer{protocol: blend.SourceName, marks: marks}.value(liqTestFill())
	if v.BidUSD != nil || liqDeref(v.Bid[0].Reason) != liqReasonPriceWithheld {
		t.Errorf("bid = %s %+v, want withheld", liqDeref(v.BidUSD), v.Bid[0])
	}
	f := liqTestFill()
	f.HasAmounts, f.Lot, f.Bid = false, nil, nil
	v = liqValuer{protocol: blend.SourceName, marks: liqTestMarks()}.value(f)
	if v.AmountsRecorded || v.LotUSD != nil || v.BidUSD != nil || liqDeref(v.USDReason) != liqReasonAmountsNotEmitted {
		t.Errorf("V1 fill = %+v, want unvalued as %s", v, liqReasonAmountsNotEmitted)
	}
	v = liqValuer{protocol: blnt.SourceName, marks: liqTestMarks()}.value(liqTestFill())
	if v.LotUSD != nil || liqDeref(v.Lot[0].Reason) != auctionUSDKindUnknown {
		t.Errorf("BLNT fill lot = %s %+v, want %s", liqDeref(v.LotUSD), v.Lot[0], auctionUSDKindUnknown)
	}
}
