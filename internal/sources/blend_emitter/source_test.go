package blend_emitter

import (
	"errors"
	"testing"

	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// ─── Fixture helpers ─────────────────────────────────────────────

// contractStrkeyFromSeed produces a valid C-strkey from a 32-byte
// seed so a synthetic "foreign contract" fixture passes strkey
// checksum without depending on a real mainnet address. Same pattern
// as internal/sources/comet/decode_test.go.
func contractStrkeyFromSeed(t *testing.T, tag byte) string {
	t.Helper()
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = tag ^ byte(i)
	}
	s, err := strkey.Encode(strkey.VersionByteContract, seed)
	if err != nil {
		t.Fatalf("strkey encode: %v", err)
	}
	return s
}

// ─── Real-mainnet fixtures ────────────────────────────────────────
//
// All base64 topics/body blobs below were pulled DIRECTLY from the
// certified ClickHouse raw lake (r1, HTTP :8123, 2026-07-09) — never
// synthesised — for the Emitter contract
// CCOQM6S7ICIUWA225O5PSJWUBEMXGFSSW2PQFO6FP4DQEKMS5DASRGRR. Every
// ledger / tx_hash / op_index / event_index cited is the exact
// coordinate of the real event.

const (
	// distribute: ledger 51,524,666, one of 465 lifetime occurrences.
	distributeFixtureLedger   = 51_524_666
	distributeFixtureTxHash   = "a4d9621406d8de34f8599ad6cece90ba4c404263e283873fc88ba74a57f0af00"
	distributeFixtureClosedAt = "2024-05-04T14:28:22Z"
	distributeFixtureTopic0   = "AAAADwAAAApkaXN0cmlidXRlAAA="
	distributeFixtureBody     = "AAAAEAAAAAEAAAACAAAAEgAAAAEdsBgMzWLDomvfiJ1XMLKxF9mQA8cB9wRxdmQX8+cpxAAAAAoAAAAAAAAAAAAAAVuJeBUA"
	distributeFixtureBackstop = "CAO3AGAMZVRMHITL36EJ2VZQWKYRPWMQAPDQD5YEOF3GIF7T44U4JAL3" // Backstop V1
	distributeFixtureAmount   = "1492660000000"
	distributeFixtureEventIdx = 1
	distributeFixtureOpIndex  = 0

	// drop: ledger 57,467,292 (the SMALLER of the two lifetime drop
	// events — 3 recipients; the other, ledger 51,499,914, has 13).
	dropFixtureLedger   = 57_467_292
	dropFixtureTxHash   = "fc3056698a541e6a7d5fdfc29538661c199ad2cce28e43dc5fb81d24a3c9be0e"
	dropFixtureClosedAt = "2025-06-09T14:25:53Z"
	dropFixtureTopic0   = "AAAADwAAAARkcm9w"
	dropFixtureBody     = "AAAAEAAAAAEAAAADAAAAEAAAAAEAAAACAAAAEgAAAAFDRl/9MM1sjH0fADwSVL5qLgk+Z7q50aglCbaDGgHhQQAAAAoAAAAAAAAAAAAACRhOcqAAAAAAEAAAAAEAAAACAAAAEgAAAAFL1c09wf3G44zNr3o8rMdWOn/kGIkQi0w6WkyZlrPmagAAAAoAAAAAAAAAAAAACRhOcqAAAAAAEAAAAAEAAAACAAAAEgAAAAEhCPZWDdSDZU8ORn2pnTQ6ak3qD9ICiQ1eQi7a70qibQAAAAoAAAAAAAAAAAAAKtSfDxiA"
	dropFixtureEventIdx = 3
	dropFixtureOpIndex  = 0

	// q_swap: ledger 56,992,670, the only lifetime occurrence.
	qSwapFixtureLedger   = 56_992_670
	qSwapFixtureTxHash   = "437e58132561d2502334430721caf9ecc8644fe68d61002db57b413062b71e7e"
	qSwapFixtureClosedAt = "2025-05-09T14:24:17Z"
	qSwapFixtureTopic0   = "AAAADwAAAAZxX3N3YXAAAA=="
	qSwapFixtureBody     = "AAAAEQAAAAEAAAADAAAADwAAAAxuZXdfYmFja3N0b3AAAAASAAAAASEI9lYN1INlTw5GfamdNDpqTeoP0gKJDV5CLtrvSqJtAAAADwAAABJuZXdfYmFja3N0b3BfdG9rZW4AAAAAABIAAAABJbKv015UMxpIkMNjGfee2xjweJ5H/Dh7OzDvLmmlTRoAAAAPAAAAC3VubG9ja190aW1lAAAAAAUAAAAAaEbukQ=="
	qSwapFixtureEventIdx = 10
	qSwapFixtureOpIndex  = 0

	// swap: ledger 57,467,277, the only lifetime occurrence — the
	// SAME new_backstop / new_backstop_token / unlock_time values as
	// the q_swap fixture above (this swap executed that queue).
	swapFixtureLedger   = 57_467_277
	swapFixtureTxHash   = "3e6f84e462293f5c4950fc4bb1f1bff6e03dae32df09260ba6b64a2e8a2b590c"
	swapFixtureClosedAt = "2025-06-09T14:24:28Z"
	swapFixtureTopic0   = "AAAADwAAAARzd2Fw"
	swapFixtureBody     = "AAAAEQAAAAEAAAADAAAADwAAAAxuZXdfYmFja3N0b3AAAAASAAAAASEI9lYN1INlTw5GfamdNDpqTeoP0gKJDV5CLtrvSqJtAAAADwAAABJuZXdfYmFja3N0b3BfdG9rZW4AAAAAABIAAAABJbKv015UMxpIkMNjGfee2xjweJ5H/Dh7OzDvLmmlTRoAAAAPAAAAC3VubG9ja190aW1lAAAAAAUAAAAAaEbukQ=="
	swapFixtureEventIdx = 1
	swapFixtureOpIndex  = 0

	// Shared new_backstop / new_backstop_token / unlock_time decoded
	// from both q_swap and swap fixtures above.
	swapConfigNewBackstop      = "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7" // Backstop V2
	swapConfigNewBackstopToken = "CAS3FL6TLZKDGGSISDBWGGPXT3NRR4DYTZD7YOD3HMYO6LTJUVGRVEAM" // Comet BLND/USDC backstop pool
	swapConfigUnlockTime       = 1_749_479_057
)

