// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"encoding/base64"
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	sdkxdr "github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/completeness"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/sources/defindex"
)

// harvestEventStreamer feeds the lake re-derive path
// (completeness.ReDeriveOutputCountsByKindFromEvents) fixed events, as the CH contract_events
// reader would, so the REAL defindex decoder runs rather than a mock.
type harvestEventStreamer struct{ evs []events.Event }

func (s harvestEventStreamer) StreamContractEvents(
	_ context.Context, from, to uint32, _ []string, _ []string,
	fn func(events.Event) error,
) error {
	for _, ev := range s.evs {
		if ev.Ledger < from || ev.Ledger > to {
			continue
		}
		if err := fn(ev); err != nil {
			return err
		}
	}
	return nil
}

// TestDefindexReconcile_CountsStrategyHarvest: the real harvest event has ScvString topic[0], so topic_0_sym is empty in
// the lake. The reconTarget must list "defindex.strategy.harvest" or SumKinds omits harvests from EXPECTED, giving
// complete=false with phantom gaps.
func TestDefindexReconcile_CountsStrategyHarvest(t *testing.T) {
	const (
		// r1 first-mismatch ledger; the strategy contract from the finding.
		harvestLedger  = uint32(57_485_721)
		strategyID     = "CDPWNUW7UMCSVO36VAJSQHQECISPJLCVPDASKHRC5SEROAAZDUQ5DG2Z"
		servedRowCount = 1 // one harvest flow written to defindex_flows at this ledger
	)

	// Locate the defindex source + its defindex_flows target as the production
	// catalogue actually wires them — not a hand-built copy, so the test cannot
	// agree with a broken catalogue.
	cat, _, err := buildReconciliationCatalogue(testConfigWithAllSources())
	if err != nil {
		t.Fatalf("buildReconciliationCatalogue: %v", err)
	}
	var src reconSource
	found := false
	for _, s := range cat {
		if s.name == "defindex" {
			src, found = s, true
			break
		}
	}
	if !found {
		t.Fatal("defindex source missing from reconciliation catalogue")
	}
	var flowsTarget reconTarget
	for _, tgt := range src.targets {
		if tgt.table == "defindex_flows" {
			flowsTarget = tgt
		}
	}
	if flowsTarget.table != "defindex_flows" {
		t.Fatal("defindex source has no defindex_flows target")
	}

	// A genuine successful strategy harvest as the CH lake reader hands it to the decoder:
	// topic[0]=ScvString("BlendStrategy"), topic[1]=ScvSymbol("harvest"), body {from: Address, amount: i128};
	// price_per_share exists on-chain but is unread (decode-by-name).
	harvest := events.Event{
		Type:                     "contract",
		Ledger:                   harvestLedger,
		LedgerClosedAt:           "2026-06-15T00:00:00Z",
		ContractID:               strategyID,
		OperationIndex:           0,
		EventIndex:               2,
		TxHash:                   "bb84120494620c8145d21bcbf031689bd8cb67f8ccc0924ea386f3add291b9e3",
		InSuccessfulContractCall: true,
		Topic:                    []string{defindex.TopicPrefixStrategy, defindex.TopicSymbolHarvest},
		Value: mustEncodeMapB64(t,
			mapEntrySym(t, "amount", i128ScVal(big.NewInt(915_806))),
			mapEntrySym(t, "from", contractAddrScVal(t, strategyID)),
			mapEntrySym(t, "price_per_share", i128ScVal(big.NewInt(1_002_345))),
		),
	}

	streamer := harvestEventStreamer{evs: []events.Event{harvest}}
	byKind, blind, err := completeness.ReDeriveOutputCountsByKindFromEvents(
		context.Background(), streamer, src.dec, src.contractIDs, src.topic0Syms,
		harvestLedger, harvestLedger,
	)
	if err != nil {
		t.Fatalf("ReDeriveOutputCountsByKindFromEvents: %v", err)
	}
	if blind.Any() {
		t.Fatalf("re-derive was blind on the harvest event: %s", blind.Detail())
	}

	// Sanity: the decoder DOES produce the harvest kind — so the outcome below
	// hinges solely on whether the catalogue sums it, not on a decoder gap.
	if got := byKind["defindex.strategy.harvest"][harvestLedger]; got != 1 {
		t.Fatalf("decoder emitted %d defindex.strategy.harvest output(s) at ledger %d, want 1 — "+
			"the harvest decode itself regressed", got, harvestLedger)
	}

	// The EXPECTED side for defindex_flows, summed over the catalogue's ACTUAL
	// kinds. Without the harvest kind this returns 0; with it, 1.
	expected := completeness.SumKinds(byKind, flowsTarget.kinds...)
	if got := expected[harvestLedger]; got != servedRowCount {
		t.Fatalf("expected defindex_flows rows at ledger %d = %d, want %d — "+
			"the defindex_flows reconTarget kinds %v do not sum the genuine harvest flow, "+
			"so the re-derive undercounts and false-flags the ledger as a projection gap",
			harvestLedger, got, servedRowCount, flowsTarget.kinds)
	}

	// End to end: with the served tier holding exactly the one harvest row, the per-ledger reconcile must find no gap.
	// Without the kind, ReconcileCounts sees expected=0 vs served=1 and reports a phantom-row gap, as in the 974-mismatch
	// verdict.
	served := map[uint32]int{harvestLedger: servedRowCount}
	if gaps := completeness.ReconcileCounts(expected, served); len(gaps) != 0 {
		t.Fatalf("ReconcileCounts flagged %d gap(s) for a correctly-served harvest ledger: %+v — "+
			"expected served==expected once strategy.harvest is counted", len(gaps), gaps)
	}
}

