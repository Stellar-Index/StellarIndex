package soroswap

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Golden regression: a real mainnet pair emitted sync then swap with amount_0_in=0, amount_0_out=0, amount_1_in=265,
// amount_1_out=70: value moved within token1 only, a real event but NOT a trade. It must be a recognized no-op (zero
// events, nil error) so the re-derive counts expected-zero.
const (
	ndPair   = "CAM7DY53G63XA4AJRS24Z6VFYAFSSF76C3RZ45BE5YU3FQS5255OOABP"
	ndTx     = "be7028b942c6ffa0cacdf35ae44ab1b19efa63fd04c2ee72049c3d6348594b7f"
	ndLedger = uint32(57403300)
	ndClosed = "2025-06-05T10:19:35Z"

	// topics_xdr: [String("SoroswapPair"), Symbol("sync")]
	ndSyncTopic0 = "AAAADgAAAAxTb3Jvc3dhcFBhaXI="
	ndSyncTopic1 = "AAAADwAAAARzeW5j"
	// SyncEvent { new_reserve_0: i128, new_reserve_1: i128 }
	ndSyncData = "AAAAEQAAAAEAAAACAAAADwAAAA1uZXdfcmVzZXJ2ZV8wAAAAAAAACgAAAAAAAAAAAAADqUgKoKIAAAAPAAAADW5ld19yZXNlcnZlXzEAAAAAAAAKAAAAAAAAAAAAAAD5dajMow=="

	// topics_xdr: [String("SoroswapPair"), Symbol("swap")]
	ndSwapTopic1 = "AAAADwAAAARzd2Fw"
	// SwapEvent { amount_0_in: 0, amount_0_out: 0, amount_1_in: 265,
	//             amount_1_out: 70, to: Address }
	ndSwapData = "AAAAEQAAAAEAAAAFAAAADwAAAAthbW91bnRfMF9pbgAAAAAKAAAAAAAAAAAAAAAAAAAAAAAAAA8AAAAMYW1vdW50XzBfb3V0AAAACgAAAAAAAAAAAAAAAAAAAAAAAAAPAAAAC2Ftb3VudF8xX2luAAAAAAoAAAAAAAAAAAAAAAAAAAEJAAAADwAAAAxhbW91bnRfMV9vdXQAAAAKAAAAAAAAAAAAAAAAAAAARgAAAA8AAAACdG8AAAAAABIAAAABEyDGdpxSLowwUitxX2f0F1gov8YWa1TTzY3IvhOidV8="
)

func ndEvent(topic0, topic1, data string, eventIndex int) events.Event {
	return events.Event{
		ContractID:     ndPair,
		Ledger:         ndLedger,
		TxHash:         ndTx,
		LedgerClosedAt: ndClosed,
		OperationIndex: 0,
		EventIndex:     eventIndex,
		Topic:          []string{topic0, topic1},
		Value:          data,
		Type:           "contract",
	}
}

func TestNonDirectionalSwap_RealLedger57403300_RecognizedNoOp(t *testing.T) {
	syncEv := ndEvent(ndSyncTopic0, ndSyncTopic1, ndSyncData, 6)
	swapEv := ndEvent(ndSyncTopic0, ndSwapTopic1, ndSwapData, 7)

	dec := NewDecoder()
	dec.SeedPair(ndPair, mustSorobanAsset(t, 0x01), mustSorobanAsset(t, 0x02))

	// The decoder CLAIMS both events — this is what made the old
	// decode error an undecodable-but-MATCHED blind spot rather than
	// an unrecognized shape.
	if !dec.Matches(syncEv) {
		t.Fatal("Matches(sync) = false for a registered pair")
	}
	if !dec.Matches(swapEv) {
		t.Fatal("Matches(swap) = false for a registered pair")
	}

	// Real on-chain order: sync (index 6) precedes swap (index 7).
	outs, err := dec.Decode(syncEv)
	if err != nil {
		t.Fatalf("Decode(sync): %v", err)
	}
	if len(outs) != 0 {
		t.Fatalf("Decode(sync) emitted %d events, want 0 (buffering)", len(outs))
	}

	// The swap completes the pair; the non-directional body must be a
	// recognized no-op: zero projected rows, nil error.
	outs, err = dec.Decode(swapEv)
	if err != nil {
		t.Fatalf("Decode(swap) = %v, want nil (recognized no-op — a decode error here re-blinds the completeness re-derive on ledger 57403300)", err)
	}
	if len(outs) != 0 {
		t.Fatalf("Decode(swap) emitted %d events, want 0 (non-directional swap is not a trade)", len(outs))
	}
	if got := dec.SkippedNonDirectional(); got != 1 {
		t.Errorf("SkippedNonDirectional() = %d, want 1", got)
	}
	// Sanity: nothing was misattributed to the unknown-pair skip path.
	if got := dec.SkippedUnknownPair(); got != 0 {
		t.Errorf("SkippedUnknownPair() = %d, want 0", got)
	}
}

// The pair WASM's LP-share SEP-41 events are classified for the every-event enumeration but deliberately NOT claimed:
// they belong to the sep41 domain, and the dispatcher is first-match-wins, so claiming them would swallow events of any
// LP-share token later added to watched_sep41_contracts.
func TestPairTokenEvents_ClassifiedButNotClaimed(t *testing.T) {
	dec := NewDecoder()
	dec.SeedPair(ndPair, mustSorobanAsset(t, 0x01), mustSorobanAsset(t, 0x02))

	for name, topic0 := range map[string]string{
		"transfer": TopicSymbolLPTransfer,
		"mint":     TopicSymbolLPMint,
		"burn":     TopicSymbolLPBurn,
		"approve":  TopicSymbolLPApprove,
	} {
		ev := ndEvent(topic0, ndSyncTopic1 /* arbitrary second topic (from-address slot) */, "", 0)
		if got := classify(&ev); got != EventPairToken {
			t.Errorf("classify(%s) = %q, want %q", name, got, EventPairToken)
		}
		if dec.Matches(ev) {
			t.Errorf("Matches(%s) = true — LP-share token events must not be claimed (first-match-wins would swallow watched SEP-41 contracts)", name)
		}
	}
}
