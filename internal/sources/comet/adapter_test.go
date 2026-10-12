package comet

import (
	"math/big"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
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

// TestDecoder_Decode_SelfPairSwap_NoEventNoError pins that a self-pair swap (token_in == token_out, the exploit
// primitive) maps to zero rows and Decode returns (nil, nil), not an error, which would make completeness re-derive
// count each as an undecodable blind spot and hold `comet` complete=false forever.
func TestDecoder_Decode_SelfPairSwap_NoEventNoError(t *testing.T) {
	d := NewDecoder()
	caller := accountStrkeyFromSeed(t, 0x10)
	token := contractStrkeyFromSeed(t, 0x20) // SAME token in AND out
	body := encodeSwapBody(t, caller, token, token,
		big.NewInt(1_000_000), big.NewInt(900_000))
	ev := events.Event{
		Topic:          []string{TopicSymbolPool, TopicSymbolSwap},
		Value:          body,
		Ledger:         64_112_340, // the exploit's first ledger
		TxHash:         "deadbeef",
		OperationIndex: 0,
		LedgerClosedAt: "2026-08-25T03:51:02Z",
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("self-pair swap must NOT error (an error fails the completeness verdict closed): %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("self-pair swap must produce zero events, got %d", len(out))
	}
}

// mustTime parses an RFC3339 timestamp for the recency-gate clock injection.
func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	tm, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("mustTime(%q): %v", s, err)
	}
	return tm
}

// TestDecoder_Decode_SelfPairSwap_IncrementsDetectionMetric pins that a dropped self-pair swap
// bumps obs.AMMSelfPairSwapTotal{source="comet"}, so the burst is observable although the row
// never reaches the served trades table. Detection only; the (nil, nil) drop is unchanged.
func TestDecoder_Decode_SelfPairSwap_IncrementsDetectionMetric(t *testing.T) {
	d := NewDecoder()
	// Pin wall-clock just after the event's close time so the recency gate
	// admits it — the LIVE case, where a self-pair swap is a real signal.
	d.now = func() time.Time { return mustTime(t, "2026-08-25T03:56:02Z") } // +5m
	caller := accountStrkeyFromSeed(t, 0x10)
	token := contractStrkeyFromSeed(t, 0x20) // SAME token in AND out
	body := encodeSwapBody(t, caller, token, token,
		big.NewInt(1_000_000), big.NewInt(900_000))
	ev := events.Event{
		Topic:          []string{TopicSymbolPool, TopicSymbolSwap},
		Value:          body,
		Ledger:         64_112_340,
		TxHash:         "deadbeef",
		OperationIndex: 0,
		LedgerClosedAt: "2026-08-25T03:51:02Z",
	}
	before := testutil.ToFloat64(obs.AMMSelfPairSwapTotal.WithLabelValues(SourceName))
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("self-pair swap must not error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("self-pair swap must still produce zero events, got %d", len(out))
	}
	after := testutil.ToFloat64(obs.AMMSelfPairSwapTotal.WithLabelValues(SourceName))
	if after-before != 1 {
		t.Fatalf("AMMSelfPairSwapTotal{comet} must increment by 1 on a live self-pair swap, got delta %v", after-before)
	}
}

// TestDecoder_Decode_SelfPairSwap_ReplayDoesNotIncrement: a backfill or completeness re-derive
// re-runs Decode over HISTORICAL ledgers, so the recency gate must NOT bump the metric there, or
// the burst alert would re-fire in present time for a non-event. The (nil, nil) drop is unchanged.
func TestDecoder_Decode_SelfPairSwap_ReplayDoesNotIncrement(t *testing.T) {
	d := NewDecoder()
	// Wall-clock ~26h after the event's close time — the replay/re-derive case.
	d.now = func() time.Time { return mustTime(t, "2026-08-26T06:00:00Z") }
	caller := accountStrkeyFromSeed(t, 0x10)
	token := contractStrkeyFromSeed(t, 0x20)
	body := encodeSwapBody(t, caller, token, token,
		big.NewInt(1_000_000), big.NewInt(900_000))
	ev := events.Event{
		Topic:          []string{TopicSymbolPool, TopicSymbolSwap},
		Value:          body,
		Ledger:         64_112_340,
		TxHash:         "deadbeef",
		LedgerClosedAt: "2026-08-25T03:51:02Z",
	}
	before := testutil.ToFloat64(obs.AMMSelfPairSwapTotal.WithLabelValues(SourceName))
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("replayed self-pair swap must not error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("replayed self-pair swap must still produce zero events, got %d", len(out))
	}
	after := testutil.ToFloat64(obs.AMMSelfPairSwapTotal.WithLabelValues(SourceName))
	if after != before {
		t.Fatalf("a replayed (stale-close-time) self-pair swap must NOT bump the metric; got delta %v", after-before)
	}
}

