// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package soroswap

import (
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Golden regression from the completeness projection "false-red" investigation
// (2026-08-18): mainnet ledger 56,779,692, tx a479f1d6…, op_index 0, registered
// pair CDJDRGUC… (soroswap_pairs: token0=CAS3J7GY…, token1=CDIKURWHYS…). The
// pair emitted sync (event_index 2) then swap (event_index 3) whose body settled
// with THREE non-zero amounts:
//
//	amount_0_in  = 10,000,000
//	amount_0_out = 8,079,362,527
//	amount_1_in  = 2,306,549,617
//	amount_1_out = 0
//
// token0 has BOTH an `in` and an `out` leg, so the direction is ambiguous: no
// single (in,out) cross-token pair describes the trade. The pre-2026-08-03
// decoder's all-four-only ambiguity guard let this three-leg shape match the
// `in1 && out0` arm and emit a trade reporting the GROSS amount_1_in /
// amount_0_out while silently dropping the 10M amount_0_in leg — fabricating a
// price. That mis-decode is exactly the served gen-0 `trades` row this ledger
// still carries (base_amount=2306549617, quote_amount=8079362527), which the
// current re-derive produces ZERO for — the reconcile's `expected=0 served=1`.
//
// This test pins the CURRENT (correct) behaviour: the completed swap+sync is a
// recognized no-op (ErrAmbiguousSwapDirection, which wraps ErrNonDirectionalSwap)
// — zero projected rows, nil error — so the ADR-0033 re-derive counts it
// expected-zero. The served row is therefore a stale phantom, not a re-derive
// decode gap; the fix is an operator-side targeted re-projection to purge it,
// NOT a decoder change and NOT masking the reconcile.
//
// Bytes are verbatim from the r1 ClickHouse lake (stellar.contract_events,
// tx_hash=a479f1d6…, ledger_seq=56779692), fetched 2026-08-18.
const (
	ambPair   = "CDJDRGUCHANJDXALZVJ5IZVB76HX4MWCON5SHF4DE5HB64CBBR7W2ZCD"
	ambTx     = "a479f1d654c653f3c5c98f5e65e8d516cfd8beaeaf3a89fe01c912811cf1e7c6"
	ambLedger = uint32(56779692)
	ambClosed = "2025-04-25T14:44:45Z"

	// topics_xdr: [String("SoroswapPair"), Symbol("sync"|"swap")]
	ambTopic0    = "AAAADgAAAAxTb3Jvc3dhcFBhaXI="
	ambSyncTopic = "AAAADwAAAARzeW5j"
	ambSwapTopic = "AAAADwAAAARzd2Fw"

	// SyncEvent { new_reserve_0: i128, new_reserve_1: i128 }
	ambSyncData = "AAAAEQAAAAEAAAACAAAADwAAAA1uZXdfcmVzZXJ2ZV8wAAAAAAAACgAAAAAAAAAAAAAZpSvGv9kAAAAPAAAADW5ld19yZXNlcnZlXzEAAAAAAAAKAAAAAAAAAAAAAAdNLouPOw=="
	// SwapEvent { amount_0_in: 10000000, amount_0_out: 8079362527,
	//             amount_1_in: 2306549617, amount_1_out: 0, to: Address }
	ambSwapData = "AAAAEQAAAAEAAAAFAAAADwAAAAthbW91bnRfMF9pbgAAAAAKAAAAAAAAAAAAAAAAAJiWgAAAAA8AAAAMYW1vdW50XzBfb3V0AAAACgAAAAAAAAAAAAAAAeGRSd8AAAAPAAAAC2Ftb3VudF8xX2luAAAAAAoAAAAAAAAAAAAAAACJeydxAAAADwAAAAxhbW91bnRfMV9vdXQAAAAKAAAAAAAAAAAAAAAAAAAAAAAAAA8AAAACdG8AAAAAABIAAAAAAAAAAMUcvzgOdcieLt2C9JijX9oyqpAMt5GH2DBSHczKYXAY"
)

func ambEvent(t *testing.T, topic1, data string, eventIndex int) events.Event {
	t.Helper()
	return events.Event{
		ContractID:     ambPair,
		Ledger:         ambLedger,
		TxHash:         ambTx,
		LedgerClosedAt: ambClosed,
		OperationIndex: 0,
		EventIndex:     eventIndex,
		Topic:          []string{ambTopic0, topic1},
		Value:          data,
		Type:           "contract",
	}
}

// TestAmbiguousSwap_RealLedger56779692_RecognizedNoOp proves the current decoder
// refuses the real three-non-zero-leg swap at ledger 56,779,692 as a recognized
// no-op, so the projection re-derive counts it expected-zero — establishing that
// the surviving served `trades` row for this ledger is a stale gen-0 phantom
// (a pre-2026-08-03 mis-decode), not a re-derive decode gap.
func TestAmbiguousSwap_RealLedger56779692_RecognizedNoOp(t *testing.T) {
	// First, the WHY: the real swap body decodes to exactly the three
	// non-zero legs (0_in, 0_out, 1_in) that make the direction ambiguous.
	amts, err := decodeSwapAmounts(ambSwapData)
	if err != nil {
		t.Fatalf("decodeSwapAmounts(real swap body): %v", err)
	}
	amt := func(n int64) canonical.Amount { return canonical.NewAmount(big.NewInt(n)) }
	for _, c := range []struct {
		name string
		got  canonical.Amount
		want canonical.Amount
	}{
		{"amount_0_in", amts.Amount0In, amt(10_000_000)},
		{"amount_0_out", amts.Amount0Out, amt(8_079_362_527)},
		{"amount_1_in", amts.Amount1In, amt(2_306_549_617)},
		{"amount_1_out", amts.Amount1Out, amt(0)},
	} {
		if c.got.Cmp(c.want) != 0 {
			t.Fatalf("%s = %s, want %s — fixture drifted", c.name, c.got, c.want)
		}
	}

	syncEv := ambEvent(t, ambSyncTopic, ambSyncData, 2)
	swapEv := ambEvent(t, ambSwapTopic, ambSwapData, 3)

	dec := NewDecoder()
	dec.SeedPair(ambPair, mustSorobanAsset(t, 0x01), mustSorobanAsset(t, 0x02))

	// The decoder CLAIMS both events (registered pair) — so a zero re-derive
	// here is a recognized no-op, NOT an unknown-pair / registry gap.
	if !dec.Matches(syncEv) {
		t.Fatal("Matches(sync) = false for a registered pair")
	}
	if !dec.Matches(swapEv) {
		t.Fatal("Matches(swap) = false for a registered pair")
	}

	// Real on-chain order: sync (index 2) precedes swap (index 3).
	outs, err := dec.Decode(syncEv)
	if err != nil {
		t.Fatalf("Decode(sync): %v", err)
	}
	if len(outs) != 0 {
		t.Fatalf("Decode(sync) emitted %d events, want 0 (buffering)", len(outs))
	}

	// The swap completes the pair; the ambiguous body must be a recognized
	// no-op: zero projected rows, nil error.
	outs, err = dec.Decode(swapEv)
	if err != nil {
		t.Fatalf("Decode(swap) = %v, want nil (recognized no-op — the ambiguous three-leg swap is not a trade)", err)
	}
	if len(outs) != 0 {
		t.Fatalf("Decode(swap) emitted %d events, want 0 — the served gen-0 trade for this ledger is a phantom the current decoder must NOT reproduce", len(outs))
	}
	// Counted as non-directional (ErrAmbiguousSwapDirection wraps
	// ErrNonDirectionalSwap), and NOT as an unknown pair — proving the refusal
	// is the ambiguity guard, not a registry gap.
	if got := dec.SkippedNonDirectional(); got != 1 {
		t.Errorf("SkippedNonDirectional() = %d, want 1", got)
	}
	if got := dec.SkippedUnknownPair(); got != 0 {
		t.Errorf("SkippedUnknownPair() = %d, want 0 (the pair is registered — the zero re-derive is ambiguity, not a registry gap)", got)
	}
}
