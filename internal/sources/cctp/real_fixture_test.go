// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package cctp

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Real mainnet goldens for the four CCTP events otherwise covered only by
// synthetic bodies. Captured from stellar.contract_events in the lake, one
// tx each: ledger 64775357 (burn on Stellar toward domain 6) and ledger
// 64775825 (mint on Stellar from a domain-6 burn). Contract identity is
// asserted before decoding. Live WASM at capture, resolved from
// stellar.contract_instance_changes (see test/fixtures/reflector/README.md):
//   - TokenMessengerMinter: a6c1acc6e367e46535733ce8a7320c718616ed42f762cf2f1a0aa77ac6e056f6 (set at ledger 62225106)
//   - MessageTransmitter:   99bd0ddc506ee13fc4f433f0627092034dc38f4773ec16930a1823a1174431d4 (set at ledger 62225178)

var realDepositForBurn = events.Event{
	ContractID:     "CAE2G5Z77UP7GYPYGFOWFGW7C7J6I4YP2AFGSADRKQY62SYUFLPNFTXL",
	Ledger:         64775357,
	TxHash:         "8c67510be8336b6d4e982d33fe1061d774f006c756288c256e4a3296fbe7f375",
	OperationIndex: 0,
	LedgerClosedAt: "2026-10-05T01:09:27Z",
	Topic: []string{
		"AAAADwAAABBkZXBvc2l0X2Zvcl9idXJu",
		"AAAAEgAAAAGt785ZruUpaPdgYdSUwlJbdWWfpClqZfSZ7ynlZHfklg==",
		"AAAAEgAAAAAAAAAAk++2le8unEqaBj6CQNuDlDfp/fZLqoxnlDn23UEKwV8=",
		"AAAAAwAAB9A=",
	},
	Value: "AAAAEQAAAAEAAAAHAAAADwAAAAZhbW91bnQAAAAAAAoAAAAAAAAAAAAAAAAAHoSAAAAADwAAABJkZXN0aW5hdGlvbl9jYWxsZXIAAAAAAA0AAAAgAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAPAAAAEmRlc3RpbmF0aW9uX2RvbWFpbgAAAAAAAwAAAAYAAAAPAAAAG2Rlc3RpbmF0aW9uX3Rva2VuX21lc3NlbmdlcgAAAAANAAAAIAAAAAAAAAAAAAAAACi1oOnGIaW62qU2IZs6IoyBaM9dAAAADwAAAAlob29rX2RhdGEAAAAAAAANAAAAAAAAAA8AAAAHbWF4X2ZlZQAAAAAKAAAAAAAAAAAAAAAAAAAAAAAAAA8AAAAObWludF9yZWNpcGllbnQAAAAAAA0AAAAgAAAAAAAAAAAAAAAAoMxWGlKAy3gXq/BAjoojz9qayR8=",
}

var realMintAndWithdraw = events.Event{
	ContractID:     "CAE2G5Z77UP7GYPYGFOWFGW7C7J6I4YP2AFGSADRKQY62SYUFLPNFTXL",
	Ledger:         64775825,
	TxHash:         "c78a8fd73af540dfd334405240598bf6073b1629f22b91accb5e72e9b3b4df1f",
	OperationIndex: 0,
	LedgerClosedAt: "2026-10-05T01:48:27Z",
	Topic: []string{
		"AAAADwAAABFtaW50X2FuZF93aXRoZHJhdwAAAA==",
		"AAAAEgAAAAFyvSD/L4KBgBuwW3wpF5AmkzJW+rr+sT6U79jdvPzykQ==",
		"AAAAEgAAAAGt785ZruUpaPdgYdSUwlJbdWWfpClqZfSZ7ynlZHfklg==",
	},
	Value: "AAAAEQAAAAEAAAACAAAADwAAAAZhbW91bnQAAAAAAAoAAAAAAAAAAAAAAAABqpbkAAAADwAAAA1mZWVfY29sbGVjdGVkAAAAAAAACgAAAAAAAAAAAAAAAAAADjI=",
}

