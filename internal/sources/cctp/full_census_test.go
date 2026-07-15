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
// the ClickHouse raw lake (ADR-0034) on 2026-07-09 — ROADMAP #89c full
// topic census: every topic_0_sym the three CCTP contracts have EVER
// emitted (26 distinct topics, 9496 total events, exactly reconciled
// against a plain count() over the same contract set; topics_xdr was
// also checked for the empty-topic_0_sym trap — none found, every
// CCTP topic is a Symbol). This file covers the 16 topics that were
// still undecoded after governance_test.go's #89b pass. Each carries
// its real ledger + tx_hash for cross-reference. See
// docs/protocols/cctp.md for the full per-topic ledger range.

// ─── admin_change_started ──────────────────────────────────────────

// TestDecodeAdminChangeStarted_RealMainnetFixture — ledger 62211158,
// tx b2993ff4713de9dd111d5dc64adacb500dae7bf86d037997964876cc3ebddc92,
// TokenMessengerMinter. The 2-step counterpart to admin_changed: this
// fires when a change is INITIATED. old_admin populated in all three
// observed instances (one per contract) but still type-tested.
func TestDecodeAdminChangeStarted_RealMainnetFixture(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     MainnetTokenMessengerMinter,
		Ledger:         62_211_158,
		TxHash:         "b2993ff4713de9dd111d5dc64adacb500dae7bf86d037997964876cc3ebddc92",
		LedgerClosedAt: "2026-04-20T21:53:15Z",
		Topic:          []string{"AAAADwAAABRhZG1pbl9jaGFuZ2Vfc3RhcnRlZA=="},
		Value:          "AAAAEQAAAAEAAAACAAAADwAAAAluZXdfYWRtaW4AAAAAAAASAAAAAAAAAAC8xhLeF5/OQjNgnK4pItnFxfht5Gfrk/bDZVpBNjNzJwAAAA8AAAAJb2xkX2FkbWluAAAAAAAAEgAAAAAAAAAAkdpJia/rUGphlYFb2HbNCnrcKE4ZBw6uqQ73rz/g4S0=",
	}
	if got := Classify(ev); got != EventAdminChangeStarted {
		t.Fatalf("Classify = %q, want %q", got, EventAdminChangeStarted)
	}
	got, err := DecodeAdminChangeStarted(ev)
	if err != nil {
		t.Fatalf("DecodeAdminChangeStarted: %v", err)
	}
	if got.NewAdmin != "GC6MMEW6C6P44QRTMCOK4KJC3HC4L6DN4RT6XE7WYNSVUQJWGNZSPSVH" {
		t.Errorf("NewAdmin = %q", got.NewAdmin)
	}
	if got.OldAdmin != "GCI5USMJV7VVA2TBSWAVXWDWZUFHVXBIJYMQODVOVEHPPLZ74DQS34NM" {
		t.Errorf("OldAdmin = %q", got.OldAdmin)
	}
	out := eventFromAdminChangeStarted(got, time.Now().UTC())
	if out.EventType != EventAdminChangeStarted {
		t.Errorf("projection EventType = %q", out.EventType)
	}
	if out.Attributes["new_admin"] != got.NewAdmin || out.Attributes["old_admin"] != got.OldAdmin {
		t.Error("projection Attributes mismatch")
	}
}

func TestDecodeAdminChangeStarted_MissingBodyField(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Topic: []string{TopicSymbolAdminChangeStarted},
		Value: "AAAAEQAAAAEAAAAA", // empty map — 'new_admin' missing
	}
	_, err := DecodeAdminChangeStarted(ev)
	if !errors.Is(err, ErrMalformedBody) {
		t.Errorf("want ErrMalformedBody, got %v", err)
	}
}

func TestDecodeAdminChangeStarted_ShortTopic(t *testing.T) {
	t.Parallel()
	ev := &events.Event{Topic: nil}
	_, err := DecodeAdminChangeStarted(ev)
	if !errors.Is(err, ErrMalformedTopic) {
		t.Errorf("want ErrMalformedTopic, got %v", err)
	}
}

// ─── attester_enabled ───────────────────────────────────────────────

// TestDecodeAttesterEnabled_RealMainnetFixture — ledger 62146641, tx
// 5b53d56d4950a854bd39e3bc806478fd2aafffa5bcbfb86c19ca51eef8b90b7a,
// MessageTransmitter. Only ever observed from MessageTransmitter.
func TestDecodeAttesterEnabled_RealMainnetFixture(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     MainnetMessageTransmitter,
		Ledger:         62_146_641,
		TxHash:         "5b53d56d4950a854bd39e3bc806478fd2aafffa5bcbfb86c19ca51eef8b90b7a",
		LedgerClosedAt: "2026-04-16T14:43:48Z",
		Topic: []string{
			"AAAADwAAABBhdHRlc3Rlcl9lbmFibGVk",
			"AAAADQAAABRyWwb3P/dh71OQ45MV4r+/YNM/lg==",
		},
		Value: "AAAAEQAAAAEAAAAA",
	}
	if got := Classify(ev); got != EventAttesterEnabled {
		t.Fatalf("Classify = %q, want %q", got, EventAttesterEnabled)
	}
	got, err := DecodeAttesterEnabled(ev)
	if err != nil {
		t.Fatalf("DecodeAttesterEnabled: %v", err)
	}
	if got.Attester != "725b06f73ff761ef5390e39315e2bfbf60d33f96" {
		t.Errorf("Attester = %q", got.Attester)
	}
	out := eventFromAttesterEnabled(got, time.Now().UTC())
	if out.EventType != EventAttesterEnabled {
		t.Errorf("projection EventType = %q", out.EventType)
	}
}

func TestDecodeAttesterEnabled_ShortTopic(t *testing.T) {
	t.Parallel()
	ev := &events.Event{Topic: []string{TopicSymbolAttesterEnabled}}
	_, err := DecodeAttesterEnabled(ev)
	if !errors.Is(err, ErrMalformedTopic) {
		t.Errorf("want ErrMalformedTopic, got %v", err)
	}
}

// ─── attester_manager_updated ──────────────────────────────────────