// ─── minimal SCVal builders (self-contained; the defindex package's own test
// helpers are unexported) ─────────────────────────────────────────────────

func i128ScVal(n *big.Int) sdkxdr.ScVal {
	abs := new(big.Int).Set(n)
	if abs.Sign() < 0 {
		abs.Neg(abs)
	}
	b := abs.Bytes()
	for len(b) < 16 {
		b = append([]byte{0}, b...)
	}
	hi := int64(0)
	for i := 0; i < 8; i++ {
		hi = (hi << 8) | int64(b[i])
	}
	lo := uint64(0)
	for i := 8; i < 16; i++ {
		lo = (lo << 8) | uint64(b[i])
	}
	if n.Sign() < 0 {
		hi = ^hi
		lo = ^lo + 1
		if lo == 0 {
			hi++
		}
	}
	return sdkxdr.ScVal{
		Type: sdkxdr.ScValTypeScvI128,
		I128: &sdkxdr.Int128Parts{Hi: sdkxdr.Int64(hi), Lo: sdkxdr.Uint64(lo)},
	}
}

func contractAddrScVal(t *testing.T, cStrkey string) sdkxdr.ScVal {
	t.Helper()
	raw, err := strkey.Decode(strkey.VersionByteContract, cStrkey)
	if err != nil {
		t.Fatalf("strkey.Decode(%q): %v", cStrkey, err)
	}
	var cid sdkxdr.ContractId
	copy(cid[:], raw)
	addr := sdkxdr.ScAddress{Type: sdkxdr.ScAddressTypeScAddressTypeContract, ContractId: &cid}
	return sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvAddress, Address: &addr}
}

func mapEntrySym(t *testing.T, key string, val sdkxdr.ScVal) sdkxdr.ScMapEntry {
	t.Helper()
	sym := sdkxdr.ScSymbol(key)
	return sdkxdr.ScMapEntry{
		Key: sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvSymbol, Sym: &sym},
		Val: val,
	}
}

func mustEncodeMapB64(t *testing.T, entries ...sdkxdr.ScMapEntry) string {
	t.Helper()
	m := sdkxdr.ScMap(entries)
	pm := &m
	sv := sdkxdr.ScVal{Type: sdkxdr.ScValTypeScvMap, Map: &pm}
	raw, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal scval map: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}
