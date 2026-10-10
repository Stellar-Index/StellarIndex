package cctp

import (
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// depositForBurnEvent builds a complete, well-formed deposit_for_burn
// events.Event for adapter-level tests. contractID lets a test point
// it at a non-CCTP contract to exercise the Matches gate.
func depositForBurnEvent(t *testing.T, contractID string) events.Event {
	t.Helper()
	burnToken := makeContractStrkey(t, 0x10)
	depositor := makeAccountStrkey(t, 0x20)
	body := b64(t, scMap(
		xdr.ScMapEntry{Key: symbol("amount"), Val: i128(big.NewInt(12_345_678))},
		xdr.ScMapEntry{Key: symbol("destination_caller"), Val: scBytes(makeBytesN32(0x50))},
		xdr.ScMapEntry{Key: symbol("destination_domain"), Val: u32(0)},
		xdr.ScMapEntry{Key: symbol("destination_token_messenger"), Val: scBytes(makeBytesN32(0x40))},
		xdr.ScMapEntry{Key: symbol("hook_data"), Val: scBytes([]byte("hook"))},
		xdr.ScMapEntry{Key: symbol("max_fee"), Val: i128(big.NewInt(500))},
		xdr.ScMapEntry{Key: symbol("mint_recipient"), Val: scBytes(makeBytesN32(0x30))},
	))
	return events.Event{
		Type:           "contract",
		Ledger:         62_700_000,
		LedgerClosedAt: "2026-05-20T14:00:00Z",
		ContractID:     contractID,
		OperationIndex: 1,
		TxHash:         "abc123",
		Topic: []string{
			TopicSymbolDepositForBurn,
			b64(t, contractAddrFromStrkey(t, burnToken)),
			b64(t, accountAddrFromStrkey(t, depositor)),
			b64(t, u32(2000)),
		},
		Value: body,
	}
}

func TestDecoder_Name(t *testing.T) {
	t.Parallel()
	if got := (&Decoder{}).Name(); got != SourceName {
		t.Errorf("Name() = %q, want %q", got, SourceName)
	}
}

func TestDecoder_Matches(t *testing.T) {
	t.Parallel()
	d := NewDecoder()

	t.Run("CCTP topic from CCTP contract", func(t *testing.T) {
		t.Parallel()
		if !d.Matches(depositForBurnEvent(t, MainnetTokenMessengerMinter)) {
			t.Error("want Matches=true for a deposit_for_burn from TokenMessengerMinter")
		}
	})

	t.Run("CCTP topic from non-CCTP contract", func(t *testing.T) {
		t.Parallel()
		// Same topic bytes, foreign emitter — must be rejected so a
		// look-alike contract can't inject rows (AGENTS.md: gate a decoder
		// on contract identity, ADR-0035).
		impostor := makeContractStrkey(t, 0x99)
		if d.Matches(depositForBurnEvent(t, impostor)) {
			t.Error("want Matches=false for a CCTP topic from a non-CCTP contract")
		}
	})

	t.Run("non-CCTP topic from CCTP contract", func(t *testing.T) {
		t.Parallel()
		ev := events.Event{
			ContractID: MainnetTokenMessengerMinter,
			Topic:      []string{b64(t, symbol("transfer"))},
		}
		if d.Matches(ev) {
			t.Error("want Matches=false for an unrecognised topic")
		}
	})
}

func TestIsCCTPContract(t *testing.T) {
	t.Parallel()
	for _, id := range []string{MainnetTokenMessengerMinter, MainnetMessageTransmitter, MainnetCctpForwarder} {
		if !IsCCTPContract(id) {
			t.Errorf("IsCCTPContract(%q) = false, want true", id)
		}
	}
	if IsCCTPContract(makeContractStrkey(t, 0x99)) {
		t.Error("IsCCTPContract on a foreign contract = true, want false")
	}
}

func TestDecoder_Decode_DepositForBurn(t *testing.T) {
	t.Parallel()
	out, err := NewDecoder().Decode(depositForBurnEvent(t, MainnetTokenMessengerMinter))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("Decode emitted %d events, want 1", len(out))
	}
	ev, ok := out[0].(Event)
	if !ok {
		t.Fatalf("emitted event is %T, want cctp.Event", out[0])
	}
	if ev.EventType != EventDepositForBurn {
		t.Errorf("EventType = %q, want %q", ev.EventType, EventDepositForBurn)
	}
	if ev.Amount != "12345678" {
		t.Errorf("Amount = %q, want 12345678", ev.Amount)
	}
	if ev.Fee != "500" {
		t.Errorf("Fee = %q, want 500", ev.Fee)
	}
	if ev.CounterpartyDomain == nil || *ev.CounterpartyDomain != 0 {
		t.Errorf("CounterpartyDomain = %v, want 0", ev.CounterpartyDomain)
	}
	if ev.Token == "" {
		t.Error("Token (burn_token) should be populated")
	}
	if ev.ObservedAt.IsZero() {
		t.Error("ObservedAt should be parsed from LedgerClosedAt")
	}
	for _, k := range []string{"depositor", "mint_recipient", "hook_data", "min_finality_threshold"} {
		if _, present := ev.Attributes[k]; !present {
			t.Errorf("Attributes missing %q", k)
		}
	}
	// Compile-time + runtime confirmation it is a consumer.Event.
	var _ consumer.Event = ev
	if ev.Source() != SourceName {
		t.Errorf("Source() = %q, want %q", ev.Source(), SourceName)
	}
}

