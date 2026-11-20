package v1_test

import (
	"context"
	"math/big"
	"testing"

	v1 "github.com/Stellar-Index/StellarIndex/internal/api/v1"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func bespokeUSDBlock() *timescale.BespokeBlock {
	twoTo64, _ := new(big.Int).SetString("18446744073709551616", 10)
	return &timescale.BespokeBlock{Category: "lending", KPIs: []timescale.BespokeKPI{
		{Label: "Claim events (90d)", Value: "3"},
		{Label: "Emissions claimed (90d)", Value: "1844674407370.9551616", Unit: "BLND", Amount: twoTo64, Token: "BLND"},
		{Label: "Emissions claimed BLNT (90d)", Value: "20000000", Amount: big.NewInt(20_000_000), Token: "BLNT"},
		{Label: "Backstop volume (90d)", Value: "1.0000000", Amount: big.NewInt(10_000_000), Token: "BLND:USDC LP"},
	}}
}

// TestValueBespokeKPIs_PricedUnpricedAndUnpriceable values BLND and BLNT emissions at the protocol token's price exactly above
// 2^63, withholds an unpriced token and an LP-share unit with a reason, and leaves count KPIs without USD fields.
func TestValueBespokeKPIs_PricedUnpricedAndUnpriceable(t *testing.T) {
	srv := newProtocolsServer(t, protocolsCase{}).srv // BLND 0.05, BLNT unpriced
	blk := bespokeUSDBlock()
	out := v1.BespokeFromStoreForTest(blk)
	srv.ValueBespokeKPIsForTest(context.Background(), blk, out)

	count, blnd, blntKPI, lp := out.KPIs[0], out.KPIs[1], out.KPIs[2], out.KPIs[3]
	if count.ValueUSD != nil || count.USDReason != "" || count.PriceUSD != nil {
		t.Errorf("count KPI gained USD fields: %+v", count)
	}
	// 2^64 base units / 10^7 x 0.05 = 92233720368.54775808 USD.
	if strOrNil(blnd.ValueUSD) != "92233720368.55" || strOrNil(blnd.PriceUSD) != "0.05" || blnd.USDReason != "" {
		t.Errorf("BLND emissions = %+v, want value_usd 92233720368.55 at 0.05", blnd)
	}
	if blntKPI.ValueUSD != nil || blntKPI.USDReason != "unpriced" {
		t.Errorf("unpriced BLNT emissions = %+v, want null value_usd + usd_reason unpriced", blntKPI)
	}
	if lp.ValueUSD != nil || lp.USDReason != "unit_not_priceable" {
		t.Errorf("Comet LP backstop volume = %+v, want null value_usd + unit_not_priceable", lp)
	}
}

// TestValueBespokeKPIs_BLNTPriced values BLNT emissions once BLNT has a price.
func TestValueBespokeKPIs_BLNTPriced(t *testing.T) {
	srv := newProtocolsServer(t, protocolsCase{clean: true}).srv // BLNT 0.01
	blk := bespokeUSDBlock()
	out := v1.BespokeFromStoreForTest(blk)
	srv.ValueBespokeKPIsForTest(context.Background(), blk, out)
	if got := out.KPIs[2]; strOrNil(got.ValueUSD) != "0.02" || got.USDReason != "" {
		t.Errorf("BLNT emissions = %+v, want value_usd 0.02 (2 BLNT x 0.01)", got)
	}
}