// TestDecoder_Decode_NonPositiveAmounts_IncrementsDetectionMetric pins that a cleanly decoded swap with a zero or
// negative amount yields zero rows and bumps obs.AMMNonPositiveSwapTotal{source="comet"}, so "armed but quiet" differs
// from "never wired".
func TestDecoder_Decode_NonPositiveAmounts_IncrementsDetectionMetric(t *testing.T) {
	d := NewDecoder()
	d.now = func() time.Time { return mustTime(t, "2026-08-25T03:56:02Z") } // +5m, LIVE case
	caller := accountStrkeyFromSeed(t, 0x10)
	tokenIn := contractStrkeyFromSeed(t, 0x20)
	tokenOut := contractStrkeyFromSeed(t, 0x30)
	body := encodeSwapBody(t, caller, tokenIn, tokenOut,
		big.NewInt(1_000_000), big.NewInt(0)) // AmountOut == 0
	ev := events.Event{
		Topic:          []string{TopicSymbolPool, TopicSymbolSwap},
		Value:          body,
		Ledger:         64_112_340,
		TxHash:         "deadbeef",
		OperationIndex: 0,
		LedgerClosedAt: "2026-08-25T03:51:02Z",
	}
	before := testutil.ToFloat64(obs.AMMNonPositiveSwapTotal.WithLabelValues(SourceName))
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("non-positive-amount swap must not error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("non-positive-amount swap must produce zero events, got %d", len(out))
	}
	after := testutil.ToFloat64(obs.AMMNonPositiveSwapTotal.WithLabelValues(SourceName))
	if after-before != 1 {
		t.Fatalf("AMMNonPositiveSwapTotal{comet} must increment by 1 on a live non-positive-amount swap, got delta %v", after-before)
	}
}

// TestDecoder_Decode_NonPositiveAmounts_ReplayDoesNotIncrement pins the recency gate: reconciliation_catalogue.go and
// verify_decoders.go replay history with their own un-disabled comet.NewDecoder(), and without the gate every
// completeness sweep would inflate this counter.
func TestDecoder_Decode_NonPositiveAmounts_ReplayDoesNotIncrement(t *testing.T) {
	d := NewDecoder()
	d.now = func() time.Time { return mustTime(t, "2026-08-26T06:00:00Z") } // ~26h later, replay case
	caller := accountStrkeyFromSeed(t, 0x10)
	tokenIn := contractStrkeyFromSeed(t, 0x20)
	tokenOut := contractStrkeyFromSeed(t, 0x30)
	body := encodeSwapBody(t, caller, tokenIn, tokenOut,
		big.NewInt(1_000_000), big.NewInt(0))
	ev := events.Event{
		Topic:          []string{TopicSymbolPool, TopicSymbolSwap},
		Value:          body,
		Ledger:         64_112_340,
		TxHash:         "deadbeef",
		LedgerClosedAt: "2026-08-25T03:51:02Z",
	}
	before := testutil.ToFloat64(obs.AMMNonPositiveSwapTotal.WithLabelValues(SourceName))
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("replayed non-positive-amount swap must not error: %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("replayed non-positive-amount swap must still produce zero events, got %d", len(out))
	}
	after := testutil.ToFloat64(obs.AMMNonPositiveSwapTotal.WithLabelValues(SourceName))
	if after != before {
		t.Fatalf("a replayed (stale-close-time) non-positive-amount swap must NOT bump the metric; got delta %v", after-before)
	}
}

