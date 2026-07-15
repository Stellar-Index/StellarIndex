package comet

import (
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// ─── consumer.go ──────────────────────────────────────────────────

func TestTradeEvent_implementsConsumerEvent(t *testing.T) {
	te := TradeEvent{}
	if got := te.EventKind(); got != "comet.trade" {
		t.Errorf("EventKind() = %q, want \"comet.trade\"", got)
	}
	if got := te.Source(); got != SourceName {
		t.Errorf("Source() = %q, want %q", got, SourceName)
	}
	// Compile-time check is in consumer.go (var _ consumer.Event = TradeEvent{}),
	// but assert at runtime too so a future field rename can't quietly drop it.
	var _ consumer.Event = te
}

// ─── dispatcher_adapter.go ────────────────────────────────────────

func TestDecoder_Name(t *testing.T) {
	d := NewDecoder()
	if got := d.Name(); got != SourceName {
		t.Errorf("Name() = %q, want %q", got, SourceName)
	}
}

func TestDecoder_Matches_TopicShape(t *testing.T) {
	d := NewDecoder()

	// Topic shape + registered pool (the in-code curated set).
	swap := events.Event{ContractID: MainnetBackstopPool, Topic: []string{TopicSymbolPool, TopicSymbolSwap}}
	if !d.Matches(swap) {
		t.Error("Matches((POOL, swap) from curated pool) = false, want true")
	}

	// Wrong topic[0]: not a pool event, even from the curated pool.
	other := events.Event{ContractID: MainnetBackstopPool, Topic: []string{TopicSymbolSwap, TopicSymbolPool}}
	if d.Matches(other) {
		t.Error("Matches((swap, POOL)) = true, want false (wrong topic order)")
	}

	empty := events.Event{ContractID: MainnetBackstopPool, Topic: nil}
	if d.Matches(empty) {
		t.Error("Matches(empty topic) = true, want false")
	}
}

func TestDecoder_Decode_HappyPathProducesOneTradeEvent(t *testing.T) {
	d := NewDecoder()
	caller := accountStrkeyFromSeed(t, 0x10)
	tokenIn := contractStrkeyFromSeed(t, 0x20)
	tokenOut := contractStrkeyFromSeed(t, 0x30)
	body := encodeSwapBody(t, caller, tokenIn, tokenOut,
		big.NewInt(1_000_000), big.NewInt(2_500_000))
	ev := events.Event{
		Topic:          []string{TopicSymbolPool, TopicSymbolSwap},
		Value:          body,
		Ledger:         52_000_000,
		TxHash:         "deadbeef",
		OperationIndex: 0,
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
	te, ok := out[0].(TradeEvent)
	if !ok {
		t.Fatalf("expected TradeEvent, got %T", out[0])
	}
	if te.Trade.Source != SourceName {
		t.Errorf("Trade.Source = %q, want %q", te.Trade.Source, SourceName)
	}
	wantBase, _ := canonical.NewSorobanAsset(tokenIn)
	if !te.Trade.Pair.Base.Equal(wantBase) {
		t.Errorf("Pair.Base = %+v, want %+v", te.Trade.Pair.Base, wantBase)
	}
}

func TestDecoder_Decode_MalformedBodyReturnsError(t *testing.T) {
	d := NewDecoder()
	ev := events.Event{
		Topic: []string{TopicSymbolPool, TopicSymbolSwap},
		Value: "not-base64",
	}
	_, err := d.Decode(ev)
	if err == nil {
		t.Error("expected decode error on malformed body, got nil")
	}
}

// TestDecoder_GateRejectsForeignContract pins ADR-0035/0040 (CS-026,
// closed 2026-07-08 — this is the FLIP of the former
// TestDecoder_Decode_NoContractIDDiscrimination, whose comment
// required exactly this inversion): Comet's `("POOL", "swap")` topic
// shape is the Balancer-v1 contract event family, shared by EVERY
// deployment of that WASM (F-1242) — forgeable by construction. A
// perfect swap shape from an unregistered contract must NOT be
// attributed to comet, while the same event from the curated pool
// (Blend's backstop) must. Decode itself remains shape-only — the
// gate lives in Matches, which the dispatcher consults first.
func TestDecoder_GateRejectsForeignContract(t *testing.T) {
	d := NewDecoder() // production gate: in-code curated set only
	caller := accountStrkeyFromSeed(t, 0x10)
	tokenIn := contractStrkeyFromSeed(t, 0x20)
	tokenOut := contractStrkeyFromSeed(t, 0x30)
	body := encodeSwapBody(t, caller, tokenIn, tokenOut,
		big.NewInt(1_000_000), big.NewInt(2_500_000))

	// Synthetic event from a contract that is NOT the curated Blend
	// backstop pool — same topic shape, same body shape, different
	// emitting contract.
	foreign := events.Event{
		Topic:          []string{TopicSymbolPool, TopicSymbolSwap},
		Value:          body,
		ContractID:     contractStrkeyFromSeed(t, 0xFF),
		Ledger:         52_000_000,
		TxHash:         "non-blend-tx",
		OperationIndex: 0,
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	}
	if d.Matches(foreign) {
		t.Fatal("foreign contract with comet-shaped topics matched — the CS-026 injection vector is open")
	}

	genuine := foreign
	genuine.ContractID = MainnetBackstopPool
	if !d.Matches(genuine) {
		t.Fatal("curated backstop pool failed to match — gate is over-closed")
	}

	// The gate composes: a caller-supplied WithSeed (the
	// protocol_contracts warm — the operator seam for admitting a
	// future pool without a redeploy) must admit a new pool.
	admitted := contractStrkeyFromSeed(t, 0xAB)
	d2 := NewDecoder(contractid.WithSeed([]string{admitted}))
	ev2 := genuine
	ev2.ContractID = admitted
	if !d2.Matches(ev2) {
		t.Fatal("operator-seeded pool failed to match — the protocol_contracts warm seam is broken")
	}
}

func TestDecoder_Decode_FailsClosedOnMissingClosedAt(t *testing.T) {
	// LedgerClosedAt is empty — the adapter must FAIL CLOSED (return
	// the error) rather than substituting time.Now(). A wall-clock
	// fallback would mis-timestamp the row during a backfill replay,
	// stamping every event with the replay run's time instead of the
	// historical ledger close. Matches the blend/phoenix/defindex
	// siblings. Production always populates LedgerClosedAt.
	d := NewDecoder()
	caller := accountStrkeyFromSeed(t, 0x10)
	tokenIn := contractStrkeyFromSeed(t, 0x20)
	tokenOut := contractStrkeyFromSeed(t, 0x30)
	body := encodeSwapBody(t, caller, tokenIn, tokenOut,
		big.NewInt(100), big.NewInt(200))
	ev := events.Event{
		Topic: []string{TopicSymbolPool, TopicSymbolSwap},
		Value: body,
		// LedgerClosedAt deliberately empty.
	}
	out, err := d.Decode(ev)
	if err == nil {
		t.Fatalf("Decode: expected an error on empty LedgerClosedAt, got nil (out=%v)", out)
	}
	if out != nil {
		t.Errorf("Decode: expected nil events on error, got %v", out)
	}
}