// TestDecodeAttesterManagerUpdated_BootstrapVoidOld — ledger 62146641,
// tx 5b53d56d4950a854bd39e3bc806478fd2aafffa5bcbfb86c19ca51eef8b90b7a,
// MessageTransmitter. old_attester_manager is Void on the ONLY
// observed instance (bootstrap) — the schema-evolution trap this
// decoder type-tests against, this time in a TOPIC field.
func TestDecodeAttesterManagerUpdated_BootstrapVoidOld(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     MainnetMessageTransmitter,
		Ledger:         62_146_641,
		TxHash:         "5b53d56d4950a854bd39e3bc806478fd2aafffa5bcbfb86c19ca51eef8b90b7a",
		LedgerClosedAt: "2026-04-16T14:43:48Z",
		Topic: []string{
			"AAAADwAAABhhdHRlc3Rlcl9tYW5hZ2VyX3VwZGF0ZWQ=",
			"AAAAAQ==",
			"AAAAEgAAAAAAAAAA9bSlyVxVQQmx8tUDobSxGIOsZhZsvmbYUSu91GsAm48=",
		},
		Value: "AAAAEQAAAAEAAAAA",
	}
	if got := Classify(ev); got != EventAttesterManagerUpdated {
		t.Fatalf("Classify = %q, want %q", got, EventAttesterManagerUpdated)
	}
	got, err := DecodeAttesterManagerUpdated(ev)
	if err != nil {
		t.Fatalf("DecodeAttesterManagerUpdated: %v", err)
	}
	if got.OldAttesterManager != "" {
		t.Errorf("OldAttesterManager = %q, want empty (void)", got.OldAttesterManager)
	}
	if got.NewAttesterManager != "GD23JJOJLRKUCCNR6LKQHINUWEMIHLDGCZWL4ZWYKEV33VDLACNY6ZG3" {
		t.Errorf("NewAttesterManager = %q", got.NewAttesterManager)
	}
	out := eventFromAttesterManagerUpdated(got, time.Now().UTC())
	if out.Attributes["old_attester_manager"] != "" {
		t.Error("projection old_attester_manager should be empty")
	}
}

func TestDecodeAttesterManagerUpdated_ShortTopic(t *testing.T) {
	t.Parallel()
	ev := &events.Event{Topic: []string{TopicSymbolAttesterManagerUpdated, "AAAAAQ=="}}
	_, err := DecodeAttesterManagerUpdated(ev)
	if !errors.Is(err, ErrMalformedTopic) {
		t.Errorf("want ErrMalformedTopic, got %v", err)
	}
}

// ─── denylisted / un_denylisted ────────────────────────────────────

// TestDecodeDenylisted_RealMainnetFixture — ledger 62226112, tx
// 3ad9700a50fc94d19ea76ac6ada066d1b91c7869b45ea37f3fcfb487d26fdf70,
// TokenMessengerMinter.
func TestDecodeDenylisted_RealMainnetFixture(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     MainnetTokenMessengerMinter,
		Ledger:         62_226_112,
		TxHash:         "3ad9700a50fc94d19ea76ac6ada066d1b91c7869b45ea37f3fcfb487d26fdf70",
		LedgerClosedAt: "2026-04-21T21:47:03Z",
		Topic: []string{
			"AAAADwAAAApkZW55bGlzdGVkAAA=",
			"AAAAEgAAAAAAAAAAng3s07/WXbZx2tEGgoBNlJi8nX7sQ60t98kRs8dPg1s=",
		},
		Value: "AAAAEQAAAAEAAAAA",
	}
	if got := Classify(ev); got != EventDenylisted {
		t.Fatalf("Classify = %q, want %q", got, EventDenylisted)
	}
	got, err := DecodeDenylisted(ev)
	if err != nil {
		t.Fatalf("DecodeDenylisted: %v", err)
	}
	if got.Account != "GCPA33GTX7LF3NTR3LIQNAUAJWKJRPE5P3WEHLJN67ERDM6HJ6BVWJVP" {
		t.Errorf("Account = %q", got.Account)
	}
	out := eventFromDenylisted(got, time.Now().UTC())
	if out.EventType != EventDenylisted {
		t.Errorf("projection EventType = %q", out.EventType)
	}
}

func TestDecodeDenylisted_ShortTopic(t *testing.T) {
	t.Parallel()
	ev := &events.Event{Topic: []string{TopicSymbolDenylisted}}
	_, err := DecodeDenylisted(ev)
	if !errors.Is(err, ErrMalformedTopic) {
		t.Errorf("want ErrMalformedTopic, got %v", err)
	}
}

// TestDecodeUnDenylisted_RealMainnetFixture — ledger 62226574, tx
// b71dfa79ba940eb5679115002ff906522db2f4bf60220ed05bad7bc0d782762c,
// TokenMessengerMinter — the SAME account as [TestDecodeDenylisted_RealMainnetFixture],
// a denylist/un-denylist pair.
func TestDecodeUnDenylisted_RealMainnetFixture(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     MainnetTokenMessengerMinter,
		Ledger:         62_226_574,
		TxHash:         "b71dfa79ba940eb5679115002ff906522db2f4bf60220ed05bad7bc0d782762c",
		LedgerClosedAt: "2026-04-21T22:31:16Z",
		Topic: []string{
			"AAAADwAAAA11bl9kZW55bGlzdGVkAAAA",
			"AAAAEgAAAAAAAAAAng3s07/WXbZx2tEGgoBNlJi8nX7sQ60t98kRs8dPg1s=",
		},
		Value: "AAAAEQAAAAEAAAAA",
	}
	if got := Classify(ev); got != EventUnDenylisted {
		t.Fatalf("Classify = %q, want %q", got, EventUnDenylisted)
	}
	got, err := DecodeUnDenylisted(ev)
	if err != nil {
		t.Fatalf("DecodeUnDenylisted: %v", err)
	}
	if got.Account != "GCPA33GTX7LF3NTR3LIQNAUAJWKJRPE5P3WEHLJN67ERDM6HJ6BVWJVP" {
		t.Errorf("Account = %q", got.Account)
	}
	out := eventFromUnDenylisted(got, time.Now().UTC())
	if out.EventType != EventUnDenylisted {
		t.Errorf("projection EventType = %q", out.EventType)
	}
}