// TestDecoder_WithoutMetrics_SuppressesBothDetectionCounters pins that WithoutMetrics suppresses BOTH counters while
// the (nil, nil) drop is unchanged; the projector builds an independent comet.Decoder beside the dispatcher's
// (ADR-0032 double-write), so detections would count twice.
func TestDecoder_WithoutMetrics_SuppressesBothDetectionCounters(t *testing.T) {
	d := NewDecoder().WithoutMetrics()
	d.now = func() time.Time { return mustTime(t, "2026-08-25T03:56:02Z") } // LIVE case
	caller := accountStrkeyFromSeed(t, 0x10)
	token := contractStrkeyFromSeed(t, 0x20)

	selfPairBody := encodeSwapBody(t, caller, token, token, big.NewInt(1_000_000), big.NewInt(900_000))
	selfPairEv := events.Event{
		Topic:          []string{TopicSymbolPool, TopicSymbolSwap},
		Value:          selfPairBody,
		Ledger:         64_112_340,
		TxHash:         "deadbeef",
		LedgerClosedAt: "2026-08-25T03:51:02Z",
	}
	beforeSelfPair := testutil.ToFloat64(obs.AMMSelfPairSwapTotal.WithLabelValues(SourceName))
	if out, err := d.Decode(selfPairEv); err != nil || len(out) != 0 {
		t.Fatalf("self-pair swap: got (%v, %v), want (0 events, nil)", out, err)
	}
	if after := testutil.ToFloat64(obs.AMMSelfPairSwapTotal.WithLabelValues(SourceName)); after != beforeSelfPair {
		t.Fatalf("WithoutMetrics() must suppress AMMSelfPairSwapTotal; got delta %v", after-beforeSelfPair)
	}

	tokenOut := contractStrkeyFromSeed(t, 0x30)
	nonPosBody := encodeSwapBody(t, caller, token, tokenOut, big.NewInt(1_000_000), big.NewInt(0))
	nonPosEv := events.Event{
		Topic:          []string{TopicSymbolPool, TopicSymbolSwap},
		Value:          nonPosBody,
		Ledger:         64_112_341,
		TxHash:         "cafef00d",
		LedgerClosedAt: "2026-08-25T03:51:02Z",
	}
	beforeNonPos := testutil.ToFloat64(obs.AMMNonPositiveSwapTotal.WithLabelValues(SourceName))
	if out, err := d.Decode(nonPosEv); err != nil || len(out) != 0 {
		t.Fatalf("non-positive-amount swap: got (%v, %v), want (0 events, nil)", out, err)
	}
	if after := testutil.ToFloat64(obs.AMMNonPositiveSwapTotal.WithLabelValues(SourceName)); after != beforeNonPos {
		t.Fatalf("WithoutMetrics() must suppress AMMNonPositiveSwapTotal; got delta %v", after-beforeNonPos)
	}
}

// TestDecoder_Decode_HappyPath_NoDetectionMetric is the false-positive guard: a normal distinct-pair swap must NOT
// bump the exploit metric, or the self-pair alert would be noise.
func TestDecoder_Decode_HappyPath_NoDetectionMetric(t *testing.T) {
	d := NewDecoder()
	caller := accountStrkeyFromSeed(t, 0x10)
	tokenIn := contractStrkeyFromSeed(t, 0x20)
	tokenOut := contractStrkeyFromSeed(t, 0x30) // DISTINCT tokens
	body := encodeSwapBody(t, caller, tokenIn, tokenOut,
		big.NewInt(1_000_000), big.NewInt(2_500_000))
	ev := events.Event{
		Topic:          []string{TopicSymbolPool, TopicSymbolSwap},
		Value:          body,
		Ledger:         52_000_000,
		TxHash:         "cafef00d",
		OperationIndex: 0,
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	}
	before := testutil.ToFloat64(obs.AMMSelfPairSwapTotal.WithLabelValues(SourceName))
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("happy-path swap must not error: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("happy-path swap must produce one trade, got %d", len(out))
	}
	after := testutil.ToFloat64(obs.AMMSelfPairSwapTotal.WithLabelValues(SourceName))
	if after != before {
		t.Fatalf("a normal swap must NOT bump the exploit metric; got delta %v", after-before)
	}
}

// TestDecoder_GateRejectsForeignContract pins ADR-0035/0040: Comet's `("POOL", "swap")` topic is shared by every
// deployment of that WASM and so forgeable. A perfect swap from an unregistered contract must not be attributed to
// comet, while the curated pool (Blend's backstop) must. Decode is shape-only; the gate is in Matches.
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
	// With LedgerClosedAt empty the adapter must FAIL CLOSED (return the error), not substitute
	// time.Now(), which would stamp backfill replays with the replay run's time. Matches the
	// blend/phoenix/defindex siblings; production always populates it.
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