func mustDecoderMatchesAndDecodesOne(t *testing.T, d *Decoder, ev events.Event) consumer.Event {
	t.Helper()
	if !d.Matches(ev) {
		t.Fatalf("Matches(%s) = false, want true", ev.Topic)
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("Decode returned %d events, want 1", len(out))
	}
	return out[0]
}

func TestDecoder_Decode_DistributeRealFixture(t *testing.T) {
	d := NewDecoder()
	ev := events.Event{
		ContractID:     MainnetEmitter,
		Topic:          []string{distributeFixtureTopic0},
		Value:          distributeFixtureBody,
		Ledger:         distributeFixtureLedger,
		TxHash:         distributeFixtureTxHash,
		OperationIndex: distributeFixtureOpIndex,
		EventIndex:     distributeFixtureEventIdx,
		LedgerClosedAt: distributeFixtureClosedAt,
	}
	got := mustDecoderMatchesAndDecodesOne(t, d, ev)
	de, ok := got.(DistributeEvent)
	if !ok {
		t.Fatalf("expected DistributeEvent, got %T", got)
	}
	if de.Source() != SourceName {
		t.Errorf("Source() = %q, want %q", de.Source(), SourceName)
	}
	if de.EventKind() != "blend_emitter.distribute" {
		t.Errorf("EventKind() = %q", de.EventKind())
	}
	if de.BackstopID != distributeFixtureBackstop {
		t.Errorf("BackstopID = %q, want %q", de.BackstopID, distributeFixtureBackstop)
	}
	if de.Amount.String() != distributeFixtureAmount {
		t.Errorf("Amount = %q, want %q", de.Amount.String(), distributeFixtureAmount)
	}
	if de.Ledger != distributeFixtureLedger || de.TxHash != distributeFixtureTxHash {
		t.Errorf("identity mismatch: ledger=%d tx=%s", de.Ledger, de.TxHash)
	}
}

