package sorobanevents

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// ─── SCVal encode helpers ───────────────────────────────────────
// Mirrored from internal/sources/cctp/decode_test.go's helpers so
// this package's tests are self-contained.

func symbolSV(s string) xdr.ScVal {
	sym := xdr.ScSymbol(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvSymbol, Sym: &sym}
}

func stringSV(s string) xdr.ScVal {
	st := xdr.ScString(s)
	return xdr.ScVal{Type: xdr.ScValTypeScvString, Str: &st}
}

func u32SV(v uint32) xdr.ScVal {
	x := xdr.Uint32(v)
	return xdr.ScVal{Type: xdr.ScValTypeScvU32, U32: &x}
}

func i128SV(n *big.Int) xdr.ScVal {
	// Reuses the cctp test's two-half split semantics; for positive
	// values within int64 range this is straightforward.
	parts := xdr.Int128Parts{
		Hi: xdr.Int64(0),
		Lo: xdr.Uint64(n.Uint64()),
	}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &parts}
}

func bytesSV(b []byte) xdr.ScVal {
	bb := xdr.ScBytes(b)
	return xdr.ScVal{Type: xdr.ScValTypeScvBytes, Bytes: &bb}
}

func b64SV(t *testing.T, sv xdr.ScVal) string {
	t.Helper()
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.StdEncoding.EncodeToString(b)
}

