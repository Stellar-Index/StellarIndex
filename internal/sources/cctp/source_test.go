// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package cctp

import (
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// TestDecodeMintAndForward_RealMainnetFixture — golden test from the
// actual lake event that surfaced board #31 (ledger 63098002 class):
// single Symbol topic, body map {amount i128, forward_recipient
// Address, token Address}.
func TestDecodeMintAndForward_RealMainnetFixture(t *testing.T) {
	ev := events.Event{
		ContractID:     "CBZL2IH7F6BIDAA3WBNXYKIXSATJGMSW7K5P5MJ6STX5RXN47TZJDF5T",
		Ledger:         63_098_002,
		TxHash:         "realtx",
		OperationIndex: 0,
		LedgerClosedAt: "2026-07-01T00:00:00Z",
		Topic:          []string{"AAAADwAAABBtaW50X2FuZF9mb3J3YXJk"},
		Value:          "AAAAEQAAAAEAAAADAAAADwAAAAZhbW91bnQAAAAAAAoAAAAAAAAAAAAAAAAAD0HcAAAADwAAABFmb3J3YXJkX3JlY2lwaWVudAAAAAAAABIAAAAAAAAAABObqXCbvnVBU7AkJOc3fgBabdiMIqhM33f+9QxxvjN5AAAADwAAAAV0b2tlbgAAAAAAABIAAAABre/OWa7lKWj3YGHUlMJSW3Vln6QpamX0me8p5WR35JY=",
	}
	if got := Classify(&ev); got != EventMintAndForward {
		t.Fatalf("Classify = %q, want %q", got, EventMintAndForward)
	}
	m, err := DecodeMintAndForward(&ev)
	if err != nil {
		t.Fatalf("DecodeMintAndForward: %v", err)
	}
	if m.Amount != "999900" {
		t.Errorf("Amount = %q, want 999900", m.Amount)
	}
	if m.ForwardRecipient != "GAJZXKLQTO7HKQKTWASCJZZXPYAFU3OYRQRKQTG7O77PKDDRXYZXTPI3" {
		t.Errorf("ForwardRecipient = %q", m.ForwardRecipient)
	}
	if m.Token == "" || m.Token[0] != 'C' {
		t.Errorf("Token = %q, want a C-strkey contract address", m.Token)
	}
	out := eventFromMintAndForward(m, time.Now().UTC())
	if out.EventType != EventMintAndForward || out.Amount != "999900" {
		t.Errorf("projection: type=%q amount=%q", out.EventType, out.Amount)
	}
}
