package sorobanevents

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// TestReconstruct_RoundTripsCapture proves Capture → Reconstruct is
// loss-free for the fields per-source backfill subcommands need:
// ContractID, OpIndex, TxHash (via hex round-trip), Topic[0..N],
// Value, OpArgs. Other fields (ID, TransactionIndex,
// InSuccessfulContractCall) are unused by decoders and not
// captured.
func TestReconstruct_RoundTripsCapture(t *testing.T) {
	t.Parallel()

	contractID := mkContractStrkey(t, 0x42)
	txHash := mkTxHashHex(0xAB)
	topicSwap := b64SV(t, symbolSV("swap"))
	topicAddr := b64SV(t, u32SV(123))
	body := b64SV(t, i128SV(big.NewInt(987654321)))
	args := []string{
		b64SV(t, symbolSV("relay")),
		b64SV(t, u32SV(99)),
	}

	original := events.Event{
		Type:           "contract",
		Ledger:         62_700_000,
		LedgerClosedAt: "2026-05-20T14:00:00Z",
		ContractID:     contractID,
		OperationIndex: 2,
		EventIndex:     4,
		TxHash:         txHash,
		Topic:          []string{topicSwap, topicAddr},
		Value:          body,
		OpArgs:         args,
	}

	row, err := Capture(original)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	roundTripped, err := Reconstruct(row)
	if err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}

	if roundTripped.Type != "contract" {
		t.Errorf("Type = %q, want contract", roundTripped.Type)
	}
	if roundTripped.Ledger != original.Ledger {
		t.Errorf("Ledger = %d, want %d", roundTripped.Ledger, original.Ledger)
	}
	if roundTripped.LedgerClosedAt != original.LedgerClosedAt {
		t.Errorf("LedgerClosedAt = %q, want %q",
			roundTripped.LedgerClosedAt, original.LedgerClosedAt)
	}
	if roundTripped.ContractID != original.ContractID {
		t.Errorf("ContractID = %q, want %q", roundTripped.ContractID, original.ContractID)
	}
	if roundTripped.OperationIndex != original.OperationIndex {
		t.Errorf("OperationIndex = %d, want %d", roundTripped.OperationIndex, original.OperationIndex)
	}
	if row.EventIndex != 4 {
		t.Errorf("Capture: row.EventIndex = %d, want 4", row.EventIndex)
	}
	if roundTripped.EventIndex != original.EventIndex {
		t.Errorf("EventIndex = %d, want %d", roundTripped.EventIndex, original.EventIndex)
	}
	if roundTripped.TxHash != original.TxHash {
		t.Errorf("TxHash = %q, want %q", roundTripped.TxHash, original.TxHash)
	}
	if len(roundTripped.Topic) != len(original.Topic) {
		t.Fatalf("Topic len = %d, want %d", len(roundTripped.Topic), len(original.Topic))
	}
	for i, b64 := range roundTripped.Topic {
		if b64 != original.Topic[i] {
			t.Errorf("Topic[%d] = %q, want %q", i, b64, original.Topic[i])
		}
	}
	if roundTripped.Value != original.Value {
		t.Errorf("Value = %q, want %q", roundTripped.Value, original.Value)
	}
	if len(roundTripped.OpArgs) != len(original.OpArgs) {
		t.Fatalf("OpArgs len = %d, want %d", len(roundTripped.OpArgs), len(original.OpArgs))
	}
	for i, b64 := range roundTripped.OpArgs {
		if b64 != original.OpArgs[i] {
			t.Errorf("OpArgs[%d] = %q, want %q", i, b64, original.OpArgs[i])
		}
	}

	// Topic[0] must parse as a Symbol — proves the base64 bytes round-tripped.
	var sv xdr.ScVal
	rawTopic0 := row.Topic0XDR
	if err := sv.UnmarshalBinary(rawTopic0); err != nil {
		t.Fatalf("Topic0XDR did not unmarshal: %v", err)
	}
	if !bytes.Equal(rawTopic0, row.Topic0XDR) {
		t.Errorf("Topic0XDR mutated during unmarshal")
	}
}

