package v1

import (
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

// TestBuildPoolReservesRow_RefusesImplausibleSymbol: the symbol is contract-authored, so a spoofing one is served as none while
// the declared decimals still apply.
func TestBuildPoolReservesRow_RefusesImplausibleSymbol(t *testing.T) {
	st := clickhouse.SoroswapPairState{Pair: "CPAIR", Token0: "CT0", Token1: "CT1", Reserve0: big.NewInt(1000), Reserve1: big.NewInt(2000), Ledger: 9}
	displays := map[string]clickhouse.TokenDisplayMeta{
		"CT0": {Symbol: "USDC‮XLM\nline", Decimals: 6, HasMeta: true},
		"CT1": {Symbol: " yXLM ", Decimals: 7, HasMeta: true},
	}
	row := buildPoolReservesRow(st, displays, true)
	if row.Token0.Symbol != "" || row.Token0.Decimals != 6 {
		t.Errorf("token0 = %+v, want no symbol and decimals 6", row.Token0)
	}
	if row.Token1.Symbol != "yXLM" {
		t.Errorf("token1 symbol = %q, want yXLM", row.Token1.Symbol)
	}
}
