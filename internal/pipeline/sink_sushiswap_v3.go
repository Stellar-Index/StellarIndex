package pipeline

import (
	"context"
	"log/slog"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	sushiswap_v3 "github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// sushiswapV3ManagerRow maps a decoded manager event to its row; a field the event did not carry stays "" (SQL NULL).
func sushiswapV3ManagerRow(e sushiswap_v3.ManagerEvent, txHash []byte) timescale.SushiswapV3ManagerEvent {
	row := timescale.SushiswapV3ManagerEvent{
		Manager:         e.ContractID,
		Ledger:          e.Ledger,
		LedgerCloseTime: e.ObservedAt,
		TxHash:          txHash,
		OpIndex:         int16(e.OpIndex),
		EventIndex:      int16(e.EventIndex),
		Kind:            e.Kind,
		TokenID:         e.TokenID,
		Owner:           e.Owner,
		Recipient:       e.Recipient,
	}
	if e.HasLiquidity {
		row.Liquidity = e.Liquidity.String()
	}
	if e.HasAmounts {
		row.Amount0, row.Amount1 = e.Amount0.String(), e.Amount1.String()
	}
	return row
}

func persistSushiswapV3Manager(ctx context.Context, logger *slog.Logger, store *timescale.Store, e sushiswap_v3.ManagerEvent) error {
	const table = "sushiswap_v3_manager_events"
	txHash, err := timescale.DecodeSoroswapTxHash(e.TxHash)
	if err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(sushiswap_v3.SourceName, table).Inc()
		logger.Error("decode sushiswap_v3 manager tx_hash failed",
			"contract_id", e.ContractID, "ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	row := sushiswapV3ManagerRow(e, txHash)
	if err := store.InsertSushiswapV3ManagerEvent(ctx, row); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(sushiswap_v3.SourceName, table).Inc()
		logger.Error("insert sushiswap_v3 manager event failed",
			"contract_id", e.ContractID, "ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, sushiswap_v3.SourceName)
	logger.Debug("sushiswap_v3 manager event ingested",
		"contract_id", e.ContractID, "ledger", e.Ledger, "kind", e.Kind, "token_id", e.TokenID)
	return nil
}
