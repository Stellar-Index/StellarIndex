// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// TestCatalogue_SymbolTopic0SourcesRejectStringTopic proves the symbol-only
// lake read counts what the full read counts: a flagged source's decoder must
// reject every ScvString topic[0] the dropped arm could have streamed.
func TestCatalogue_SymbolTopic0SourcesRejectStringTopic(t *testing.T) {
	cfg := testConfigWithAllSources()
	cfg.Supply.WatchedSEP41Contracts = testWatchedSEP41
	cat, _, err := buildReconciliationCatalogue(cfg)
	if err != nil {
		t.Fatalf("buildReconciliationCatalogue: %v", err)
	}
	flagged := map[string]bool{}
	for _, src := range cat {
		if !src.symbolTopic0 {
			continue
		}
		flagged[src.name] = true
		if len(src.factories) > 0 || len(src.topic0Syms) == 0 || len(src.contractIDs) == 0 {
			t.Errorf("%s: symbolTopic0 needs a contract-scoped topic prefilter and no factory walk", src.name)
			continue
		}
		for _, sym := range src.topic0Syms {
			ev := events.Event{Type: "contract", ContractID: src.contractIDs[0], Topic: []string{scval.MustEncodeSymbol(sym)}}
			if !src.dec.Matches(ev) {
				t.Errorf("%s: decoder rejects Symbol topic[0] %q, so this check proves nothing", src.name, sym)
			}
			ev.Topic = []string{scval.MustEncodeString(sym)}
			if src.dec.Matches(ev) {
				t.Errorf("%s: decoder accepts String topic[0] %q; the symbol-only read would undercount it", src.name, sym)
			}
		}
	}
	if !flagged["sep41_supply"] {
		t.Error("sep41_supply must read the lake symbol-only: the String arm reads topics_xdr across every KALE granule")
	}
}
