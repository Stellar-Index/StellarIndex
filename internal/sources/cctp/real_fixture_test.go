// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package cctp

import (
	"encoding/binary"
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Real mainnet goldens for the four CCTP events otherwise covered only by
// synthetic bodies. Captured from stellar.contract_events in the lake, one
// tx each: ledger 64775357 (burn on Stellar toward domain 6) and ledger
// 64775825 (mint on Stellar from a domain-6 burn). Contract identity is
// asserted before decoding. Live WASM at capture, resolved from
// stellar.contract_instance_changes (see test/fixtures/reflector/README.md):
//   - a6c1acc6e367e46535733ce8a7320c718616ed42f762cf2f1a0aa77ac6e056f6 TokenMessengerMinter, set at ledger 62225106
//   - 99bd0ddc506ee13fc4f433f0627092034dc38f4773ec16930a1823a1174431d4 MessageTransmitter, set at ledger 62225178

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

// TestDepositForBurnAmount_IsCanonicalSixDecimals pins the outbound scale the
// served CCTP sums divide by 1e6 (protocol_bespoke_cctp.go): the burn's amount
// must equal the BurnMessage amount the same tx sends cross-chain, which the
// destination (domain 6, Base, 6-decimal USDC) mints 1:1. A 7-decimal local
// SAC amount here would be 10x the message amount.
func TestDepositForBurnAmount_IsCanonicalSixDecimals(t *testing.T) {
	t.Parallel()
	burnEv, msgEv := realDepositForBurn, realMessageSent
	burn, err := DecodeDepositForBurn(&burnEv)
	if err != nil {
		t.Fatalf("DecodeDepositForBurn: %v", err)
	}
	sent, err := DecodeMessageSent(&msgEv)
	if err != nil {
		t.Fatalf("DecodeMessageSent: %v", err)
	}
	if burn.TxHash != sent.TxHash || burn.OpIndex != sent.OpIndex {
		t.Fatalf("fixtures are not the same op: burn %s/%d, message %s/%d", burn.TxHash, burn.OpIndex, sent.TxHash, sent.OpIndex)
	}
	msg, err := hex.DecodeString(sent.Message)
	if err != nil {
		t.Fatalf("message hex: %v", err)
	}
	// CCTP v2 message: 148-byte header (version, source/destination domain at
	// [4:8]/[8:12]), then BurnMessage: version(4) burnToken(32)
	// mintRecipient(32) amount(32, big-endian uint256).
	const header, amountOff = 148, 148 + 4 + 32 + 32
	if len(msg) < amountOff+32 {
		t.Fatalf("message too short: %d bytes", len(msg))
	}
	if src, dst := binary.BigEndian.Uint32(msg[4:8]), binary.BigEndian.Uint32(msg[8:12]); src != 27 || dst != burn.DestinationDomain {
		t.Fatalf("message domains = %d->%d, want 27->%d", src, dst, burn.DestinationDomain)
	}
	if got, want := hex.EncodeToString(msg[header+4+32:amountOff]), burn.MintRecipient; got != want {
		t.Fatalf("BurnMessage mintRecipient = %s, want %s (not the same transfer)", got, want)
	}
	wire := new(big.Int).SetBytes(msg[amountOff : amountOff+32])
	amt, ok := new(big.Int).SetString(burn.Amount, 10)
	if !ok {
		t.Fatalf("DepositForBurn.Amount %q is not an integer", burn.Amount)
	}
	if amt.Cmp(wire) != 0 {
		t.Fatalf("deposit_for_burn amount %s != BurnMessage amount %s: outbound sums are not at the canonical 6-decimal scale", amt, wire)
	}
}