func TestDecodeUnDenylisted_ShortTopic(t *testing.T) {
	t.Parallel()
	ev := &events.Event{Topic: []string{TopicSymbolUnDenylisted}}
	_, err := DecodeUnDenylisted(ev)
	if !errors.Is(err, ErrMalformedTopic) {
		t.Errorf("want ErrMalformedTopic, got %v", err)
	}
}

// ─── denylister_changed ────────────────────────────────────────────

// TestDecodeDenylisterChanged_BootstrapVoidOld — ledger 62146653, tx
// f73bb9d181e8a85d46eb0263c3dfc7399a5b1d0cc40c1284dda4c333693b18e9,
// TokenMessengerMinter. old_denylister is Void on the ONLY observed
// instance (bootstrap).
func TestDecodeDenylisterChanged_BootstrapVoidOld(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     MainnetTokenMessengerMinter,
		Ledger:         62_146_653,
		TxHash:         "f73bb9d181e8a85d46eb0263c3dfc7399a5b1d0cc40c1284dda4c333693b18e9",
		LedgerClosedAt: "2026-04-16T14:44:59Z",
		Topic: []string{
			"AAAADwAAABJkZW55bGlzdGVyX2NoYW5nZWQAAA==",
			"AAAAAQ==",
			"AAAAEgAAAAAAAAAAfsJ19PMuLy4pOzeuXe1Eku2iqN+a3yVe7igZxj/Frr4=",
		},
		Value: "AAAAEQAAAAEAAAAA",
	}
	if got := Classify(ev); got != EventDenylisterChanged {
		t.Fatalf("Classify = %q, want %q", got, EventDenylisterChanged)
	}
	got, err := DecodeDenylisterChanged(ev)
	if err != nil {
		t.Fatalf("DecodeDenylisterChanged: %v", err)
	}
	if got.OldDenylister != "" {
		t.Errorf("OldDenylister = %q, want empty (void)", got.OldDenylister)
	}
	if got.NewDenylister != "GB7ME5PU6MXC6LRJHM324XPNISJO3IVI36NN6JK65YUBTRR7YWXL426Q" {
		t.Errorf("NewDenylister = %q", got.NewDenylister)
	}
	out := eventFromDenylisterChanged(got, time.Now().UTC())
	if out.EventType != EventDenylisterChanged {
		t.Errorf("projection EventType = %q", out.EventType)
	}
}

func TestDecodeDenylisterChanged_ShortTopic(t *testing.T) {
	t.Parallel()
	ev := &events.Event{Topic: []string{TopicSymbolDenylisterChanged, "AAAAAQ=="}}
	_, err := DecodeDenylisterChanged(ev)
	if !errors.Is(err, ErrMalformedTopic) {
		t.Errorf("want ErrMalformedTopic, got %v", err)
	}
}

// ─── fee_recipient_set ─────────────────────────────────────────────

// TestDecodeFeeRecipientSet_RealMainnetFixture — ledger 62146653, tx
// f73bb9d181e8a85d46eb0263c3dfc7399a5b1d0cc40c1284dda4c333693b18e9,
// TokenMessengerMinter.
func TestDecodeFeeRecipientSet_RealMainnetFixture(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     MainnetTokenMessengerMinter,
		Ledger:         62_146_653,
		TxHash:         "f73bb9d181e8a85d46eb0263c3dfc7399a5b1d0cc40c1284dda4c333693b18e9",
		LedgerClosedAt: "2026-04-16T14:44:59Z",
		Topic:          []string{"AAAADwAAABFmZWVfcmVjaXBpZW50X3NldAAAAA=="},
		Value:          "AAAAEQAAAAEAAAABAAAADwAAAA1mZWVfcmVjaXBpZW50AAAAAAAAEgAAAAAAAAAAUCIXXF6vvxROBDlm4fRXWbjy6VWii/GEFM24qYujPmc=",
	}
	if got := Classify(ev); got != EventFeeRecipientSet {
		t.Fatalf("Classify = %q, want %q", got, EventFeeRecipientSet)
	}
	got, err := DecodeFeeRecipientSet(ev)
	if err != nil {
		t.Fatalf("DecodeFeeRecipientSet: %v", err)
	}
	if got.FeeRecipient != "GBICEF24L2X36FCOAQ4WNYPUK5M3R4XJKWRIX4MECTG3RKMLUM7GOHOR" {
		t.Errorf("FeeRecipient = %q", got.FeeRecipient)
	}
	out := eventFromFeeRecipientSet(got, time.Now().UTC())
	if out.EventType != EventFeeRecipientSet {
		t.Errorf("projection EventType = %q", out.EventType)
	}
}

func TestDecodeFeeRecipientSet_MissingBodyField(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Topic: []string{TopicSymbolFeeRecipientSet},
		Value: "AAAAEQAAAAEAAAAA",
	}
	_, err := DecodeFeeRecipientSet(ev)
	if !errors.Is(err, ErrMalformedBody) {
		t.Errorf("want ErrMalformedBody, got %v", err)
	}
}

// ─── max_message_body_size_updated ─────────────────────────────────

// TestDecodeMaxMessageBodySizeUpdated_RealMainnetFixture — ledger
// 62146641, tx 5b53d56d4950a854bd39e3bc806478fd2aafffa5bcbfb86c19ca51eef8b90b7a,
// MessageTransmitter. new value 8192 bytes.
func TestDecodeMaxMessageBodySizeUpdated_RealMainnetFixture(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     MainnetMessageTransmitter,
		Ledger:         62_146_641,
		TxHash:         "5b53d56d4950a854bd39e3bc806478fd2aafffa5bcbfb86c19ca51eef8b90b7a",
		LedgerClosedAt: "2026-04-16T14:43:48Z",
		Topic:          []string{"AAAADwAAAB1tYXhfbWVzc2FnZV9ib2R5X3NpemVfdXBkYXRlZAAAAA=="},
		Value:          "AAAAEQAAAAEAAAABAAAADwAAABluZXdfbWF4X21lc3NhZ2VfYm9keV9zaXplAAAAAAAAAwAAIAA=",
	}
	if got := Classify(ev); got != EventMaxMessageBodySizeUpdated {
		t.Fatalf("Classify = %q, want %q", got, EventMaxMessageBodySizeUpdated)
	}
	got, err := DecodeMaxMessageBodySizeUpdated(ev)
	if err != nil {
		t.Fatalf("DecodeMaxMessageBodySizeUpdated: %v", err)
	}
	if got.NewMaxMessageBodySize != 8192 {
		t.Errorf("NewMaxMessageBodySize = %d, want 8192", got.NewMaxMessageBodySize)
	}
	out := eventFromMaxMessageBodySizeUpdated(got, time.Now().UTC())
	if out.EventType != EventMaxMessageBodySizeUpdated {
		t.Errorf("projection EventType = %q", out.EventType)
	}
}

