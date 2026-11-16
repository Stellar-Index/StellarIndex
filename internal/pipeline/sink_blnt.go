// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"context"
	"log/slog"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/blnt"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// persistBLNTEvent lands one identity-gated BLNT V3 event. It is a lossless event catalogue,
// never a Blend table row or a spot-price observation.
func persistBLNTEvent(ctx context.Context, logger *slog.Logger, store *timescale.Store, e blnt.Event) error {
	if err := store.InsertBLNTEvent(ctx, timescale.BLNTEvent{
		ContractID:      e.ContractID,
		Ledger:          e.Ledger,
		LedgerCloseTime: e.ObservedAt,
		TxHash:          e.TxHash,
		OpIndex:         e.OpIndex,
		EventIndex:      e.EventIndex,
		ContractRole:    string(e.Role),
		EventKind:       e.EventType,
		TopicsXDR:       e.TopicsXDR,
		ValueXDR:        e.ValueXDR,
	}); err != nil {
		obs.SourceInsertErrorsTotal.WithLabelValues(blnt.SourceName, "blnt_events").Inc()
		logger.Error("insert BLNT V3 event failed",
			"contract_id", e.ContractID, "contract_role", e.Role, "event_type", e.EventType,
			"ledger", e.Ledger, "tx_hash", e.TxHash, "err", err)
		return err
	}
	bumpEntryCount(ctx, logger, store, blnt.SourceName)
	logger.Debug("BLNT V3 event ingested",
		"source", blnt.SourceName, "contract_role", e.Role, "event_type", e.EventType,
		"contract_id", e.ContractID, "ledger", e.Ledger)
	return nil
}
