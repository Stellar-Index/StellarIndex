package v1

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/sources/blend"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

var liqFillTime = time.Date(2026, 3, 4, 10, 30, 0, 0, time.UTC)

const liqFillLedger = 61_000_000

func liqRate(s string) *big.Int { n, _ := new(big.Int).SetString(s, 10); return n }

// liqTestMarks: USDC b_rate 1.1 and XLM d_rate 1.25 written in the fill's ledger; USDC at 0.90 an hour before the fill and 1.50
// after it, XLM at 0.30 before and 0.60 after.
func liqTestMarks() map[string]*liqMarks {
	return map[string]*liqMarks{
		"CUSDC": {
			scale:  7,
			prices: []liqPrice{{close: liqFillTime.Add(-time.Hour), price: "0.90"}, {close: liqFillTime.Add(time.Hour), price: "1.50"}},
			rates:  map[uint32]blend.ReserveData{liqFillLedger: {Version: blend.PoolV2, BRate: liqRate("1100000000000"), DRate: liqRate("1000000000000")}},
		},
		"CXLM": {
			scale:  7,
			prices: []liqPrice{{close: liqFillTime.Add(-30 * time.Minute), price: "0.30"}, {close: liqFillTime.Add(30 * time.Minute), price: "0.60"}},
			rates:  map[uint32]blend.ReserveData{liqFillLedger: {Version: blend.PoolV2, BRate: liqRate("1000000000000"), DRate: liqRate("1250000000000")}},
		},
	}
}

// liqTestFill is a Blend V2 user liquidation fill: the filler takes 100 USDC b-tokens and repays 100 XLM d-tokens.
func liqTestFill() timescale.LendingLiquidationFill {
	return timescale.LendingLiquidationFill{
		LendingEventPos: timescale.LendingEventPos{Ts: liqFillTime, Ledger: liqFillLedger, TxHash: "ab"},
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

// TestLiqValuer_RateOnlyFromFillLedger: a reserve state from any other ledger is not the fill's rate; the token amount is still served.
func TestLiqValuer_RateOnlyFromFillLedger(t *testing.T) {
	marks := liqTestMarks()
	marks["CUSDC"].rates = map[uint32]blend.ReserveData{liqFillLedger + 5: {Version: blend.PoolV2, BRate: liqRate("2000000000000")}}
	v := liqValuer{protocol: blend.SourceName, marks: marks}.value(liqTestFill())
	l := v.Lot[0]
	if l.Underlying != nil || l.USD != nil || liqDeref(l.Reason) != liqReasonRateUnrecorded || l.Amount != "1000000000" {
		t.Fatalf("lot leg = %+v, want amount served, underlying and usd null, %s", l, liqReasonRateUnrecorded)
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
	marks["CUSDC"].rates[liqFillLedger] = blend.ReserveData{Version: blend.PoolV2, BRate: liqRate("1000000000000"), DRate: liqRate("1000000000000")}
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
