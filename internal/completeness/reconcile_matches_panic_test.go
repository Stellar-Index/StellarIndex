// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package completeness

import (
	"context"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// matchPanicDecoder panics in MATCHES on rows in one specific ledger and
// behaves normally everywhere else — the [panickingMatcher] shape, but
// drivable through a whole re-derive stream rather than one direct call.
//
// This is what a malformed / upgraded-WASM row does to a real decoder's
// ownership test: Matches type-asserts the topic vector and reads body
// fields, so it panics as readily as Decode does. The projector recovers
// that panic and drops the row; the completeness re-derive exists to make
// the drop VISIBLE, so it must recover it the same way and record the
// ledger as a blind spot — never let it escape and crash the
// compute-completeness run (no snapshot written, every source's verdict
// frozen), and never let the ledger net to zero and read CLEAN.
type matchPanicDecoder struct{ panicLedger uint32 }

func (d matchPanicDecoder) Matches(ev events.Event) bool {
	if ev.Ledger == d.panicLedger {
		panic("matches: nil-map read on upgraded WASM topic vector")
	}
	return ev.ContractID == "MATCH"
}

func (matchPanicDecoder) Decode(events.Event) ([]consumer.Event, error) {
	return []consumer.Event{fakeOutput{}}, nil
}

// TestReDeriveOutputCountsByKindFromEvents_RecoversMatchesPanic drives a
// panicking Matches through the re-derive CALL SITE. TestSafeMatches_PanicBecomesAnError
// calls the helper directly, so it stays green if a refactor inlines
// `dec.Matches(ev)` back into the loop; this test then fails with the panic.
func TestReDeriveOutputCountsByKindFromEvents_RecoversMatchesPanic(t *testing.T) {
	const panicLedger uint32 = 601
	es := fakeEventStreamer{evs: []events.Event{
		evAt(600, "MATCH", 0),
		evAt(panicLedger, "MATCH", 0),
	}}

	byKind, blind, err := ReDeriveOutputCountsByKindFromEvents(
		context.Background(), es, matchPanicDecoder{panicLedger: panicLedger}, nil, nil, 600, 601)
	if err != nil {
		t.Fatalf("ReDeriveOutputCountsByKindFromEvents returned error: %v", err)
	}
	if byKind["trade"][600] != 1 {
		t.Errorf("ledger 600 = %d trade outputs, want 1", byKind["trade"][600])
	}
	if byKind["trade"][panicLedger] != 0 {
		t.Errorf("ledger whose Matches panicked contributed %d outputs, want 0", byKind["trade"][panicLedger])
	}
	if got, want := blind.UndecodableMatched, 1; got != want {
		t.Errorf("UndecodableMatched = %d, want %d", got, want)
	}
	if len(blind.Ledgers) != 1 || blind.Ledgers[0] != panicLedger {
		t.Errorf("Ledgers = %v, want [%d]", blind.Ledgers, panicLedger)
	}
}