func TestDecodeMaxMessageBodySizeUpdated_MissingBodyField(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Topic: []string{TopicSymbolMaxMessageBodySizeUpdated},
		Value: "AAAAEQAAAAEAAAAA",
	}
	_, err := DecodeMaxMessageBodySizeUpdated(ev)
	if !errors.Is(err, ErrMalformedBody) {
		t.Errorf("want ErrMalformedBody, got %v", err)
	}
}

// ─── min_fee_controller_set ────────────────────────────────────────

// TestDecodeMinFeeControllerSet_RealMainnetFixture — ledger 62146653,
// tx f73bb9d181e8a85d46eb0263c3dfc7399a5b1d0cc40c1284dda4c333693b18e9,
// TokenMessengerMinter.
func TestDecodeMinFeeControllerSet_RealMainnetFixture(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     MainnetTokenMessengerMinter,
		Ledger:         62_146_653,
		TxHash:         "f73bb9d181e8a85d46eb0263c3dfc7399a5b1d0cc40c1284dda4c333693b18e9",
		LedgerClosedAt: "2026-04-16T14:44:59Z",
		Topic: []string{
			"AAAADwAAABZtaW5fZmVlX2NvbnRyb2xsZXJfc2V0AAA=",
			"AAAAEgAAAAAAAAAAw8NBK6snRffJICAHH4taOcvP+J/olKKNQUOf9My3WI4=",
		},
		Value: "AAAAEQAAAAEAAAAA",
	}
	if got := Classify(ev); got != EventMinFeeControllerSet {
		t.Fatalf("Classify = %q, want %q", got, EventMinFeeControllerSet)
	}
	got, err := DecodeMinFeeControllerSet(ev)
	if err != nil {
		t.Fatalf("DecodeMinFeeControllerSet: %v", err)
	}
	if got.MinFeeController != "GDB4GQJLVMTUL56JEAQAOH4LLI44XT7YT7UJJIUNIFBZ75GMW5MI5SIG" {
		t.Errorf("MinFeeController = %q", got.MinFeeController)
	}
	out := eventFromMinFeeControllerSet(got, time.Now().UTC())
	if out.EventType != EventMinFeeControllerSet {
		t.Errorf("projection EventType = %q", out.EventType)
	}
}

func TestDecodeMinFeeControllerSet_ShortTopic(t *testing.T) {
	t.Parallel()
	ev := &events.Event{Topic: []string{TopicSymbolMinFeeControllerSet}}
	_, err := DecodeMinFeeControllerSet(ev)
	if !errors.Is(err, ErrMalformedTopic) {
		t.Errorf("want ErrMalformedTopic, got %v", err)
	}
}

// ─── pauser_changed / rescuer_changed ──────────────────────────────

// TestDecodePauserChanged_RealMainnetFixture — ledger 62146641, tx
// 5b53d56d4950a854bd39e3bc806478fd2aafffa5bcbfb86c19ca51eef8b90b7a,
// MessageTransmitter. NOTE the body field is `new_address`, not
// `new_pauser` — confirmed against the real event.
func TestDecodePauserChanged_RealMainnetFixture(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     MainnetMessageTransmitter,
		Ledger:         62_146_641,
		TxHash:         "5b53d56d4950a854bd39e3bc806478fd2aafffa5bcbfb86c19ca51eef8b90b7a",
		LedgerClosedAt: "2026-04-16T14:43:48Z",
		Topic:          []string{"AAAADwAAAA5wYXVzZXJfY2hhbmdlZAAA"},
		Value:          "AAAAEQAAAAEAAAABAAAADwAAAAtuZXdfYWRkcmVzcwAAAAASAAAAAAAAAAD5WmVihPYyDufrwFzW/Ue9OlfDUxmTfyhdvkrJFQ5h4g==",
	}
	if got := Classify(ev); got != EventPauserChanged {
		t.Fatalf("Classify = %q, want %q", got, EventPauserChanged)
	}
	got, err := DecodePauserChanged(ev)
	if err != nil {
		t.Fatalf("DecodePauserChanged: %v", err)
	}
	if got.NewAddress != "GD4VUZLCQT3DEDXH5PAFZVX5I66TUV6DKMMZG7ZILW7EVSIVBZQ6FR3P" {
		t.Errorf("NewAddress = %q", got.NewAddress)
	}
	out := eventFromPauserChanged(got, time.Now().UTC())
	if out.EventType != EventPauserChanged {
		t.Errorf("projection EventType = %q", out.EventType)
	}
}

func TestDecodePauserChanged_MissingBodyField(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Topic: []string{TopicSymbolPauserChanged},
		Value: "AAAAEQAAAAEAAAAA",
	}
	_, err := DecodePauserChanged(ev)
	if !errors.Is(err, ErrMalformedBody) {
		t.Errorf("want ErrMalformedBody, got %v", err)
	}
}