// TestReconstruct_PreservesFiveOrMoreTopics is the C2-11 regression
// guard: a Soroban event with MORE than four topics (e.g. an Aquarius
// multi-token pool event) must round-trip through Capture →
// Reconstruct with EVERY topic preserved — no >4→4 truncation.
//
// Proven-red against the pre-fix code: decodeTopics capped at a
// [4][]byte and reconstructTopics capped want>4→4, so the
// reconstructed Topic slice held only 4 entries and the len==6
// assertion below fails. The end-to-end length is the tight proof —
// Reconstruct can only emit 6 topics if Capture actually persisted all
// 6 (otherwise it falls back to the four topic_0..3 columns). Uses
// only pre-existing symbols so the assertion compiles (and fails)
// against a full revert of the fix.
func TestReconstruct_PreservesFiveOrMoreTopics(t *testing.T) {
	t.Parallel()

	// Six topics: a Symbol kind + five distinct scalar topics. Each is
	// byte-distinct so a dropped/reordered topic is detectable.
	topics := []string{
		b64SV(t, symbolSV("multi_swap")),
		b64SV(t, u32SV(11)),
		b64SV(t, u32SV(22)),
		b64SV(t, u32SV(33)),
		b64SV(t, u32SV(44)), // topic[4] — lost by the pre-fix cap
		b64SV(t, u32SV(55)), // topic[5] — lost by the pre-fix cap
	}

	original := events.Event{
		Type:           "contract",
		Ledger:         62_800_000,
		LedgerClosedAt: "2026-05-21T09:30:00Z",
		ContractID:     mkContractStrkey(t, 0x77),
		OperationIndex: 1,
		EventIndex:     3,
		TxHash:         mkTxHashHex(0xC3),
		Topic:          topics,
		Value:          b64SV(t, i128SV(big.NewInt(424242))),
	}

	row, err := Capture(original)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	// topic_count records the true arity (this was already correct
	// pre-fix — only the topic bytes were being dropped).
	if row.TopicCount != 6 {
		t.Errorf("row.TopicCount = %d, want 6", row.TopicCount)
	}
	// Back-compat: the four fixed columns still carry the first four
	// topics (existing readers + the topic_0_sym index rely on them).
	if len(row.Topic0XDR) == 0 || len(row.Topic1XDR) == 0 ||
		len(row.Topic2XDR) == 0 || len(row.Topic3XDR) == 0 {
		t.Errorf("Topic0..3XDR must stay populated for back-compat: "+
			"got lens %d,%d,%d,%d",
			len(row.Topic0XDR), len(row.Topic1XDR),
			len(row.Topic2XDR), len(row.Topic3XDR))
	}

	roundTripped, err := Reconstruct(row)
	if err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}

	// Replay-side: every topic reconstructs, in order. Pre-fix this
	// was 4 (topics 5+ silently dropped); post-fix it is all 6.
	if len(roundTripped.Topic) != len(original.Topic) {
		t.Fatalf("Reconstruct Topic len = %d, want %d (topics 5+ dropped by the >4→4 truncation)",
			len(roundTripped.Topic), len(original.Topic))
	}
	for i, b64 := range roundTripped.Topic {
		if b64 != original.Topic[i] {
			t.Errorf("Topic[%d] = %q, want %q", i, b64, original.Topic[i])
		}
	}
}

// TestReconstruct_NoOpArgs handles the common case of an event
// that didn't come from an InvokeContract op (CAP-67 classic-op
// transfer events, system events, etc.). OpArgs is nil on
// reconstruction, which the consumer expects.
func TestReconstruct_NoOpArgs(t *testing.T) {
	t.Parallel()

	original := events.Event{
		Type:           "contract",
		Ledger:         62_700_001,
		LedgerClosedAt: "2026-05-20T14:00:05Z",
		ContractID:     mkContractStrkey(t, 0x10),
		OperationIndex: 0,
		TxHash:         mkTxHashHex(0x01),
		Topic:          []string{b64SV(t, symbolSV("transfer"))},
		Value:          b64SV(t, i128SV(big.NewInt(1))),
	}

	row, err := Capture(original)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if row.OpArgsXDR != nil {
		t.Fatalf("OpArgsXDR should be nil for an event without OpArgs")
	}

	roundTripped, err := Reconstruct(row)
	if err != nil {
		t.Fatalf("Reconstruct: %v", err)
	}
	if len(roundTripped.OpArgs) != 0 {
		t.Errorf("Reconstruct() OpArgs = %v, want nil/empty", roundTripped.OpArgs)
	}
}