func TestDecoder_Decode_DropRealFixture(t *testing.T) {
	d := NewDecoder()
	ev := events.Event{
		ContractID:     MainnetEmitter,
		Topic:          []string{dropFixtureTopic0},
		Value:          dropFixtureBody,
		Ledger:         dropFixtureLedger,
		TxHash:         dropFixtureTxHash,
		OperationIndex: dropFixtureOpIndex,
		EventIndex:     dropFixtureEventIdx,
		LedgerClosedAt: dropFixtureClosedAt,
	}
	got := mustDecoderMatchesAndDecodesOne(t, d, ev)
	drop, ok := got.(DropEvent)
	if !ok {
		t.Fatalf("expected DropEvent, got %T", got)
	}
	wantRecipients := []Recipient{
		{Address: "CBBUMX75GDGWZDD5D4ADYESUXZVC4CJ6M65LTUNIEUE3NAY2AHQUDDUN", Amount: mustAmount(t, "10000000000000")},
		{Address: "CBF5LTJ5YH64NY4MZWXXUPFMY5LDU77EDCERBC2MHJNEZGMWWPTGU6O7", Amount: mustAmount(t, "10000000000000")},
		{Address: "CAQQR5SWBXKIGZKPBZDH3KM5GQ5GUTPKB7JAFCINLZBC5WXPJKRG3IM7", Amount: mustAmount(t, "47092690000000")},
	}
	if len(drop.Recipients) != len(wantRecipients) {
		t.Fatalf("Recipients len = %d, want %d", len(drop.Recipients), len(wantRecipients))
	}
	for i, want := range wantRecipients {
		got := drop.Recipients[i]
		if got.Address != want.Address {
			t.Errorf("Recipients[%d].Address = %q, want %q", i, got.Address, want.Address)
		}
		if got.Amount.String() != want.Amount.String() {
			t.Errorf("Recipients[%d].Amount = %q, want %q", i, got.Amount.String(), want.Amount.String())
		}
	}
}

func TestDecoder_Decode_QSwapRealFixture(t *testing.T) {
	d := NewDecoder()
	ev := events.Event{
		ContractID:     MainnetEmitter,
		Topic:          []string{qSwapFixtureTopic0},
		Value:          qSwapFixtureBody,
		Ledger:         qSwapFixtureLedger,
		TxHash:         qSwapFixtureTxHash,
		OperationIndex: qSwapFixtureOpIndex,
		EventIndex:     qSwapFixtureEventIdx,
		LedgerClosedAt: qSwapFixtureClosedAt,
	}
	got := mustDecoderMatchesAndDecodesOne(t, d, ev)
	sc, ok := got.(SwapConfigEvent)
	if !ok {
		t.Fatalf("expected SwapConfigEvent, got %T", got)
	}
	if sc.Kind != SwapConfigQueued {
		t.Errorf("Kind = %q, want %q", sc.Kind, SwapConfigQueued)
	}
	assertSwapConfigFields(t, sc)
}

func TestDecoder_Decode_SwapRealFixture(t *testing.T) {
	d := NewDecoder()
	ev := events.Event{
		ContractID:     MainnetEmitter,
		Topic:          []string{swapFixtureTopic0},
		Value:          swapFixtureBody,
		Ledger:         swapFixtureLedger,
		TxHash:         swapFixtureTxHash,
		OperationIndex: swapFixtureOpIndex,
		EventIndex:     swapFixtureEventIdx,
		LedgerClosedAt: swapFixtureClosedAt,
	}
	got := mustDecoderMatchesAndDecodesOne(t, d, ev)
	sc, ok := got.(SwapConfigEvent)
	if !ok {
		t.Fatalf("expected SwapConfigEvent, got %T", got)
	}
	if sc.Kind != SwapConfigExecuted {
		t.Errorf("Kind = %q, want %q", sc.Kind, SwapConfigExecuted)
	}
	assertSwapConfigFields(t, sc)

	// The one observed swap executed exactly what the one observed
	// q_swap queued — same target/timelock. Pin this cross-fixture
	// invariant so a future decode-path regression that scrambles
	// field order is caught here, not just per-kind.
	if sc.NewBackstop != swapConfigNewBackstop || sc.NewBackstopToken != swapConfigNewBackstopToken || sc.UnlockTime != swapConfigUnlockTime {
		t.Fatalf("swap fields diverge from the q_swap fixture: %+v", sc)
	}
}