// TestDecodeRescuerChanged_RealMainnetFixture — ledger 62146641, tx
// 5b53d56d4950a854bd39e3bc806478fd2aafffa5bcbfb86c19ca51eef8b90b7a,
// MessageTransmitter.
func TestDecodeRescuerChanged_RealMainnetFixture(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     MainnetMessageTransmitter,
		Ledger:         62_146_641,
		TxHash:         "5b53d56d4950a854bd39e3bc806478fd2aafffa5bcbfb86c19ca51eef8b90b7a",
		LedgerClosedAt: "2026-04-16T14:43:48Z",
		Topic:          []string{"AAAADwAAAA9yZXNjdWVyX2NoYW5nZWQA"},
		Value:          "AAAAEQAAAAEAAAABAAAADwAAAAtuZXdfcmVzY3VlcgAAAAASAAAAAAAAAACUgOy4Qj4pyp6/DXnmRQpnBBTSEZbr/EGJ/2ZFIEEUfw==",
	}
	if got := Classify(ev); got != EventRescuerChanged {
		t.Fatalf("Classify = %q, want %q", got, EventRescuerChanged)
	}
	got, err := DecodeRescuerChanged(ev)
	if err != nil {
		t.Fatalf("DecodeRescuerChanged: %v", err)
	}
	if got.NewRescuer != "GCKIB3FYII7CTSU6X4GXTZSFBJTQIFGSCGLOX7CBRH7WMRJAIEKH7GOQ" {
		t.Errorf("NewRescuer = %q", got.NewRescuer)
	}
	out := eventFromRescuerChanged(got, time.Now().UTC())
	if out.EventType != EventRescuerChanged {
		t.Errorf("projection EventType = %q", out.EventType)
	}
}

func TestDecodeRescuerChanged_MissingBodyField(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Topic: []string{TopicSymbolRescuerChanged},
		Value: "AAAAEQAAAAEAAAAA",
	}
	_, err := DecodeRescuerChanged(ev)
	if !errors.Is(err, ErrMalformedBody) {
		t.Errorf("want ErrMalformedBody, got %v", err)
	}
}

// ─── set_token_controller ──────────────────────────────────────────

// TestDecodeSetTokenController_RealMainnetFixture — ledger 62146653,
// tx f73bb9d181e8a85d46eb0263c3dfc7399a5b1d0cc40c1284dda4c333693b18e9,
// TokenMessengerMinter.
func TestDecodeSetTokenController_RealMainnetFixture(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     MainnetTokenMessengerMinter,
		Ledger:         62_146_653,
		TxHash:         "f73bb9d181e8a85d46eb0263c3dfc7399a5b1d0cc40c1284dda4c333693b18e9",
		LedgerClosedAt: "2026-04-16T14:44:59Z",
		Topic:          []string{"AAAADwAAABRzZXRfdG9rZW5fY29udHJvbGxlcg=="},
		Value:          "AAAAEQAAAAEAAAABAAAADwAAABB0b2tlbl9jb250cm9sbGVyAAAAEgAAAAAAAAAAkdpJia/rUGphlYFb2HbNCnrcKE4ZBw6uqQ73rz/g4S0=",
	}
	if got := Classify(ev); got != EventSetTokenController {
		t.Fatalf("Classify = %q, want %q", got, EventSetTokenController)
	}
	got, err := DecodeSetTokenController(ev)
	if err != nil {
		t.Fatalf("DecodeSetTokenController: %v", err)
	}
	if got.TokenController != "GCI5USMJV7VVA2TBSWAVXWDWZUFHVXBIJYMQODVOVEHPPLZ74DQS34NM" {
		t.Errorf("TokenController = %q", got.TokenController)
	}
	out := eventFromSetTokenController(got, time.Now().UTC())
	if out.EventType != EventSetTokenController {
		t.Errorf("projection EventType = %q", out.EventType)
	}
}

func TestDecodeSetTokenController_MissingBodyField(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Topic: []string{TopicSymbolSetTokenController},
		Value: "AAAAEQAAAAEAAAAA",
	}
	_, err := DecodeSetTokenController(ev)
	if !errors.Is(err, ErrMalformedBody) {
		t.Errorf("want ErrMalformedBody, got %v", err)
	}
}

// ─── signature_threshold_updated ───────────────────────────────────

// TestDecodeSignatureThresholdUpdated_RealMainnetFixture — ledger
// 62146641, tx 5b53d56d4950a854bd39e3bc806478fd2aafffa5bcbfb86c19ca51eef8b90b7a,
// MessageTransmitter. 0 -> 2.
func TestDecodeSignatureThresholdUpdated_RealMainnetFixture(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     MainnetMessageTransmitter,
		Ledger:         62_146_641,
		TxHash:         "5b53d56d4950a854bd39e3bc806478fd2aafffa5bcbfb86c19ca51eef8b90b7a",
		LedgerClosedAt: "2026-04-16T14:43:48Z",
		Topic:          []string{"AAAADwAAABtzaWduYXR1cmVfdGhyZXNob2xkX3VwZGF0ZWQA"},
		Value:          "AAAAEQAAAAEAAAACAAAADwAAABduZXdfc2lnbmF0dXJlX3RocmVzaG9sZAAAAAADAAAAAgAAAA8AAAAXb2xkX3NpZ25hdHVyZV90aHJlc2hvbGQAAAAAAwAAAAA=",
	}
	if got := Classify(ev); got != EventSignatureThresholdUpdated {
		t.Fatalf("Classify = %q, want %q", got, EventSignatureThresholdUpdated)
	}
	got, err := DecodeSignatureThresholdUpdated(ev)
	if err != nil {
		t.Fatalf("DecodeSignatureThresholdUpdated: %v", err)
	}
	if got.NewSignatureThreshold != 2 {
		t.Errorf("NewSignatureThreshold = %d, want 2", got.NewSignatureThreshold)
	}
	if got.OldSignatureThreshold != 0 {
		t.Errorf("OldSignatureThreshold = %d, want 0", got.OldSignatureThreshold)
	}
	out := eventFromSignatureThresholdUpdated(got, time.Now().UTC())
	if out.EventType != EventSignatureThresholdUpdated {
		t.Errorf("projection EventType = %q", out.EventType)
	}
}

func TestDecodeSignatureThresholdUpdated_MissingBodyField(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Topic: []string{TopicSymbolSignatureThresholdUpdated},
		Value: "AAAAEQAAAAEAAAAA",
	}
	_, err := DecodeSignatureThresholdUpdated(ev)
	if !errors.Is(err, ErrMalformedBody) {
		t.Errorf("want ErrMalformedBody, got %v", err)
	}
}

// ─── set_burn_limit_per_message ────────────────────────────────────