var realMessageSent = events.Event{
	ContractID:     "CACMENFFJPJMSDAJQLX4R7K3SFZIW2LJSE3R2UMLGSWHFHS353FVXAZV",
	Ledger:         64775357,
	TxHash:         "8c67510be8336b6d4e982d33fe1061d774f006c756288c256e4a3296fbe7f375",
	OperationIndex: 0,
	LedgerClosedAt: "2026-10-05T01:09:27Z",
	Topic: []string{
		"AAAADwAAAAxtZXNzYWdlX3NlbnQ=",
	},
	Value: "AAAAEQAAAAEAAAABAAAADwAAAAdtZXNzYWdlAAAAAA0AAAF4AAAAAQAAABsAAAAGAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAJo3c//R/zYfgxXWKa3xfT5HMP0AppAHFUMe1LFCre0gAAAAAAAAAAAAAAACi1oOnGIaW62qU2IZs6IoyBaM9dAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAfQAAAAAAAAAAGt785ZruUpaPdgYdSUwlJbdWWfpClqZfSZ7ynlZHfklgAAAAAAAAAAAAAAAKDMVhpSgMt4F6vwQI6KI8/amskfAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAehICT77aV7y6cSpoGPoJA24OUN+n99kuqjGeUOfbdQQrBXwAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA==",
}

var realMessageReceived = events.Event{
	ContractID:     "CACMENFFJPJMSDAJQLX4R7K3SFZIW2LJSE3R2UMLGSWHFHS353FVXAZV",
	Ledger:         64775825,
	TxHash:         "c78a8fd73af540dfd334405240598bf6073b1629f22b91accb5e72e9b3b4df1f",
	OperationIndex: 0,
	LedgerClosedAt: "2026-10-05T01:48:27Z",
	Topic: []string{
		"AAAADwAAABBtZXNzYWdlX3JlY2VpdmVk",
		"AAAAEgAAAAFyvSD/L4KBgBuwW3wpF5AmkzJW+rr+sT6U79jdvPzykQ==",
		"AAAADQAAACCbyZIWStcHdNYl6KgxuVs6SvVGA/eHNUSb+LNq+NzjPQ==",
		"AAAAAwAAA+g=",
	},
	Value: "AAAAEQAAAAEAAAADAAAADwAAAAxtZXNzYWdlX2JvZHkAAAANAAABPAAAAAEAAAAAAAAAAAAAAACDNYn81u224I9MfDLU9xtUvaApE3K9IP8vgoGAG7BbfCkXkCaTMlb6uv6xPpTv2N28/PKRAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAGqpRYAAAAAAAAAAAAAAADqJYSWqTEf/inN+SDKDou0tByfBAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAABEIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAADjIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA9yqDQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA4R0FNTkEyUTZOVFpTVUJMTUVYTFRJWFlPUkU3T1hKSkJNRUdJWDdUMk9YREFLSUE3Q0NLTjRSSlYAAAAPAAAABnNlbmRlcgAAAAAADQAAACAAAAAAAAAAAAAAAAAotaDpxiGlutqlNiGbOiKMgWjPXQAAAA8AAAANc291cmNlX2RvbWFpbgAAAAAAAAMAAAAG",
}

func requireRealEvent(t *testing.T, ev *events.Event, contract, want string) {
	t.Helper()
	if ev.ContractID != contract {
		t.Fatalf("ContractID = %q, want %q", ev.ContractID, contract)
	}
	if got := Classify(ev); got != want {
		t.Fatalf("Classify = %q, want %q", got, want)
	}
}