func assertSwapConfigFields(t *testing.T, sc SwapConfigEvent) {
	t.Helper()
	if sc.NewBackstop != swapConfigNewBackstop {
		t.Errorf("NewBackstop = %q, want %q", sc.NewBackstop, swapConfigNewBackstop)
	}
	if sc.NewBackstopToken != swapConfigNewBackstopToken {
		t.Errorf("NewBackstopToken = %q, want %q", sc.NewBackstopToken, swapConfigNewBackstopToken)
	}
	if sc.UnlockTime != swapConfigUnlockTime {
		t.Errorf("UnlockTime = %d, want %d", sc.UnlockTime, swapConfigUnlockTime)
	}
	if sc.Source() != SourceName {
		t.Errorf("Source() = %q, want %q", sc.Source(), SourceName)
	}
	if sc.EventKind() != "blend_emitter.swap_config" {
		t.Errorf("EventKind() = %q", sc.EventKind())
	}
}

// ─── classify() ────────────────────────────────────────────────────

func TestClassify(t *testing.T) {
	cases := []struct {
		name  string
		topic []string
		want  string
	}{
		{"distribute", []string{distributeFixtureTopic0}, EventDistribute},
		{"drop", []string{dropFixtureTopic0}, EventDrop},
		{"q_swap", []string{qSwapFixtureTopic0}, EventQSwap},
		{"swap", []string{swapFixtureTopic0}, EventSwap},
		{"empty topic", nil, ""},
		{"unknown symbol", []string{scval.MustEncodeSymbol("unknown")}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := events.Event{Topic: tc.topic}
			if got := classify(&ev); got != tc.want {
				t.Errorf("classify(%v) = %q, want %q", tc.topic, got, tc.want)
			}
		})
	}
}

// ─── Reject paths ────────────────────────────────────────────────

func TestDecoder_Decode_MalformedBodyReturnsError(t *testing.T) {
	d := NewDecoder()
	cases := []struct {
		name  string
		topic string
		body  string
	}{
		{"distribute not base64", distributeFixtureTopic0, "not-base64"},
		{"drop not base64", dropFixtureTopic0, "not-base64"},
		{"q_swap not base64", qSwapFixtureTopic0, "not-base64"},
		// distribute body with wrong Vec arity (reuse the drop body,
		// a 3-element Vec instead of the expected 2).
		{"distribute wrong arity", distributeFixtureTopic0, dropFixtureBody},
		// drop body whose outer Vec is empty is covered by
		// TestDecoder_Decode_DropEmptyRecipientsRejected below.
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ev := events.Event{
				ContractID:     MainnetEmitter,
				Topic:          []string{tc.topic},
				Value:          tc.body,
				LedgerClosedAt: distributeFixtureClosedAt,
			}
			_, err := d.Decode(ev)
			if err == nil {
				t.Fatal("expected decode error, got nil")
			}
		})
	}
}

func TestDecoder_Decode_DropEmptyRecipientsRejected(t *testing.T) {
	// AAAAEAAAAAA= = ScvVec with a zero-length vec.
	ev := events.Event{
		ContractID:     MainnetEmitter,
		Topic:          []string{dropFixtureTopic0},
		Value:          "AAAAEAAAAAA=",
		LedgerClosedAt: dropFixtureClosedAt,
	}
	d := NewDecoder()
	_, err := d.Decode(ev)
	if !errors.Is(err, ErrEmptyRecipients) {
		t.Fatalf("Decode err = %v, want ErrEmptyRecipients", err)
	}
}

func TestDecoder_Decode_UnknownTopicReturnsErrNotEmitterEvent(t *testing.T) {
	d := NewDecoder()
	ev := events.Event{
		ContractID: MainnetEmitter,
		Topic:      []string{"AAAADwAAAAV1bmtub3duAA=="}, // Symbol("unknow") — not a real Emitter topic
	}
	_, err := d.Decode(ev)
	if !errors.Is(err, ErrNotEmitterEvent) {
		t.Fatalf("Decode err = %v, want ErrNotEmitterEvent", err)
	}
}