// TestDecodeSetBurnLimitPerMessage_RealMainnetFixture — ledger
// 62146712, tx 3548733cb4f3e4272ee96a4d54b656eb4f38e86b9148bf496850a932bed574a9,
// TokenMessengerMinter. token is the same Stellar USDC SAC as
// TestDecodeTokenPairLinked_RealMainnetFixture's local_token.
func TestDecodeSetBurnLimitPerMessage_RealMainnetFixture(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     MainnetTokenMessengerMinter,
		Ledger:         62_146_712,
		TxHash:         "3548733cb4f3e4272ee96a4d54b656eb4f38e86b9148bf496850a932bed574a9",
		LedgerClosedAt: "2026-04-16T14:50:43Z",
		Topic: []string{
			"AAAADwAAABpzZXRfYnVybl9saW1pdF9wZXJfbWVzc2FnZQAA",
			"AAAAEgAAAAGt785ZruUpaPdgYdSUwlJbdWWfpClqZfSZ7ynlZHfklg==",
		},
		Value: "AAAAEQAAAAEAAAABAAAADwAAABZidXJuX2xpbWl0X3Blcl9tZXNzYWdlAAAAAAAKAAAAAAAAAAAAAAAAAAAAAA==",
	}
	if got := Classify(ev); got != EventSetBurnLimitPerMessage {
		t.Fatalf("Classify = %q, want %q", got, EventSetBurnLimitPerMessage)
	}
	got, err := DecodeSetBurnLimitPerMessage(ev)
	if err != nil {
		t.Fatalf("DecodeSetBurnLimitPerMessage: %v", err)
	}
	if got.Token != "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75" {
		t.Errorf("Token = %q", got.Token)
	}
	if got.BurnLimitPerMessage != "0" {
		t.Errorf("BurnLimitPerMessage = %q, want 0", got.BurnLimitPerMessage)
	}
	out := eventFromSetBurnLimitPerMessage(got, time.Now().UTC())
	if out.Token != got.Token {
		t.Errorf("projection Token = %q, want %q", out.Token, got.Token)
	}
	if out.Amount != "" {
		t.Error("set_burn_limit_per_message should NOT promote to Event.Amount (policy ceiling, not a movement)")
	}
	if out.Attributes["burn_limit_per_message"] != got.BurnLimitPerMessage {
		t.Error("projection burn_limit_per_message mismatch")
	}
}

func TestDecodeSetBurnLimitPerMessage_MissingBodyField(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Topic: []string{
			TopicSymbolSetBurnLimitPerMessage,
			"AAAAEgAAAAGt785ZruUpaPdgYdSUwlJbdWWfpClqZfSZ7ynlZHfklg==",
		},
		Value: "AAAAEQAAAAEAAAAA",
	}
	_, err := DecodeSetBurnLimitPerMessage(ev)
	if !errors.Is(err, ErrMalformedBody) {
		t.Errorf("want ErrMalformedBody, got %v", err)
	}
}

func TestDecodeSetBurnLimitPerMessage_ShortTopic(t *testing.T) {
	t.Parallel()
	ev := &events.Event{Topic: []string{TopicSymbolSetBurnLimitPerMessage}}
	_, err := DecodeSetBurnLimitPerMessage(ev)
	if !errors.Is(err, ErrMalformedTopic) {
		t.Errorf("want ErrMalformedTopic, got %v", err)
	}
}

// ─── swap_minter_config_set ────────────────────────────────────────

// TestDecodeSwapMinterConfigSet_RealMainnetFixture — ledger 62146806,
// tx 20474c2b2f6c64ba838ad6b3f70dde01a6cf7ce9651027092de1dee079aebb2d,
// TokenMessengerMinter. Nested-map body.
func TestDecodeSwapMinterConfigSet_RealMainnetFixture(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     MainnetTokenMessengerMinter,
		Ledger:         62_146_806,
		TxHash:         "20474c2b2f6c64ba838ad6b3f70dde01a6cf7ce9651027092de1dee079aebb2d",
		LedgerClosedAt: "2026-04-16T14:59:37Z",
		Topic: []string{
			"AAAADwAAABZzd2FwX21pbnRlcl9jb25maWdfc2V0AAA=",
			"AAAAEgAAAAGt785ZruUpaPdgYdSUwlJbdWWfpClqZfSZ7ynlZHfklg==",
		},
		Value: "AAAAEQAAAAEAAAABAAAADwAAABJzd2FwX21pbnRlcl9jb25maWcAAAAAABEAAAABAAAAAgAAAA8AAAALYWxsb3dfYXNzZXQAAAAAEgAAAAGdvXNSHLc6+nr9bhv7pcERoHEpmWrrvz5aYAaUra7bIQAAAA8AAAALc3dhcF9taW50ZXIAAAAAEgAAAAGetNfgUoqCcXctaWzcaNHasgv02KBwMqNeDwKZhqzFHg==",
	}
	if got := Classify(ev); got != EventSwapMinterConfigSet {
		t.Fatalf("Classify = %q, want %q", got, EventSwapMinterConfigSet)
	}
	got, err := DecodeSwapMinterConfigSet(ev)
	if err != nil {
		t.Fatalf("DecodeSwapMinterConfigSet: %v", err)
	}
	if got.Token != "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75" {
		t.Errorf("Token = %q", got.Token)
	}
	if got.AllowAsset != "CCO3242SDS3TV6T27VXBX65FYEI2A4JJTFVOXPZ6LJQANFFNV3NSD575" {
		t.Errorf("AllowAsset = %q", got.AllowAsset)
	}
	if got.SwapMinter != "CCPLJV7AKKFIE4LXFVUWZXDI2HNLEC7U3CQHAMVDLYHQFGMGVTCR4D5W" {
		t.Errorf("SwapMinter = %q", got.SwapMinter)
	}
	out := eventFromSwapMinterConfigSet(got, time.Now().UTC())
	if out.Token != got.Token {
		t.Errorf("projection Token = %q, want %q", out.Token, got.Token)
	}
	if out.Attributes["allow_asset"] != got.AllowAsset || out.Attributes["swap_minter"] != got.SwapMinter {
		t.Error("projection nested-map fields mismatch")
	}
}

func TestDecodeSwapMinterConfigSet_MissingBodyField(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Topic: []string{
			TopicSymbolSwapMinterConfigSet,
			"AAAAEgAAAAGt785ZruUpaPdgYdSUwlJbdWWfpClqZfSZ7ynlZHfklg==",
		},
		Value: "AAAAEQAAAAEAAAAA",
	}
	_, err := DecodeSwapMinterConfigSet(ev)
	if !errors.Is(err, ErrMalformedBody) {
		t.Errorf("want ErrMalformedBody, got %v", err)
	}
}

