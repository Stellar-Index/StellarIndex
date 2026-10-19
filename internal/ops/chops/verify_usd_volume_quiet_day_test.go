package chops

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

func TestEmptyTradeDayViolations(t *testing.T) {
	quiet := timescale.TradeDayLedgerEvidence{Ledgers: 17280, First: 1131886, Last: 1149165, EdgesCovered: true}
	cases := []struct {
		name string
		mut  func(*timescale.TradeDayLedgerEvidence)
		want int
	}{
		{"fully ingested tradeless day passes", func(*timescale.TradeDayLedgerEvidence) {}, 0},
		{"no ledgers logged (empty or wrong DB)", func(e *timescale.TradeDayLedgerEvidence) { *e = timescale.TradeDayLedgerEvidence{} }, 1},
		{"interior gap", func(e *timescale.TradeDayLedgerEvidence) { e.Ledgers-- }, 1},
		{"day edge not ingested (writer behind)", func(e *timescale.TradeDayLedgerEvidence) { e.EdgesCovered = false }, 1},
		{"chain shows trades the table lacks", func(e *timescale.TradeDayLedgerEvidence) { e.ClassicTradeEffects = 3 }, 1},
		{"trade rows exist but the valuation read saw none", func(e *timescale.TradeDayLedgerEvidence) { e.TradeRowsExist = true }, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := quiet
			tc.mut(&ev)
			if got := emptyTradeDayViolations(ev); got != tc.want {
				t.Fatalf("emptyTradeDayViolations(%+v) = %d, want %d", ev, got, tc.want)
			}
		})
	}
}
