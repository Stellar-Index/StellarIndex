// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

func loadReflectorEvent(t *testing.T, path string) events.Event {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var fx struct {
		ContractID     string   `json:"contract_id"`
		Ledger         uint32   `json:"ledger"`
		TxHash         string   `json:"tx_hash"`
		LedgerClosedAt string   `json:"ledger_closed_at"`
		Topics         []string `json:"topics"`
		Value          string   `json:"value"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	return events.Event{
		ContractID:     fx.ContractID,
		Ledger:         fx.Ledger,
		TxHash:         fx.TxHash,
		LedgerClosedAt: fx.LedgerClosedAt,
		Topic:          fx.Topics,
		Value:          fx.Value,
		Type:           "contract",
	}
}