func TestDecoder_Decode_FailsClosedOnMissingClosedAt(t *testing.T) {
	// LedgerClosedAt deliberately empty — the adapter must FAIL
	// CLOSED (return the error) rather than substitute time.Now(),
	// matching the comet/blend/phoenix/defindex siblings.
	d := NewDecoder()
	ev := events.Event{
		ContractID: MainnetEmitter,
		Topic:      []string{distributeFixtureTopic0},
		Value:      distributeFixtureBody,
	}
	out, err := d.Decode(ev)
	if err == nil {
		t.Fatalf("Decode: expected error on empty LedgerClosedAt, got nil (out=%v)", out)
	}
	if out != nil {
		t.Errorf("Decode: expected nil events on error, got %v", out)
	}
}

// ─── Gate (ADR-0035/0040) ──────────────────────────────────────────

// TestDecoder_GateRejectsForeignContract pins the load-bearing
// invariant this whole package exists to enforce: `distribute`
// COLLIDES with blend_backstop's own `distribute` topic (see
// events.go's package doc), so a perfectly-shaped `distribute` event
// from a contract that is NOT the curated Emitter must NOT be
// attributed to blend_emitter — while the same event from the
// genuine Emitter must be. Decode itself stays shape-only; the gate
// lives in Matches, which the dispatcher consults first.
func TestDecoder_GateRejectsForeignContract(t *testing.T) {
	d := NewDecoder() // production gate: in-code curated set only
	foreign := events.Event{
		ContractID:     contractStrkeyFromSeed(t, 0xFF),
		Topic:          []string{distributeFixtureTopic0},
		Value:          distributeFixtureBody,
		Ledger:         distributeFixtureLedger,
		TxHash:         "non-emitter-tx",
		OperationIndex: 0,
		LedgerClosedAt: distributeFixtureClosedAt,
	}
	if d.Matches(foreign) {
		t.Fatal("foreign contract with emitter-shaped topic matched — the distribute/blend_backstop collision is unguarded")
	}

	genuine := foreign
	genuine.ContractID = MainnetEmitter
	if !d.Matches(genuine) {
		t.Fatal("curated Emitter contract failed to match — gate is over-closed")
	}

	// The gate composes: a caller-supplied WithSeed (the
	// protocol_contracts warm — the operator seam for admitting a
	// future Emitter instance without a redeploy) must admit it.
	admitted := contractStrkeyFromSeed(t, 0xAB)
	d2 := NewDecoder(contractid.WithSeed([]string{admitted}))
	ev2 := genuine
	ev2.ContractID = admitted
	if !d2.Matches(ev2) {
		t.Fatal("operator-seeded contract failed to match — the protocol_contracts warm seam is broken")
	}
}

func TestDecoder_Name(t *testing.T) {
	d := NewDecoder()
	if got := d.Name(); got != SourceName {
		t.Errorf("Name() = %q, want %q", got, SourceName)
	}
}

// ─── consumer.Event compile/runtime checks ─────────────────────────

func TestConsumerEventTypes_ImplementInterface(t *testing.T) {
	var (
		_ consumer.Event = DistributeEvent{}
		_ consumer.Event = DropEvent{}
		_ consumer.Event = SwapConfigEvent{}
	)
	if (DistributeEvent{}).Source() != SourceName {
		t.Error("DistributeEvent.Source() mismatch")
	}
	if (DropEvent{}).Source() != SourceName {
		t.Error("DropEvent.Source() mismatch")
	}
	if (SwapConfigEvent{}).Source() != SourceName {
		t.Error("SwapConfigEvent.Source() mismatch")
	}
}

// ─── helpers ────────────────────────────────────────────────────────

func mustAmount(t *testing.T, s string) canonical.Amount {
	t.Helper()
	a, err := canonical.FromString(s)
	if err != nil {
		t.Fatalf("amount %q: %v", s, err)
	}
	return a
}