func TestDecodeSwapMinterConfigSet_ShortTopic(t *testing.T) {
	t.Parallel()
	ev := &events.Event{Topic: []string{TopicSymbolSwapMinterConfigSet}}
	_, err := DecodeSwapMinterConfigSet(ev)
	if !errors.Is(err, ErrMalformedTopic) {
		t.Errorf("want ErrMalformedTopic, got %v", err)
	}
}

// ─── token_decimal_config_added ────────────────────────────────────

// TestDecodeTokenDecimalConfigAdded_RealMainnetFixture — ledger
// 62146699, tx 670772e55fd37004bf641f7fb65c61a81985766b82814eca81af09d5c00749b5,
// TokenMessengerMinter. Nested-map body; canonical=6, local=7.
func TestDecodeTokenDecimalConfigAdded_RealMainnetFixture(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		ContractID:     MainnetTokenMessengerMinter,
		Ledger:         62_146_699,
		TxHash:         "670772e55fd37004bf641f7fb65c61a81985766b82814eca81af09d5c00749b5",
		LedgerClosedAt: "2026-04-16T14:49:26Z",
		Topic: []string{
			"AAAADwAAABp0b2tlbl9kZWNpbWFsX2NvbmZpZ19hZGRlZAAA",
			"AAAAEgAAAAGt785ZruUpaPdgYdSUwlJbdWWfpClqZfSZ7ynlZHfklg==",
		},
		Value: "AAAAEQAAAAEAAAABAAAADwAAABR0b2tlbl9kZWNpbWFsX2NvbmZpZwAAABEAAAABAAAAAgAAAA8AAAASY2Fub25pY2FsX2RlY2ltYWxzAAAAAAADAAAABgAAAA8AAAAObG9jYWxfZGVjaW1hbHMAAAAAAAMAAAAH",
	}
	if got := Classify(ev); got != EventTokenDecimalConfigAdded {
		t.Fatalf("Classify = %q, want %q", got, EventTokenDecimalConfigAdded)
	}
	got, err := DecodeTokenDecimalConfigAdded(ev)
	if err != nil {
		t.Fatalf("DecodeTokenDecimalConfigAdded: %v", err)
	}
	if got.Token != "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75" {
		t.Errorf("Token = %q", got.Token)
	}
	if got.CanonicalDecimals != 6 {
		t.Errorf("CanonicalDecimals = %d, want 6", got.CanonicalDecimals)
	}
	if got.LocalDecimals != 7 {
		t.Errorf("LocalDecimals = %d, want 7", got.LocalDecimals)
	}
	out := eventFromTokenDecimalConfigAdded(got, time.Now().UTC())
	if out.Token != got.Token {
		t.Errorf("projection Token = %q, want %q", out.Token, got.Token)
	}
	if out.Attributes["canonical_decimals"] != got.CanonicalDecimals || out.Attributes["local_decimals"] != got.LocalDecimals {
		t.Error("projection nested-map fields mismatch")
	}
}

func TestDecodeTokenDecimalConfigAdded_MissingBodyField(t *testing.T) {
	t.Parallel()
	ev := &events.Event{
		Topic: []string{
			TopicSymbolTokenDecimalConfigAdded,
			"AAAAEgAAAAGt785ZruUpaPdgYdSUwlJbdWWfpClqZfSZ7ynlZHfklg==",
		},
		Value: "AAAAEQAAAAEAAAAA",
	}
	_, err := DecodeTokenDecimalConfigAdded(ev)
	if !errors.Is(err, ErrMalformedBody) {
		t.Errorf("want ErrMalformedBody, got %v", err)
	}
}

func TestDecodeTokenDecimalConfigAdded_ShortTopic(t *testing.T) {
	t.Parallel()
	ev := &events.Event{Topic: []string{TopicSymbolTokenDecimalConfigAdded}}
	_, err := DecodeTokenDecimalConfigAdded(ev)
	if !errors.Is(err, ErrMalformedTopic) {
		t.Errorf("want ErrMalformedTopic, got %v", err)
	}
}

// ─── Decoder.Decode end-to-end (dispatcher_adapter.go wiring) ────

