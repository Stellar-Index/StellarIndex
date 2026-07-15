// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package cctp

import (
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Golden fixtures below are VERBATIM real mainnet events pulled from
// the ClickHouse raw lake (ADR-0034) on 2026-07-08 — ROADMAP #89b
// decoder topic-match audit. Each carries its real ledger + tx_hash
// for cross-reference. See docs/protocols/cctp.md for the full
// per-topic ledger range.

// ─── ownership_transfer ──────────────────────────────────────────

// TestDecodeOwnershipTransfer_RealMainnetFixture — ledger 62211157,
// tx b75645d61204eca9fab9d04ba55a7a63c5c12a7bd7f613f180921e714f5226da,
// TokenMessengerMinter. old_owner populated (this is the ONE real
// ownership transfer observed on mainnet for this contract, not a
// bootstrap event — TestDecodeOwnershipTransfer's own fixture is the
// only occurrence per contract).
func TestDecodeOwnershipTransfer_RealMainnetFixture(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     MainnetTokenMessengerMinter,
		Ledger:         62_211_157,
		TxHash:         "b75645d61204eca9fab9d04ba55a7a63c5c12a7bd7f613f180921e714f5226da",
		OperationIndex: 0,
		LedgerClosedAt: "2026-06-01T00:00:00Z",
		Topic:          []string{"AAAADwAAABJvd25lcnNoaXBfdHJhbnNmZXIAAA=="},
		Value:          "AAAAEQAAAAEAAAADAAAADwAAABFsaXZlX3VudGlsX2xlZGdlcgAAAAAAAAMDt+dVAAAADwAAAAluZXdfb3duZXIAAAAAAAASAAAAAAAAAABAeDknCYoLJIRZ4SX2VAWQdCWEHDF2NFzHFPH5YsKpiAAAAA8AAAAJb2xkX293bmVyAAAAAAAAEgAAAAAAAAAAkdpJia/rUGphlYFb2HbNCnrcKE4ZBw6uqQ73rz/g4S0=",
	}
	if got := Classify(ev); got != EventOwnershipTransfer {
		t.Fatalf("Classify = %q, want %q", got, EventOwnershipTransfer)
	}
	got, err := DecodeOwnershipTransfer(ev)
	if err != nil {
		t.Fatalf("DecodeOwnershipTransfer: %v", err)
	}
	if got.LiveUntilLedger != 62_383_957 {
		t.Errorf("LiveUntilLedger = %d, want 62383957", got.LiveUntilLedger)
	}
	if got.NewOwner != "GBAHQOJHBGFAWJEELHQSL5SUAWIHIJMEDQYXMNC4Y4KPD6LCYKUYQV43" {
		t.Errorf("NewOwner = %q", got.NewOwner)
	}
	if got.OldOwner != "GCI5USMJV7VVA2TBSWAVXWDWZUFHVXBIJYMQODVOVEHPPLZ74DQS34NM" {
		t.Errorf("OldOwner = %q", got.OldOwner)
	}
	out := eventFromOwnershipTransfer(got, time.Now().UTC())
	if out.EventType != EventOwnershipTransfer {
		t.Errorf("projection EventType = %q", out.EventType)
	}
	if out.Amount != "" || out.Token != "" || out.CounterpartyDomain != nil {
		t.Error("ownership_transfer should carry no amount/token/domain")
	}
	for _, k := range []string{"live_until_ledger", "new_owner", "old_owner"} {
		if _, ok := out.Attributes[k]; !ok {
			t.Errorf("Attributes missing %q", k)
		}
	}
}

func TestDecodeOwnershipTransfer_MissingBodyField(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Topic: []string{TopicSymbolOwnershipTransfer},
		Value: "AAAAEQAAAAEAAAAA", // empty map — 'live_until_ledger' missing
	}
	_, err := DecodeOwnershipTransfer(ev)
	if err == nil {
		t.Fatal("expected ErrMalformedBody")
	}
	if !errors.Is(err, ErrMalformedBody) {
		t.Errorf("want ErrMalformedBody, got %v", err)
	}
}

func TestDecodeOwnershipTransfer_ShortTopic(t *testing.T) {
	t.Parallel()
	ev := &events.Event{Topic: nil}
	_, err := DecodeOwnershipTransfer(ev)
	if !errors.Is(err, ErrMalformedTopic) {
		t.Errorf("want ErrMalformedTopic, got %v", err)
	}
}