func TestDecoder_Decode_NonCCTPContract(t *testing.T) {
	t.Parallel()
	// A genuine CCTP-shaped event from a foreign contract: Decode
	// returns nothing rather than minting a row.
	out, err := NewDecoder().Decode(depositForBurnEvent(t, makeContractStrkey(t, 0x99)))
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("Decode emitted %d events for a foreign contract, want 0", len(out))
	}
}

func TestDecoder_Decode_EmptyClosedAt(t *testing.T) {
	t.Parallel()
	ev := depositForBurnEvent(t, MainnetTokenMessengerMinter)
	ev.LedgerClosedAt = "" // EventClosedAt fails closed
	_, err := NewDecoder().Decode(ev)
	if err == nil {
		t.Fatal("want an error when LedgerClosedAt is empty")
	}
}

func TestDecoder_Decode_MintAndWithdraw(t *testing.T) {
	t.Parallel()
	mintRecipient := makeAccountStrkey(t, 0x60)
	mintToken := makeContractStrkey(t, 0x70)
	body := b64(t, scMap(
		xdr.ScMapEntry{Key: symbol("amount"), Val: i128(big.NewInt(1_000_000_000))},
		xdr.ScMapEntry{Key: symbol("fee_collected"), Val: i128(big.NewInt(50))},
	))
	ev := events.Event{
		Ledger:         62_700_005,
		LedgerClosedAt: "2026-05-20T14:00:25Z",
		ContractID:     MainnetTokenMessengerMinter,
		Topic: []string{
			TopicSymbolMintAndWithdraw,
			b64(t, accountAddrFromStrkey(t, mintRecipient)),
			b64(t, contractAddrFromStrkey(t, mintToken)),
		},
		Value: body,
	}
	out, err := NewDecoder().Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("Decode emitted %d events, want 1", len(out))
	}
	got := out[0].(Event)
	if got.EventType != EventMintAndWithdraw {
		t.Errorf("EventType = %q, want %q", got.EventType, EventMintAndWithdraw)
	}
	if got.Amount != "1000000000" || got.Fee != "50" {
		t.Errorf("Amount/Fee = %q/%q, want 1000000000/50", got.Amount, got.Fee)
	}
	// mint_and_withdraw carries no domain.
	if got.CounterpartyDomain != nil {
		t.Errorf("CounterpartyDomain = %v, want nil", *got.CounterpartyDomain)
	}
}

func TestDecoder_Decode_MessageReceived(t *testing.T) {
	t.Parallel()
	caller := makeAccountStrkey(t, 0x80)
	body := b64(t, scMap(
		xdr.ScMapEntry{Key: symbol("message_body"), Val: scBytes([]byte("payload"))},
		xdr.ScMapEntry{Key: symbol("sender"), Val: scBytes(makeBytesN32(0xA0))},
		xdr.ScMapEntry{Key: symbol("source_domain"), Val: u32(7)}, // Solana
	))
	ev := events.Event{
		LedgerClosedAt: "2026-05-20T14:01:00Z",
		ContractID:     MainnetMessageTransmitter,
		Topic: []string{
			TopicSymbolMessageReceived,
			b64(t, accountAddrFromStrkey(t, caller)),
			b64(t, scBytes(makeBytesN32(0x90))),
			b64(t, u32(2000)),
		},
		Value: body,
	}
	out, err := NewDecoder().Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("Decode emitted %d events, want 1", len(out))
	}
	got := out[0].(Event)
	if got.EventType != EventMessageReceived {
		t.Errorf("EventType = %q, want %q", got.EventType, EventMessageReceived)
	}
	if got.CounterpartyDomain == nil || *got.CounterpartyDomain != 7 {
		t.Errorf("CounterpartyDomain = %v, want 7 (source_domain)", got.CounterpartyDomain)
	}
}

func TestDecoder_Decode_MessageSent(t *testing.T) {
	t.Parallel()
	body := b64(t, scMap(
		xdr.ScMapEntry{Key: symbol("message"), Val: scBytes([]byte("envelope"))},
	))
	ev := events.Event{
		LedgerClosedAt: "2026-05-20T14:00:01Z",
		ContractID:     MainnetMessageTransmitter,
		Topic:          []string{TopicSymbolMessageSent},
		Value:          body,
	}
	out, err := NewDecoder().Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("Decode emitted %d events, want 1", len(out))
	}
	got := out[0].(Event)
	if got.EventType != EventMessageSent {
		t.Errorf("EventType = %q, want %q", got.EventType, EventMessageSent)
	}
	if got.Amount != "" || got.Token != "" || got.CounterpartyDomain != nil {
		t.Error("message_sent should carry no amount/token/domain")
	}
	if _, present := got.Attributes["message"]; !present {
		t.Error("Attributes missing 'message'")
	}
}

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
