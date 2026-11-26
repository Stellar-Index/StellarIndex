package pipeline

import (
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	sushiswap_v3 "github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
)

// TestSushiswapV3ManagerRow: a field the event did not carry maps to "" (NULL), never "0", and u128 values keep full width.
func TestSushiswapV3ManagerRow(t *testing.T) {
	above63, _ := new(big.Int).SetString("340282366920938463463374607431768211455", 10)
	mint := sushiswap_v3.ManagerEvent{Kind: sushiswap_v3.EventMint}
	mint.TokenID, mint.Owner = 9, "G1"
	if r := sushiswapV3ManagerRow(mint, nil); r.Liquidity != "" || r.Amount0 != "" || r.Amount1 != "" || r.Owner != "G1" || r.TokenID != 9 {
		t.Fatalf("mint row = %+v, want owner + token id and NULL amounts", r)
	}

	collect := sushiswap_v3.ManagerEvent{Kind: sushiswap_v3.EventCollect}
	collect.Recipient, collect.HasAmounts = "G2", true
	collect.Amount0, collect.Amount1 = canonical.NewAmount(big.NewInt(0)), canonical.NewAmount(above63)
	r := sushiswapV3ManagerRow(collect, nil)
	if r.Liquidity != "" || r.Amount0 != "0" || r.Amount1 != above63.String() || r.Recipient != "G2" {
		t.Fatalf("collect row = %+v, want a real zero amount0, a full-width amount1 and NULL liquidity", r)
	}
}
