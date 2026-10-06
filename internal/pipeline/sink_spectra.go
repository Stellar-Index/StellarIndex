// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"log/slog"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/spectra"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// persistSpectraEvent lands one decoded Spectra event into spectra_events
// (migration 0210); a discovery kind also re-derives its spectra_markets row
// in the same transaction. The store refuses a field the kind does not carry.
func persistSpectraEvent(ctx context.Context, logger *slog.Logger, store *timescale.Store, e spectra.Event) error {
	if err := store.InsertSpectraEvent(ctx, spectraRow(e)); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(spectra.SourceName, "spectra_events").Inc()
		logger.Error("insert Spectra event failed",
			"contract_id", e.ContractID, "kind", e.Kind,
			"ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, spectra.SourceName)
	logger.Debug("Spectra event ingested",
		"source", spectra.SourceName, "kind", e.Kind,
		"contract_id", e.ContractID, "ledger", e.Ledger)
	return nil
}

func spectraRow(e spectra.Event) timescale.SpectraEvent {
	return timescale.SpectraEvent{
		ContractID:      e.ContractID,
		Ledger:          e.Ledger,
		LedgerCloseTime: e.ObservedAt,
		TxHash:          e.TxHash,
		OpIndex:         e.OpIndex,
		EventIndex:      e.EventIndex,
		Kind:            timescale.SpectraEventKind(e.Kind),
		Role:            timescale.SpectraRole(e.Role),
		MarketPT:        e.MarketPT,
		Caller:          e.Caller,
		Receiver:        e.Receiver,
		Owner:           e.Owner,
		Maker:           e.Maker,
		OrderID:         e.OrderID,
		IBT:             e.IBT,
		YT:              e.YT,
		DurationSeconds: e.DurationSeconds,
		Shares:          e.Shares,
		VaultShares:     e.VaultShares,
		Assets:          e.Assets,
		Amount:          e.Amount,
		YieldInIBT:      e.YieldInIBT,
	}
}