// TestDecoder_Decode_FullCensusEvents exercises the full Matches +
// Decode path for all 16 remaining topics from a known CCTP contract,
// guarding the dispatcher_adapter.go switch statement (a Classify
// case with no matching switch arm would silently fall through to
// ErrUnknownEvent instead of emitting a row) — same guard as
// governance_test.go's TestDecoder_Decode_GovernanceEvents for the
// first 5.
func TestDecoder_Decode_FullCensusEvents(t *testing.T) {
	t.Parallel()
	d := NewDecoder()
	cases := []struct {
		name     string
		topic    []string
		data     string
		wantType string
	}{
		{"admin_change_started", []string{"AAAADwAAABRhZG1pbl9jaGFuZ2Vfc3RhcnRlZA=="}, "AAAAEQAAAAEAAAACAAAADwAAAAluZXdfYWRtaW4AAAAAAAASAAAAAAAAAAC8xhLeF5/OQjNgnK4pItnFxfht5Gfrk/bDZVpBNjNzJwAAAA8AAAAJb2xkX2FkbWluAAAAAAAAEgAAAAAAAAAAkdpJia/rUGphlYFb2HbNCnrcKE4ZBw6uqQ73rz/g4S0=", EventAdminChangeStarted},
		{"attester_enabled", []string{"AAAADwAAABBhdHRlc3Rlcl9lbmFibGVk", "AAAADQAAABRyWwb3P/dh71OQ45MV4r+/YNM/lg=="}, "AAAAEQAAAAEAAAAA", EventAttesterEnabled},
		{"attester_manager_updated", []string{"AAAADwAAABhhdHRlc3Rlcl9tYW5hZ2VyX3VwZGF0ZWQ=", "AAAAAQ==", "AAAAEgAAAAAAAAAA9bSlyVxVQQmx8tUDobSxGIOsZhZsvmbYUSu91GsAm48="}, "AAAAEQAAAAEAAAAA", EventAttesterManagerUpdated},
		{"denylisted", []string{"AAAADwAAAApkZW55bGlzdGVkAAA=", "AAAAEgAAAAAAAAAAng3s07/WXbZx2tEGgoBNlJi8nX7sQ60t98kRs8dPg1s="}, "AAAAEQAAAAEAAAAA", EventDenylisted},
		{"un_denylisted", []string{"AAAADwAAAA11bl9kZW55bGlzdGVkAAAA", "AAAAEgAAAAAAAAAAng3s07/WXbZx2tEGgoBNlJi8nX7sQ60t98kRs8dPg1s="}, "AAAAEQAAAAEAAAAA", EventUnDenylisted},
		{"denylister_changed", []string{"AAAADwAAABJkZW55bGlzdGVyX2NoYW5nZWQAAA==", "AAAAAQ==", "AAAAEgAAAAAAAAAAfsJ19PMuLy4pOzeuXe1Eku2iqN+a3yVe7igZxj/Frr4="}, "AAAAEQAAAAEAAAAA", EventDenylisterChanged},
		{"fee_recipient_set", []string{"AAAADwAAABFmZWVfcmVjaXBpZW50X3NldAAAAA=="}, "AAAAEQAAAAEAAAABAAAADwAAAA1mZWVfcmVjaXBpZW50AAAAAAAAEgAAAAAAAAAAUCIXXF6vvxROBDlm4fRXWbjy6VWii/GEFM24qYujPmc=", EventFeeRecipientSet},
		{"max_message_body_size_updated", []string{"AAAADwAAAB1tYXhfbWVzc2FnZV9ib2R5X3NpemVfdXBkYXRlZAAAAA=="}, "AAAAEQAAAAEAAAABAAAADwAAABluZXdfbWF4X21lc3NhZ2VfYm9keV9zaXplAAAAAAAAAwAAIAA=", EventMaxMessageBodySizeUpdated},
		{"min_fee_controller_set", []string{"AAAADwAAABZtaW5fZmVlX2NvbnRyb2xsZXJfc2V0AAA=", "AAAAEgAAAAAAAAAAw8NBK6snRffJICAHH4taOcvP+J/olKKNQUOf9My3WI4="}, "AAAAEQAAAAEAAAAA", EventMinFeeControllerSet},
		{"pauser_changed", []string{"AAAADwAAAA5wYXVzZXJfY2hhbmdlZAAA"}, "AAAAEQAAAAEAAAABAAAADwAAAAtuZXdfYWRkcmVzcwAAAAASAAAAAAAAAAD5WmVihPYyDufrwFzW/Ue9OlfDUxmTfyhdvkrJFQ5h4g==", EventPauserChanged},
		{"rescuer_changed", []string{"AAAADwAAAA9yZXNjdWVyX2NoYW5nZWQA"}, "AAAAEQAAAAEAAAABAAAADwAAAAtuZXdfcmVzY3VlcgAAAAASAAAAAAAAAACUgOy4Qj4pyp6/DXnmRQpnBBTSEZbr/EGJ/2ZFIEEUfw==", EventRescuerChanged},
		{"set_token_controller", []string{"AAAADwAAABRzZXRfdG9rZW5fY29udHJvbGxlcg=="}, "AAAAEQAAAAEAAAABAAAADwAAABB0b2tlbl9jb250cm9sbGVyAAAAEgAAAAAAAAAAkdpJia/rUGphlYFb2HbNCnrcKE4ZBw6uqQ73rz/g4S0=", EventSetTokenController},
		{"signature_threshold_updated", []string{"AAAADwAAABtzaWduYXR1cmVfdGhyZXNob2xkX3VwZGF0ZWQA"}, "AAAAEQAAAAEAAAACAAAADwAAABduZXdfc2lnbmF0dXJlX3RocmVzaG9sZAAAAAADAAAAAgAAAA8AAAAXb2xkX3NpZ25hdHVyZV90aHJlc2hvbGQAAAAAAwAAAAA=", EventSignatureThresholdUpdated},
		{"set_burn_limit_per_message", []string{"AAAADwAAABpzZXRfYnVybl9saW1pdF9wZXJfbWVzc2FnZQAA", "AAAAEgAAAAGt785ZruUpaPdgYdSUwlJbdWWfpClqZfSZ7ynlZHfklg=="}, "AAAAEQAAAAEAAAABAAAADwAAABZidXJuX2xpbWl0X3Blcl9tZXNzYWdlAAAAAAAKAAAAAAAAAAAAAAAAAAAAAA==", EventSetBurnLimitPerMessage},
		{"swap_minter_config_set", []string{"AAAADwAAABZzd2FwX21pbnRlcl9jb25maWdfc2V0AAA=", "AAAAEgAAAAGt785ZruUpaPdgYdSUwlJbdWWfpClqZfSZ7ynlZHfklg=="}, "AAAAEQAAAAEAAAABAAAADwAAABJzd2FwX21pbnRlcl9jb25maWcAAAAAABEAAAABAAAAAgAAAA8AAAALYWxsb3dfYXNzZXQAAAAAEgAAAAGdvXNSHLc6+nr9bhv7pcERoHEpmWrrvz5aYAaUra7bIQAAAA8AAAALc3dhcF9taW50ZXIAAAAAEgAAAAGetNfgUoqCcXctaWzcaNHasgv02KBwMqNeDwKZhqzFHg==", EventSwapMinterConfigSet},
		{"token_decimal_config_added", []string{"AAAADwAAABp0b2tlbl9kZWNpbWFsX2NvbmZpZ19hZGRlZAAA", "AAAAEgAAAAGt785ZruUpaPdgYdSUwlJbdWWfpClqZfSZ7ynlZHfklg=="}, "AAAAEQAAAAEAAAABAAAADwAAABR0b2tlbl9kZWNpbWFsX2NvbmZpZwAAABEAAAABAAAAAgAAAA8AAAASY2Fub25pY2FsX2RlY2ltYWxzAAAAAAADAAAABgAAAA8AAAAObG9jYWxfZGVjaW1hbHMAAAAAAAMAAAAH", EventTokenDecimalConfigAdded},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ev := events.Event{
				ContractID:     MainnetTokenMessengerMinter,
				LedgerClosedAt: "2026-05-28T00:00:00Z",
				Topic:          c.topic,
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