// ─── ownership_transfer_completed ────────────────────────────────

// TestDecodeOwnershipTransferCompleted_RealMainnetFixture — ledger
// 62146641, tx 5b53d56d4950a854bd39e3bc806478fd2aafffa5bcbfb86c19ca51eef8b90b7a,
// MessageTransmitter (the bootstrap acceptance).
func TestDecodeOwnershipTransferCompleted_RealMainnetFixture(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     MainnetMessageTransmitter,
		Ledger:         62_146_641,
		TxHash:         "5b53d56d4950a854bd39e3bc806478fd2aafffa5bcbfb86c19ca51eef8b90b7a",
		LedgerClosedAt: "2026-05-28T00:00:00Z",
		Topic:          []string{"AAAADwAAABxvd25lcnNoaXBfdHJhbnNmZXJfY29tcGxldGVk"},
		Value:          "AAAAEQAAAAEAAAABAAAADwAAAAluZXdfb3duZXIAAAAAAAASAAAAAAAAAACR2kmJr+tQamGVgVvYds0KetwoThkHDq6pDvevP+DhLQ==",
	}
	if got := Classify(ev); got != EventOwnershipTransferCompleted {
		t.Fatalf("Classify = %q, want %q", got, EventOwnershipTransferCompleted)
	}
	got, err := DecodeOwnershipTransferCompleted(ev)
	if err != nil {
		t.Fatalf("DecodeOwnershipTransferCompleted: %v", err)
	}
	if got.NewOwner != "GCI5USMJV7VVA2TBSWAVXWDWZUFHVXBIJYMQODVOVEHPPLZ74DQS34NM" {
		t.Errorf("NewOwner = %q", got.NewOwner)
	}
	out := eventFromOwnershipTransferCompleted(got, time.Now().UTC())
	if out.EventType != EventOwnershipTransferCompleted {
		t.Errorf("projection EventType = %q", out.EventType)
	}
}

func TestDecodeOwnershipTransferCompleted_MissingBodyField(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Topic: []string{TopicSymbolOwnershipTransferCompleted},
		Value: "AAAAEQAAAAEAAAAA", // empty map — 'new_owner' missing
	}
	_, err := DecodeOwnershipTransferCompleted(ev)
	if !errors.Is(err, ErrMalformedBody) {
		t.Errorf("want ErrMalformedBody, got %v", err)
	}
}

// ─── admin_changed ────────────────────────────────────────────────

// TestDecodeAdminChanged_BootstrapVoidOldAdmin — ledger 62146641, tx
// 5b53d56d4950a854bd39e3bc806478fd2aafffa5bcbfb86c19ca51eef8b90b7a,
// MessageTransmitter. old_admin is ScvVoid on the real mainnet
// bootstrap event — the schema-evolution trap this decoder guards
// against (CLAUDE.md "Type-test before MustI128").
func TestDecodeAdminChanged_BootstrapVoidOldAdmin(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     MainnetMessageTransmitter,
		Ledger:         62_146_641,
		TxHash:         "5b53d56d4950a854bd39e3bc806478fd2aafffa5bcbfb86c19ca51eef8b90b7a",
		LedgerClosedAt: "2026-05-28T00:00:00Z",
		Topic:          []string{"AAAADwAAAA1hZG1pbl9jaGFuZ2VkAAAA"},
		Value:          "AAAAEQAAAAEAAAACAAAADwAAAAluZXdfYWRtaW4AAAAAAAASAAAAAAAAAACR2kmJr+tQamGVgVvYds0KetwoThkHDq6pDvevP+DhLQAAAA8AAAAJb2xkX2FkbWluAAAAAAAAAQ==",
	}
	if got := Classify(ev); got != EventAdminChanged {
		t.Fatalf("Classify = %q, want %q", got, EventAdminChanged)
	}
	got, err := DecodeAdminChanged(ev)
	if err != nil {
		t.Fatalf("DecodeAdminChanged: %v", err)
	}
	if got.NewAdmin != "GCI5USMJV7VVA2TBSWAVXWDWZUFHVXBIJYMQODVOVEHPPLZ74DQS34NM" {
		t.Errorf("NewAdmin = %q", got.NewAdmin)
	}
	if got.OldAdmin != "" {
		t.Errorf("OldAdmin = %q, want empty (void)", got.OldAdmin)
	}
}

