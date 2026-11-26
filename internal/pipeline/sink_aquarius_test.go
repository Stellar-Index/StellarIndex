// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
)

// A concentrated pool's event must reach the store with shares absent, so its liquidity is never stored as LP shares; a
// shares-last event keeps its shares.
func TestAquariusLiquidityRow_concentratedHasNoShares(t *testing.T) {
	amounts := []canonical.Amount{canonical.NewAmount(big.NewInt(37_766_771)), canonical.NewAmount(big.NewInt(8_000))}
	cl := aquariusLiquidityRow(aquarius.LiquidityEvent{
		ContractID: "CABOSWXYBLE2UJKYWITFRICR37D4UOB75CNXIH7VI5ZKKL526GFHXAUI", Action: aquarius.LiquidityDeposit,
		Amounts: amounts, Concentrated: true, Liquidity: canonical.NewAmount(big.NewInt(549_667)),
	})
	if !cl.SharesAbsent || !cl.Shares.IsZero() || cl.Amounts[0].String() != "37766771" {
		t.Errorf("concentrated row = %+v, want SharesAbsent with the token amounts", cl)
	}
	classic := aquariusLiquidityRow(aquarius.LiquidityEvent{
		Action: aquarius.LiquidityDeposit, Amounts: amounts, Shares: canonical.NewAmount(big.NewInt(30)),
	})
	if classic.SharesAbsent || classic.Shares.String() != "30" {
		t.Errorf("shares-last row = %+v, want shares 30 present", classic)
	}
}
