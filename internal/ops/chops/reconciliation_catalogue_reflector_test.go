// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/sources/reflector"
)

// TestBuildReconciliationCatalogue_ReflectorDecimals drives a real mainnet
// event per Reflector variant through the catalogue decoder that ch-rebuild
// re-derives with: it must stamp the same configured scale the projector
// does, or a re-derive overwrites correctly scaled oracle_updates rows.
func TestBuildReconciliationCatalogue_ReflectorDecimals(t *testing.T) {
	dir := filepath.Join("..", "..", "..", "test", "fixtures", "reflector", "v6-2026-04-23")
	for _, tc := range []struct {
		source  string
		fixture string
		set     func(*config.ReflectorOracleConfig, string, uint8)
	}{
		{reflector.SourceDEX, "62251160_9322ba2f5c95.json", func(r *config.ReflectorOracleConfig, c string, d uint8) {
			r.DEXContract, r.DEXDecimals = c, d
		}},
		{reflector.SourceCEX, "62251214_7b118512cb3c.json", func(r *config.ReflectorOracleConfig, c string, d uint8) {
			r.CEXContract, r.CEXDecimals = c, d
		}},
		{reflector.SourceFX, "62251211_f59b732d06a5.json", func(r *config.ReflectorOracleConfig, c string, d uint8) {
			r.FXContract, r.FXDecimals = c, d
		}},
	} {
		ev := loadReflectorEvent(t, filepath.Join(dir, tc.fixture))
		for _, want := range []struct {
			configured, emitted uint8
		}{{0, reflector.DefaultDecimals}, {7, 7}} {
			var cfg config.Config
			tc.set(&cfg.Oracle.Reflector, ev.ContractID, want.configured)
			cat, _, err := buildReconciliationCatalogue(cfg)
			if err != nil {
				t.Fatalf("buildReconciliationCatalogue: %v", err)
			}
			var src *reconSource
			for i := range cat {
				if cat[i].name == tc.source {
					src = &cat[i]
				}
			}
			if src == nil {
				t.Fatalf("%s: not in catalogue with its contract configured", tc.source)
			}
			out, err := src.dec.Decode(ev)
			if err != nil {
				t.Fatalf("%s: Decode: %v", tc.source, err)
			}
			if len(out) == 0 {
				t.Fatalf("%s: no rows from a real update", tc.source)
			}
			for _, e := range out {
				u := e.(reflector.UpdateEvent).Update
				if u.Decimals != want.emitted {
					t.Fatalf("%s configured %d: row %s Decimals = %d, want %d",
						tc.source, want.configured, u.Asset, u.Decimals, want.emitted)
				}
			}
		}
	}
}

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
