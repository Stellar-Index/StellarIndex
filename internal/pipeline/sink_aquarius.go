// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"log/slog"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/aquarius"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// persistAquariusLiquidity lands one deposit/withdraw_liquidity into aquarius_liquidity, one row per token position,
// shares on the token_index=0 row (migration 0089) and NULL for a concentrated pool.
func persistAquariusLiquidity(ctx context.Context, logger *slog.Logger, store *timescale.Store, e aquarius.LiquidityEvent) error {
	if err := store.InsertAquariusLiquidity(ctx, aquariusLiquidityRow(e)); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(aquarius.SourceName, "aquarius_liquidity").Inc()
		logger.Error("insert Aquarius liquidity failed",
			"contract_id", e.ContractID, "action", e.Action,
			"ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, aquarius.SourceName)
	logger.Debug("Aquarius liquidity ingested",
		"source", aquarius.SourceName, "action", e.Action,
		"contract_id", e.ContractID, "ledger", e.Ledger,
		"tokens", len(e.Tokens))
	return nil
}

// aquariusLiquidityRow maps a decoded liquidity event to its store row; a concentrated pool's liquidity is not LP shares.
func aquariusLiquidityRow(e aquarius.LiquidityEvent) timescale.AquariusLiquidityEvent {
	return timescale.AquariusLiquidityEvent{
		ContractID:      e.ContractID,
		Ledger:          e.Ledger,
		LedgerCloseTime: e.ObservedAt,
		TxHash:          e.TxHash,
		OpIndex:         e.OpIndex,
		EventIndex:      e.EventIndex,
		Action:          timescale.AquariusLiquidityAction(e.Action),
		Tokens:          e.Tokens,
		Amounts:         e.Amounts,
		Shares:          e.Shares,
		SharesAbsent:    e.Concentrated,
	}
}