// TestDecodeAdminChanged_RealTransferPopulatedOldAdmin — ledger
// 62146653, tx f73bb9d181e8a85d46eb0263c3dfc7399a5b1d0cc40c1284dda4c333693b18e9,
// TokenMessengerMinter — the later real reassignment where old_admin
// IS populated (contrast with the bootstrap fixture above).
func TestDecodeAdminChanged_RealTransferPopulatedOldAdmin(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     MainnetTokenMessengerMinter,
		Ledger:         62_225_106,
		TxHash:         "ec5b5bb50d34ef40675a58ed7a39c56f5cdfa262ce17d3074c92b85f29f0e362",
		LedgerClosedAt: "2026-06-01T00:00:00Z",
		Topic:          []string{"AAAADwAAAA1hZG1pbl9jaGFuZ2VkAAAA"},
		Value:          "AAAAEQAAAAEAAAACAAAADwAAAAluZXdfYWRtaW4AAAAAAAASAAAAAAAAAAC8xhLeF5/OQjNgnK4pItnFxfht5Gfrk/bDZVpBNjNzJwAAAA8AAAAJb2xkX2FkbWluAAAAAAAAEgAAAAAAAAAAkdpJia/rUGphlYFb2HbNCnrcKE4ZBw6uqQ73rz/g4S0=",
	}
	got, err := DecodeAdminChanged(ev)
	if err != nil {
		t.Fatalf("DecodeAdminChanged: %v", err)
	}
	if got.NewAdmin != "GC6MMEW6C6P44QRTMCOK4KJC3HC4L6DN4RT6XE7WYNSVUQJWGNZSPSVH" {
		t.Errorf("NewAdmin = %q", got.NewAdmin)
	}
	if got.OldAdmin != "GCI5USMJV7VVA2TBSWAVXWDWZUFHVXBIJYMQODVOVEHPPLZ74DQS34NM" {
		t.Errorf("OldAdmin = %q, want populated", got.OldAdmin)
	}
	out := eventFromAdminChanged(got, time.Now().UTC())
	if out.Attributes["old_admin"] != got.OldAdmin {
		t.Errorf("projection old_admin mismatch")
	}
}

func TestDecodeAdminChanged_MissingBodyField(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Topic: []string{TopicSymbolAdminChanged},
		Value: "AAAAEQAAAAEAAAAA", // empty map — 'new_admin' missing
	}
	_, err := DecodeAdminChanged(ev)
	if !errors.Is(err, ErrMalformedBody) {
		t.Errorf("want ErrMalformedBody, got %v", err)
	}
}

// ─── remote_token_messenger_added ────────────────────────────────

// TestDecodeRemoteTokenMessengerAdded_RealMainnetFixture — ledger
// 62146653, tx f73bb9d181e8a85d46eb0263c3dfc7399a5b1d0cc40c1284dda4c333693b18e9,
// TokenMessengerMinter, domain=0 (Ethereum).
func TestDecodeRemoteTokenMessengerAdded_RealMainnetFixture(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     MainnetTokenMessengerMinter,
		Ledger:         62_146_653,
		TxHash:         "f73bb9d181e8a85d46eb0263c3dfc7399a5b1d0cc40c1284dda4c333693b18e9",
		LedgerClosedAt: "2026-05-28T00:00:00Z",
		Topic:          []string{"AAAADwAAABxyZW1vdGVfdG9rZW5fbWVzc2VuZ2VyX2FkZGVk"},
		Value:          "AAAAEQAAAAEAAAACAAAADwAAAAZkb21haW4AAAAAAAMAAAAAAAAADwAAAA90b2tlbl9tZXNzZW5nZXIAAAAADQAAACAAAAAAAAAAAAAAAAAotaDpxiGlutqlNiGbOiKMgWjPXQ==",
	}
	if got := Classify(ev); got != EventRemoteTokenMessengerAdded {
		t.Fatalf("Classify = %q, want %q", got, EventRemoteTokenMessengerAdded)
	}
	got, err := DecodeRemoteTokenMessengerAdded(ev)
	if err != nil {
		t.Fatalf("DecodeRemoteTokenMessengerAdded: %v", err)
	}
	if got.Domain != 0 {
		t.Errorf("Domain = %d, want 0 (Ethereum)", got.Domain)
	}
	if got.TokenMessenger != "00000000000000000000000028b5a0e9c621a5badaa536219b3a228c8168cf5d" {
		t.Errorf("TokenMessenger = %q", got.TokenMessenger)
	}
	out := eventFromRemoteTokenMessengerAdded(got, time.Now().UTC())
	if out.CounterpartyDomain == nil || *out.CounterpartyDomain != 0 {
		t.Errorf("projection CounterpartyDomain = %v, want 0", out.CounterpartyDomain)
	}
	if out.Token != "" {
		t.Error("remote_token_messenger_added should not promote Token (no Stellar-strkey field)")
	}
}