func mkContractStrkey(t *testing.T, seed byte) string {
	t.Helper()
	var raw [32]byte
	raw[0] = seed
	s, err := strkey.Encode(strkey.VersionByteContract, raw[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	return s
}

func mkTxHashHex(seed byte) string {
	var h [32]byte
	for i := range h {
		h[i] = seed
	}
	return hex.EncodeToString(h[:])
}

// ─── tests ──────────────────────────────────────────────────────

// TestCapture_SymbolTopic verifies the common Symbol-topic[0] case
// — a "swap" event from a fake contract — sets topic_0_sym and
// preserves the XDR bytes.
func TestCapture_SymbolTopic(t *testing.T) {
	t.Parallel()

	contractID := mkContractStrkey(t, 0x42)
	txHash := mkTxHashHex(0xAB)

	topicSwap := b64SV(t, symbolSV("swap"))
	topicAddrLike := b64SV(t, u32SV(123))
	body := b64SV(t, i128SV(big.NewInt(1_000_000)))

	ev := events.Event{
		Type:           "contract",
		Ledger:         62_700_000,
		LedgerClosedAt: "2026-05-20T14:00:00Z",
		ContractID:     contractID,
		OperationIndex: 2,
		TxHash:         txHash,
		Topic:          []string{topicSwap, topicAddrLike},
		Value:          body,
	}

	row, err := Capture(ev)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	if row.ContractID != contractID {
		t.Errorf("ContractID = %q, want %q", row.ContractID, contractID)
	}
	if len(row.ContractIDHex) != 32 {
		t.Errorf("ContractIDHex len = %d, want 32", len(row.ContractIDHex))
	}
	if row.Topic0Sym != "swap" {
		t.Errorf("Topic0Sym = %q, want %q", row.Topic0Sym, "swap")
	}
	if row.TopicCount != 2 {
		t.Errorf("TopicCount = %d, want 2", row.TopicCount)
	}
	// Topic0XDR must be the base64-decoded raw bytes.
	want0, _ := base64.StdEncoding.DecodeString(topicSwap)
	if !bytes.Equal(row.Topic0XDR, want0) {
		t.Errorf("Topic0XDR mismatch — got %x, want %x", row.Topic0XDR, want0)
	}
	want1, _ := base64.StdEncoding.DecodeString(topicAddrLike)
	if !bytes.Equal(row.Topic1XDR, want1) {
		t.Errorf("Topic1XDR mismatch — got %x, want %x", row.Topic1XDR, want1)
	}
	if row.Topic2XDR != nil {
		t.Errorf("Topic2XDR = %x, want nil", row.Topic2XDR)
	}
	if row.Topic3XDR != nil {
		t.Errorf("Topic3XDR = %x, want nil", row.Topic3XDR)
	}
	if len(row.BodyXDR) == 0 {
		t.Errorf("BodyXDR is empty")
	}
	if row.OpArgsXDR != nil {
		t.Errorf("OpArgsXDR = %x, want nil (no OpArgs on this event)", row.OpArgsXDR)
	}

	// TxHash must round-trip through hex.
	wantHash, _ := hex.DecodeString(txHash)
	if !bytes.Equal(row.TxHash, wantHash) {
		t.Errorf("TxHash mismatch — got %x, want %x", row.TxHash, wantHash)
	}
	if row.OpIndex != 2 {
		t.Errorf("OpIndex = %d, want 2", row.OpIndex)
	}
	if row.Ledger != 62_700_000 {
		t.Errorf("Ledger = %d, want 62700000", row.Ledger)
	}
}

// TestCapture_EventIndexDistinguishesSameOpEvents guards against silent loss: without event_index threading, a
// multi-event op (Phoenix emits 8 per swap) collides on the (ledger, tx_hash, op_index, event_index) PK and ON
// CONFLICT DO NOTHING drops all but the first.
func TestCapture_EventIndexDistinguishesSameOpEvents(t *testing.T) {
	t.Parallel()

	contractID := mkContractStrkey(t, 0x55)
	txHash := mkTxHashHex(0x99)
	body := b64SV(t, i128SV(big.NewInt(1)))

	type pk struct {
		op, ev int16
	}
	seen := make(map[pk]bool)

	// One operation (op_index=0), eight events — the Phoenix shape.
	for evIdx := 0; evIdx < 8; evIdx++ {
		ev := events.Event{
			Type:           "contract",
			Ledger:         62_800_000,
			LedgerClosedAt: "2026-05-21T00:00:00Z",
			ContractID:     contractID,
			OperationIndex: 0,
			EventIndex:     evIdx,
			TxHash:         txHash,
			Topic:          []string{b64SV(t, symbolSV("swap"))},
			Value:          body,
		}
		row, err := Capture(ev)
		if err != nil {
			t.Fatalf("Capture(evIdx=%d): %v", evIdx, err)
		}
		if int(row.EventIndex) != evIdx {
			t.Errorf("evIdx=%d: row.EventIndex = %d, want %d", evIdx, row.EventIndex, evIdx)
		}
		key := pk{op: row.OpIndex, ev: row.EventIndex}
		if seen[key] {
			t.Errorf("evIdx=%d: PK collision on (op=%d, event=%d) — multi-event op would drop rows",
				evIdx, key.op, key.ev)
		}
		seen[key] = true
	}
	if len(seen) != 8 {
		t.Errorf("got %d distinct (op, event) PKs, want 8", len(seen))
	}
}

// TestCapture_StringTopic verifies that a String-typed topic[0] is
// also surfaced through topic_0_sym (the convenience column covers
// both Symbol and String).
func TestCapture_StringTopic(t *testing.T) {
	t.Parallel()

	contractID := mkContractStrkey(t, 0x10)
	txHash := mkTxHashHex(0x01)

	topicStr := b64SV(t, stringSV("some-string-topic"))
	body := b64SV(t, u32SV(0))

	ev := events.Event{
		Type:           "contract",
		Ledger:         50_000_000,
		LedgerClosedAt: "2025-12-01T00:00:00Z",
		ContractID:     contractID,
		TxHash:         txHash,
		Topic:          []string{topicStr},
		Value:          body,
	}
	row, err := Capture(ev)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if row.Topic0Sym != "some-string-topic" {
		t.Errorf("Topic0Sym = %q, want %q", row.Topic0Sym, "some-string-topic")
	}
}

// TestCapture_NULStringTopic: a contract-supplied String topic carrying a NUL byte must not be
// stored raw, because Postgres text rejects NUL (SQLSTATE 22021) and would poison the whole
// ingest batch. Topic0Sym must be the scval.ToText hex-escaped form.
func TestCapture_NULStringTopic(t *testing.T) {
	t.Parallel()

	contractID := mkContractStrkey(t, 0x20)
	txHash := mkTxHashHex(0x02)

	raw := "swap\x00evil"
	topicStr := b64SV(t, stringSV(raw))
	body := b64SV(t, u32SV(0))

	ev := events.Event{
		Type:           "contract",
		Ledger:         50_000_001,
		LedgerClosedAt: "2025-12-01T00:00:00Z",
		ContractID:     contractID,
		TxHash:         txHash,
		Topic:          []string{topicStr},
		Value:          body,
	}
	row, err := Capture(ev)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	want := scval.ToText(raw)
	if want == raw {
		t.Fatalf("test setup broken: ToText did not transform the NUL-bearing input")
	}
	if row.Topic0Sym != want {
		t.Errorf("Topic0Sym = %q, want %q (scval.ToText form)", row.Topic0Sym, want)
	}
	if strings.Contains(row.Topic0Sym, "\x00") {
		t.Errorf("Topic0Sym still contains a NUL byte, would hit SQLSTATE 22021: %q", row.Topic0Sym)
	}
}

// TestCapture_NonSymbolTopic — when topic[0] is neither Symbol nor
// String, topic_0_sym must be "" (sink writes SQL NULL) but the
// raw XDR must still be captured.
func TestCapture_NonSymbolTopic(t *testing.T) {
	t.Parallel()

	contractID := mkContractStrkey(t, 0x77)
	txHash := mkTxHashHex(0x77)

	topic0 := b64SV(t, u32SV(0xDEADBEEF)) // U32 — neither Symbol nor String
	body := b64SV(t, bytesSV([]byte("opaque-body")))

	ev := events.Event{
		Type:           "contract",
		Ledger:         60_000_000,
		LedgerClosedAt: "2026-03-01T00:00:00Z",
		ContractID:     contractID,
		TxHash:         txHash,
		Topic:          []string{topic0},
		Value:          body,
	}
	row, err := Capture(ev)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if row.Topic0Sym != "" {
		t.Errorf("Topic0Sym = %q, want \"\" for non-Symbol/non-String topic[0]", row.Topic0Sym)
	}
	// But the raw bytes must still be preserved.
	want, _ := base64.StdEncoding.DecodeString(topic0)
	if !bytes.Equal(row.Topic0XDR, want) {
		t.Errorf("Topic0XDR mismatch — got %x, want %x", row.Topic0XDR, want)
	}
	if len(row.BodyXDR) == 0 {
		t.Errorf("BodyXDR empty for non-Symbol topic case")
	}
}

// TestCapture_OpArgsRoundTrip pins that when events.Event.OpArgs is set (InvokeContract-emitted events), OpArgsXDR
// is the XDR-marshalled ScVec of those args and round-trips to the input ScVals.
func TestCapture_OpArgsRoundTrip(t *testing.T) {
	t.Parallel()

	contractID := mkContractStrkey(t, 0xFE)
	txHash := mkTxHashHex(0xEF)

	topic0 := b64SV(t, symbolSV("write_prices"))
	body := b64SV(t, bytesSV([]byte("body-bytes")))

	// Three args — typical for RedStone's write_prices(updater,
	// feed_ids, payload) shape.
	arg0 := b64SV(t, u32SV(100))
	arg1 := b64SV(t, symbolSV("feed_a"))
	arg2 := b64SV(t, bytesSV([]byte("payload")))

	ev := events.Event{
		Type:           "contract",
		Ledger:         62_500_000,
		LedgerClosedAt: "2026-05-15T00:00:00Z",
		ContractID:     contractID,
		OperationIndex: 0,
		TxHash:         txHash,
		Topic:          []string{topic0},
		Value:          body,
		OpArgs:         []string{arg0, arg1, arg2},
	}
	row, err := Capture(ev)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if row.OpArgsXDR == nil {
		t.Fatalf("OpArgsXDR is nil — expected non-nil for OpArgs-bearing event")
	}

	// Round-trip: unmarshal the OpArgsXDR back to ScVal, verify
	// it's a ScvVec with three elements matching the inputs.
	var sv xdr.ScVal
	if err := sv.UnmarshalBinary(row.OpArgsXDR); err != nil {
		t.Fatalf("UnmarshalBinary(OpArgsXDR): %v", err)
	}
	if sv.Type != xdr.ScValTypeScvVec {
		t.Fatalf("OpArgsXDR ScVal type = %s, want ScvVec", sv.Type)
	}
	vecPtr := sv.Vec
	if vecPtr == nil || *vecPtr == nil {
		t.Fatalf("OpArgsXDR vec is nil")
	}
	vec := **vecPtr
	if len(vec) != 3 {
		t.Fatalf("OpArgsXDR vec len = %d, want 3", len(vec))
	}
	// Element 0 must be a U32 == 100.
	if vec[0].Type != xdr.ScValTypeScvU32 || vec[0].U32 == nil || uint32(*vec[0].U32) != 100 {
		t.Errorf("arg0 round-trip failed; got %+v", vec[0])
	}
	// Element 1 must be a Symbol == "feed_a".
	if vec[1].Type != xdr.ScValTypeScvSymbol || vec[1].Sym == nil || string(*vec[1].Sym) != "feed_a" {
		t.Errorf("arg1 round-trip failed; got %+v", vec[1])
	}
	// Element 2 must be Bytes("payload").
	if vec[2].Type != xdr.ScValTypeScvBytes || vec[2].Bytes == nil || string(*vec[2].Bytes) != "payload" {
		t.Errorf("arg2 round-trip failed; got %+v", vec[2])
	}
}

// TestCapture_NonContractEvent: system and diagnostic events must return ErrSkip rather than a
// row. This is defence in depth, since the dispatcher already filters them in
// contractEventToEventsEvent.
func TestCapture_NonContractEvent(t *testing.T) {
	t.Parallel()
	_, err := Capture(events.Event{
		Type:       "diagnostic",
		ContractID: mkContractStrkey(t, 0x42),
		Topic:      []string{b64SV(t, symbolSV("debug"))},
		Value:      b64SV(t, u32SV(0)),
	})
	if !errors.Is(err, ErrSkip) {
		t.Errorf("Capture(non-contract) = %v, want ErrSkip", err)
	}
}

// TestCapture_EmptyContractID — defence: an event with an empty
// ContractID must return ErrSkip rather than producing a row with
// junk contract_id_hex.
func TestCapture_EmptyContractID(t *testing.T) {
	t.Parallel()
	_, err := Capture(events.Event{
		Type:           "contract",
		ContractID:     "",
		LedgerClosedAt: "2026-04-01T00:00:00Z",
		Topic:          []string{b64SV(t, symbolSV("x"))},
		Value:          b64SV(t, u32SV(0)),
	})
	if !errors.Is(err, ErrSkip) {
		t.Errorf("Capture(empty ContractID) = %v, want ErrSkip", err)
	}
}

// TestCapture_NoTopics — defence: zero-topic events would violate
// the migration's NOT NULL constraint on topic_0_xdr; must skip.
func TestCapture_NoTopics(t *testing.T) {
	t.Parallel()
	_, err := Capture(events.Event{
		Type:           "contract",
		ContractID:     mkContractStrkey(t, 1),
		LedgerClosedAt: "2026-04-01T00:00:00Z",
		Topic:          nil,
		Value:          b64SV(t, u32SV(0)),
	})
	if !errors.Is(err, ErrSkip) {
		t.Errorf("Capture(no topics) = %v, want ErrSkip", err)
	}
}

// TestCapture_FourTopics — events with exactly four topics must
// populate Topic0XDR..Topic3XDR all.
func TestCapture_FourTopics(t *testing.T) {
	t.Parallel()

	contractID := mkContractStrkey(t, 0xAB)
	txHash := mkTxHashHex(0xCD)

	topics := []string{
		b64SV(t, symbolSV("event")),
		b64SV(t, u32SV(1)),
		b64SV(t, u32SV(2)),
		b64SV(t, u32SV(3)),
	}
	ev := events.Event{
		Type:           "contract",
		Ledger:         60_000_000,
		LedgerClosedAt: "2026-04-01T00:00:00Z",
		ContractID:     contractID,
		TxHash:         txHash,
		Topic:          topics,
		Value:          b64SV(t, u32SV(0)),
	}
	row, err := Capture(ev)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if row.TopicCount != 4 {
		t.Errorf("TopicCount = %d, want 4", row.TopicCount)
	}
	if row.Topic0XDR == nil || row.Topic1XDR == nil || row.Topic2XDR == nil || row.Topic3XDR == nil {
		t.Errorf("expected all four Topic*XDR populated; got 0=%v 1=%v 2=%v 3=%v",
			row.Topic0XDR != nil, row.Topic1XDR != nil, row.Topic2XDR != nil, row.Topic3XDR != nil)
	}
}

// TestReconstruct_RoundTripsCapture proves Capture to Reconstruct is loss-free for the fields decoders need
// (ContractID, OpIndex, TxHash, Topic[0..N], Value, OpArgs); ID, TransactionIndex and InSuccessfulContractCall are
// unused by decoders and not captured.
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

// TestReconstruct_PreservesFiveOrMoreTopics pins that an event with more than four topics (e.g. an Aquarius
// multi-token pool event) round-trips with every topic, no 4-topic truncation; len==6 proves Capture persisted all
// 6, not just the topic_0..3 columns.
func TestReconstruct_PreservesFiveOrMoreTopics(t *testing.T) {
	t.Parallel()

	// Six topics: a Symbol kind + five distinct scalar topics. Each is
	// byte-distinct so a dropped/reordered topic is detectable.
	topics := []string{
		b64SV(t, symbolSV("multi_swap")),
		b64SV(t, u32SV(11)),
		b64SV(t, u32SV(22)),
		b64SV(t, u32SV(33)),
		b64SV(t, u32SV(44)), // topic[4] — lost by a 4-topic cap
		b64SV(t, u32SV(55)), // topic[5] — lost by a 4-topic cap
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

	// topic_count records the true arity (only the topic bytes are at
	// risk of being dropped).
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

	// Replay-side: every topic reconstructs, in order. A capped
	// decoder gives 4 (topics 5+ silently dropped); it must be all 6.
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

// TestReconstruct_TopicGapErrors pins that a row whose topics_xdr has an empty slot before a non-empty one fails
// closed, instead of silently shifting the later topic down a position.
func TestReconstruct_TopicGapErrors(t *testing.T) {
	t.Parallel()

	swap, err := symbolSV("swap").MarshalBinary()
	if err != nil {
		t.Fatalf("marshal swap topic: %v", err)
	}
	extra, err2 := u32SV(7).MarshalBinary()
	if err2 != nil {
		t.Fatalf("marshal extra topic: %v", err2)
	}

	var txHash [32]byte
	txHash[0] = 0xD4

	row := Row{
		Ledger:     62_900_000,
		ContractID: mkContractStrkey(t, 0x55),
		TxHash:     txHash[:],
		TopicCount: 3,
		Topic0XDR:  swap,
		TopicsXDR:  [][]byte{swap, {}, extra}, // gap at index 1
	}

	_, rerr := Reconstruct(row)
	if rerr == nil {
		t.Fatal("Reconstruct() err = nil, want error for a topic gap")
	}
}

// TestReconstruct_NoOpArgs pins the common case of an event not from an InvokeContract op (CAP-67 classic-op
// transfer events, system events): OpArgs is nil on reconstruction, as the consumer expects.
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