func TestDecodeDepositForBurn_RealMainnetFixture(t *testing.T) {
	t.Parallel()
	ev := realDepositForBurn
	requireRealEvent(t, &ev, MainnetTokenMessengerMinter, EventDepositForBurn)
	got, err := DecodeDepositForBurn(&ev)
	if err != nil {
		t.Fatalf("DecodeDepositForBurn: %v", err)
	}
	want := DepositForBurn{
		Ledger:                    64_775_357,
		TxHash:                    "8c67510be8336b6d4e982d33fe1061d774f006c756288c256e4a3296fbe7f375",
		OpIndex:                   0,
		ClosedAt:                  "2026-10-05T01:09:27Z",
		ContractID:                MainnetTokenMessengerMinter,
		BurnToken:                 "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		Depositor:                 "GCJ67NUV54XJYSU2AY7IEQG3QOKDP2P56ZF2VDDHSQ47NXKBBLAV65AO",
		MinFinalityThreshold:      2000,
		Amount:                    "2000000",
		MintRecipient:             "000000000000000000000000a0cc561a5280cb7817abf0408e8a23cfda9ac91f",
		DestinationDomain:         6,
		DestinationTokenMessenger: "00000000000000000000000028b5a0e9c621a5badaa536219b3a228c8168cf5d",
		DestinationCaller:         "0000000000000000000000000000000000000000000000000000000000000000",
		MaxFee:                    "0",
		HookData:                  "",
	}
	if got != want {
		t.Errorf("DepositForBurn mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestDecodeMintAndWithdraw_RealMainnetFixture(t *testing.T) {
	t.Parallel()
	ev := realMintAndWithdraw
	requireRealEvent(t, &ev, MainnetTokenMessengerMinter, EventMintAndWithdraw)
	got, err := DecodeMintAndWithdraw(&ev)
	if err != nil {
		t.Fatalf("DecodeMintAndWithdraw: %v", err)
	}
	want := MintAndWithdraw{
		Ledger:        64_775_825,
		TxHash:        "c78a8fd73af540dfd334405240598bf6073b1629f22b91accb5e72e9b3b4df1f",
		OpIndex:       0,
		ClosedAt:      "2026-10-05T01:48:27Z",
		ContractID:    MainnetTokenMessengerMinter,
		MintRecipient: MainnetCctpForwarder,
		MintToken:     "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75",
		Amount:        "27956964",
		FeeCollected:  "3634",
	}
	if got != want {
		t.Errorf("MintAndWithdraw mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestDecodeMessageSent_RealMainnetFixture(t *testing.T) {
	t.Parallel()
	ev := realMessageSent
	requireRealEvent(t, &ev, MainnetMessageTransmitter, EventMessageSent)
	got, err := DecodeMessageSent(&ev)
	if err != nil {
		t.Fatalf("DecodeMessageSent: %v", err)
	}
	want := MessageSent{
		Ledger:     64_775_357,
		TxHash:     "8c67510be8336b6d4e982d33fe1061d774f006c756288c256e4a3296fbe7f375",
		OpIndex:    0,
		ClosedAt:   "2026-10-05T01:09:27Z",
		ContractID: MainnetMessageTransmitter,
		Message:    "000000010000001b00000006000000000000000000000000000000000000000000000000000000000000000009a3773ffd1ff361f8315d629adf17d3e4730fd00a6900715431ed4b142aded200000000000000000000000028b5a0e9c621a5badaa536219b3a228c8168cf5d0000000000000000000000000000000000000000000000000000000000000000000007d00000000000000001adefce59aee52968f76061d494c2525b75659fa4296a65f499ef29e56477e496000000000000000000000000a0cc561a5280cb7817abf0408e8a23cfda9ac91f00000000000000000000000000000000000000000000000000000000001e848093efb695ef2e9c4a9a063e8240db839437e9fdf64baa8c679439f6dd410ac15f000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000",
	}
	if got != want {
		t.Errorf("MessageSent mismatch\n got: %+v\nwant: %+v", got, want)
	}
}

func TestDecodeMessageReceived_RealMainnetFixture(t *testing.T) {
	t.Parallel()
	ev := realMessageReceived
	requireRealEvent(t, &ev, MainnetMessageTransmitter, EventMessageReceived)
	got, err := DecodeMessageReceived(&ev)
	if err != nil {
		t.Fatalf("DecodeMessageReceived: %v", err)
	}
	want := MessageReceived{
		Ledger:                    64_775_825,
		TxHash:                    "c78a8fd73af540dfd334405240598bf6073b1629f22b91accb5e72e9b3b4df1f",
		OpIndex:                   0,
		ClosedAt:                  "2026-10-05T01:48:27Z",
		ContractID:                MainnetMessageTransmitter,
		Caller:                    MainnetCctpForwarder,
		Nonce:                     "9bc992164ad70774d625e8a831b95b3a4af54603f78735449bf8b36af8dce33d",
		FinalityThresholdExecuted: 1000,
		SourceDomain:              6,
		Sender:                    "00000000000000000000000028b5a0e9c621a5badaa536219b3a228c8168cf5d",
		MessageBody:               "00000001000000000000000000000000833589fcd6edb6e08f4c7c32d4f71b54bda0291372bd20ff2f8281801bb05b7c29179026933256fabafeb13e94efd8ddbcfcf2910000000000000000000000000000000000000000000000000000000001aaa516000000000000000000000000ea258496a9311ffe29cdf920ca0e8bb4b41c9f0400000000000000000000000000000000000000000000000000000000000011080000000000000000000000000000000000000000000000000000000000000e320000000000000000000000000000000000000000000000000000000003dcaa0d000000000000000000000000000000000000000000000000000000000000003847414d4e413251364e545a5355424c4d45584c544958594f5245374f584a4a424d454749583754324f5844414b49413743434b4e34524a56",
	}
	if got != want {
		t.Errorf("MessageReceived mismatch\n got: %+v\nwant: %+v", got, want)
	}
}