func TestDecodeRemoteTokenMessengerAdded_MissingBodyField(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Topic: []string{TopicSymbolRemoteTokenMessengerAdded},
		Value: "AAAAEQAAAAEAAAAA", // empty map — 'domain' missing
	}
	_, err := DecodeRemoteTokenMessengerAdded(ev)
	if !errors.Is(err, ErrMalformedBody) {
		t.Errorf("want ErrMalformedBody, got %v", err)
	}
}

// ─── token_pair_linked ────────────────────────────────────────────

// TestDecodeTokenPairLinked_RealMainnetFixture — ledger 62146739, tx
// 88908290a164b45bc8abc6503fca9e4ef56359db22791345987f9b6703633f12,
// TokenMessengerMinter. remote_token decodes to Ethereum USDC
// (0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48).
func TestDecodeTokenPairLinked_RealMainnetFixture(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     MainnetTokenMessengerMinter,
		Ledger:         62_146_739,
		TxHash:         "88908290a164b45bc8abc6503fca9e4ef56359db22791345987f9b6703633f12",
		LedgerClosedAt: "2026-05-28T00:00:00Z",
		Topic:          []string{"AAAADwAAABF0b2tlbl9wYWlyX2xpbmtlZAAAAA=="},
		Value:          "AAAAEQAAAAEAAAADAAAADwAAAAtsb2NhbF90b2tlbgAAAAASAAAAAa3vzlmu5Slo92Bh1JTCUlt1ZZ+kKWpl9JnvKeVkd+SWAAAADwAAAA1yZW1vdGVfZG9tYWluAAAAAAAAAwAAAAAAAAAPAAAADHJlbW90ZV90b2tlbgAAAA0AAAAgAAAAAAAAAAAAAAAAoLhpkcYhizbB0Z1KLp6wzjYG60g=",
	}
	if got := Classify(ev); got != EventTokenPairLinked {
		t.Fatalf("Classify = %q, want %q", got, EventTokenPairLinked)
	}
	got, err := DecodeTokenPairLinked(ev)
	if err != nil {
		t.Fatalf("DecodeTokenPairLinked: %v", err)
	}
	if got.LocalToken != "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75" {
		t.Errorf("LocalToken = %q", got.LocalToken)
	}
	if got.RemoteDomain != 0 {
		t.Errorf("RemoteDomain = %d, want 0 (Ethereum)", got.RemoteDomain)
	}
	if got.RemoteToken != "000000000000000000000000a0b86991c6218b36c1d19d4a2e9eb0ce3606eb48" {
		t.Errorf("RemoteToken = %q", got.RemoteToken)
	}
	out := eventFromTokenPairLinked(got, time.Now().UTC())
	if out.Token != got.LocalToken {
		t.Errorf("projection Token = %q, want LocalToken %q", out.Token, got.LocalToken)
	}
	if out.CounterpartyDomain == nil || *out.CounterpartyDomain != 0 {
		t.Errorf("projection CounterpartyDomain = %v, want 0", out.CounterpartyDomain)
	}
}

func TestDecodeTokenPairLinked_MissingBodyField(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Topic: []string{TopicSymbolTokenPairLinked},
		Value: "AAAAEQAAAAEAAAAA", // empty map — 'local_token' missing
	}
	_, err := DecodeTokenPairLinked(ev)
	if !errors.Is(err, ErrMalformedBody) {
		t.Errorf("want ErrMalformedBody, got %v", err)
	}
}