func TestDecoder_Matches_AllFivePoolKinds(t *testing.T) {
	d := NewDecoder()
	for _, topic1 := range []string{
		TopicSymbolSwap, TopicSymbolJoinPool, TopicSymbolExitPool,
		TopicSymbolDeposit, TopicSymbolWithdraw,
	} {
		ev := events.Event{ContractID: MainnetBackstopPool, Topic: []string{TopicSymbolPool, topic1}}
		if !d.Matches(ev) {
			t.Errorf("Matches((POOL, %q)) = false, want true", topic1)
		}
	}
}

func TestDecoder_Matches_RejectsUnknownPoolKind(t *testing.T) {
	// A hypothetical upgrade emitting (POOL, "bind") must not be claimed until the decoder handles it, keeping the
	// orphan-events counter honest and surfacing the schema drift.
	d := NewDecoder()
	bind := mustEncodeSymbolForTest(t, "bind")
	ev := events.Event{Topic: []string{TopicSymbolPool, bind}}
	if d.Matches(ev) {
		t.Error("Matches((POOL, bind)) = true, want false (unknown kind)")
	}
}

func TestDecoder_Decode_JoinPoolProducesLiquidityEvent(t *testing.T) {
	d := NewDecoder()
	caller := accountStrkeyFromSeed(t, 0x60)
	token := contractStrkeyFromSeed(t, 0x61)
	amount := big.NewInt(900_000)

	body := encodeLiquidityBody(t, caller, token, "token_in", "token_amount_in", amount)
	ev := events.Event{
		Topic:          []string{TopicSymbolPool, TopicSymbolJoinPool},
		Value:          body,
		Ledger:         52_000_500,
		TxHash:         "beef01",
		OperationIndex: 1,
		LedgerClosedAt: "2026-05-26T12:00:00Z",
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	le, ok := out[0].(LiquidityEvent)
	if !ok {
		t.Fatalf("got %T, want LiquidityEvent", out[0])
	}
	if le.Kind != LiquidityJoinPool {
		t.Errorf("Kind = %q, want %q", le.Kind, LiquidityJoinPool)
	}
	if le.EventKind() != "comet.liquidity" {
		t.Errorf("EventKind() = %q, want comet.liquidity", le.EventKind())
	}
	if le.Source() != SourceName {
		t.Errorf("Source() = %q, want %q", le.Source(), SourceName)
	}
}

func TestDecoder_Decode_WithdrawProducesLiquidityEventWithPoolAmount(t *testing.T) {
	d := NewDecoder()
	caller := accountStrkeyFromSeed(t, 0x62)
	token := contractStrkeyFromSeed(t, 0x63)
	body := encodeWithdrawBody(t, caller, token, big.NewInt(1_000), big.NewInt(50))
	ev := events.Event{
		Topic:          []string{TopicSymbolPool, TopicSymbolWithdraw},
		Value:          body,
		LedgerClosedAt: "2026-05-26T12:00:00Z",
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	le := out[0].(LiquidityEvent)
	if le.PoolAmountIn.BigInt().Int64() != 50 {
		t.Errorf("PoolAmountIn = %s, want 50", le.PoolAmountIn)
	}
}

// TestDecoder_Decode_SwapStillProducesTradeEvent ensures the
// adapter's broadened Matches predicate doesn't accidentally route
// swap events through the liquidity path.
func TestDecoder_Decode_SwapStillProducesTradeEvent(t *testing.T) {
	d := NewDecoder()
	caller := accountStrkeyFromSeed(t, 0x64)
	tokenIn := contractStrkeyFromSeed(t, 0x65)
	tokenOut := contractStrkeyFromSeed(t, 0x66)
	body := encodeSwapBody(t, caller, tokenIn, tokenOut,
		big.NewInt(1_000_000), big.NewInt(2_500_000))
	ev := events.Event{
		Topic:          []string{TopicSymbolPool, TopicSymbolSwap},
		Value:          body,
		LedgerClosedAt: "2026-05-26T12:00:00Z",
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	if _, ok := out[0].(TradeEvent); !ok {
		t.Errorf("got %T, want TradeEvent", out[0])
	}
}

func TestDecoder_Decode_UnknownPoolKindReturnsErrNotCometEvent(t *testing.T) {
	d := NewDecoder()
	bind := mustEncodeSymbolForTest(t, "bind")
	ev := events.Event{Topic: []string{TopicSymbolPool, bind}}
	_, err := d.Decode(ev)
	if err == nil {
		t.Error("expected error on unknown (POOL, bind), got nil")
	}
}