// ─── Decoder.Decode end-to-end (dispatcher_adapter.go wiring) ────

// TestDecoder_Decode_GovernanceEvents exercises the full Matches +
// Decode path for all 5 governance topics from a known CCTP
// contract, guarding the dispatcher_adapter.go switch statement
// added alongside Classify/decode (a Classify case with no matching
// switch arm would silently fall through to ErrUnknownEvent instead
// of emitting a row).
func TestDecoder_Decode_GovernanceEvents(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	cases := []struct {
		name     string
		topic    string
		data     string
		wantType string
	}{
		{"ownership_transfer", "AAAADwAAABJvd25lcnNoaXBfdHJhbnNmZXIAAA==", "AAAAEQAAAAEAAAADAAAADwAAABFsaXZlX3VudGlsX2xlZGdlcgAAAAAAAAMDt+dVAAAADwAAAAluZXdfb3duZXIAAAAAAAASAAAAAAAAAABAeDknCYoLJIRZ4SX2VAWQdCWEHDF2NFzHFPH5YsKpiAAAAA8AAAAJb2xkX293bmVyAAAAAAAAEgAAAAAAAAAAkdpJia/rUGphlYFb2HbNCnrcKE4ZBw6uqQ73rz/g4S0=", EventOwnershipTransfer},
		{"ownership_transfer_completed", "AAAADwAAABxvd25lcnNoaXBfdHJhbnNmZXJfY29tcGxldGVk", "AAAAEQAAAAEAAAABAAAADwAAAAluZXdfb3duZXIAAAAAAAASAAAAAAAAAACR2kmJr+tQamGVgVvYds0KetwoThkHDq6pDvevP+DhLQ==", EventOwnershipTransferCompleted},
		{"admin_changed", "AAAADwAAAA1hZG1pbl9jaGFuZ2VkAAAA", "AAAAEQAAAAEAAAACAAAADwAAAAluZXdfYWRtaW4AAAAAAAASAAAAAAAAAACR2kmJr+tQamGVgVvYds0KetwoThkHDq6pDvevP+DhLQAAAA8AAAAJb2xkX2FkbWluAAAAAAAAAQ==", EventAdminChanged},
		{"remote_token_messenger_added", "AAAADwAAABxyZW1vdGVfdG9rZW5fbWVzc2VuZ2VyX2FkZGVk", "AAAAEQAAAAEAAAACAAAADwAAAAZkb21haW4AAAAAAAMAAAAAAAAADwAAAA90b2tlbl9tZXNzZW5nZXIAAAAADQAAACAAAAAAAAAAAAAAAAAotaDpxiGlutqlNiGbOiKMgWjPXQ==", EventRemoteTokenMessengerAdded},
		{"token_pair_linked", "AAAADwAAABF0b2tlbl9wYWlyX2xpbmtlZAAAAA==", "AAAAEQAAAAEAAAADAAAADwAAAAtsb2NhbF90b2tlbgAAAAASAAAAAa3vzlmu5Slo92Bh1JTCUlt1ZZ+kKWpl9JnvKeVkd+SWAAAADwAAAA1yZW1vdGVfZG9tYWluAAAAAAAAAwAAAAAAAAAPAAAADHJlbW90ZV90b2tlbgAAAA0AAAAgAAAAAAAAAAAAAAAAoLhpkcYhizbB0Z1KLp6wzjYG60g=", EventTokenPairLinked},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ev := events.Event{
				ContractID:     MainnetTokenMessengerMinter,
				LedgerClosedAt: "2026-05-28T00:00:00Z",
				Topic:          []string{c.topic},
				Value:          c.data,
			}
			if !d.Matches(ev) {
				t.Fatalf("Matches = false for %s from a known CCTP contract", c.name)
			}
			out, err := d.Decode(ev)
			if err != nil {
				t.Fatalf("Decode: %v", err)
			}
			if len(out) != 1 {
				t.Fatalf("Decode emitted %d events, want 1", len(out))
			}
			got, ok := out[0].(Event)
			if !ok {
				t.Fatalf("emitted event is %T, want cctp.Event", out[0])
			}
			if got.EventType != c.wantType {
				t.Errorf("EventType = %q, want %q", got.EventType, c.wantType)
			}
		})
	}
}
