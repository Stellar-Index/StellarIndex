package aquarius

import (
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/contractid"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// ─── consumer.go ──────────────────────────────────────────────────

func TestTradeEvent_implementsConsumerEvent(t *testing.T) {
	te := TradeEvent{}
	if got := te.EventKind(); got != "aquarius.trade" {
		t.Errorf("EventKind() = %q, want \"aquarius.trade\"", got)
	}
	if got := te.Source(); got != SourceName {
		t.Errorf("Source() = %q, want %q", got, SourceName)
	}
	var _ consumer.Event = te
}

// ─── dispatcher_adapter.go ────────────────────────────────────────

func TestDecoder_Name(t *testing.T) {
	if got := NewDecoder().Name(); got != SourceName {
		t.Errorf("Name() = %q, want %q", got, SourceName)
	}
}

// testPool is a synthetic pool strkey the test decoder seeds so
// Matches() gate checks pass without depending on the curated
// mainnet list (which newTestDecoder also carries, being built on
// NewDecoder).
const testPool = "C-test-pool-strkey"

// newTestDecoder mirrors phoenix's helper: production seed + one
// synthetic test pool.
func newTestDecoder() *Decoder {
	return NewDecoder(contractid.WithSeed([]string{testPool}))
}

func TestDecoder_Matches(t *testing.T) {
	d := newTestDecoder()
	tokenIn := makeContractStrkey(t, 0x01)
	tokenOut := makeContractStrkey(t, 0x02)
	user := makeAccountStrkey(t, 0x03)

	good := events.Event{
		ContractID: testPool,
		Topic: []string{
			TopicSymbolTrade,
			encodeContractAddrFromStrkey(t, tokenIn),
			encodeContractAddrFromStrkey(t, tokenOut),
			encodeAccountAddrFromStrkey(t, user),
		},
	}
	if !d.Matches(good) {
		t.Error("Matches(trade event) = false, want true")
	}

	for _, tc := range []struct {
		name string
		ev   events.Event
	}{
		{"empty topic", events.Event{ContractID: testPool}},
		{"non-trade topic[0]", events.Event{ContractID: testPool, Topic: []string{
			// "deposit" (bare) is now a recognized+gated rewards-gauge
			// topic (ROADMAP #89) so it no longer proves this case —
			// use a genuinely unclassified topic name instead.
			encodeSymbol(t, "totally_unrecognized_topic"),
			encodeContractAddrFromStrkey(t, tokenIn),
			encodeContractAddrFromStrkey(t, tokenOut),
			encodeAccountAddrFromStrkey(t, user),
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if d.Matches(tc.ev) {
				t.Errorf("Matches(%s) = true, want false", tc.name)
			}
		})
	}
}

// TestDecoder_GateRejectsForeignContract pins ADR-0035/0040 (CS-026):
// the bare Symbol("trade") 4-topic shape is forgeable — the r1 lake
// contains a parallel non-registry router deployment and a
// foreign-WASM look-alike emitting the identical shape
// (docs/protocols/aquarius.md, flagged sets). A perfect trade shape
// from an unregistered contract must NOT be attributed to aquarius,
// while the same event from a curated registry pool must.
func TestDecoder_GateRejectsForeignContract(t *testing.T) {
	d := NewDecoder() // production gate: curated registry set only
	topics := []string{
		TopicSymbolTrade,
		encodeContractAddrFromStrkey(t, makeContractStrkey(t, 0x01)),
		encodeContractAddrFromStrkey(t, makeContractStrkey(t, 0x02)),
		encodeAccountAddrFromStrkey(t, makeAccountStrkey(t, 0x03)),
	}

	foreign := events.Event{
		ContractID: "CFOREIGNFAKEPOOL0000000000000000000000000000000000000000",
		Topic:      topics,
	}
	if d.Matches(foreign) {
		t.Fatal("foreign contract with aquarius-shaped topics matched — the CS-026 injection vector is open")
	}

	genuine := events.Event{ContractID: MainnetPools[0], Topic: topics}
	if !d.Matches(genuine) {
		t.Fatal("curated registry pool failed to match — gate is over-closed")
	}
}

// TestDecoder_AddPoolRegistersNewPool pins the router fan-out
// (ADR-0035): a router add_pool announcement registers the new pool
// so its subsequent trades pass the gate; the same announcement from
// a non-router contract is rejected outright.
func TestDecoder_AddPoolRegistersNewPool(t *testing.T) {
	d := NewDecoder()
	newPool := makeContractStrkey(t, 0x7A)
	tradeTopics := []string{
		TopicSymbolTrade,
		encodeContractAddrFromStrkey(t, makeContractStrkey(t, 0x01)),
		encodeContractAddrFromStrkey(t, makeContractStrkey(t, 0x02)),
		encodeAccountAddrFromStrkey(t, makeAccountStrkey(t, 0x03)),
	}

	preGate := events.Event{ContractID: newPool, Topic: tradeTopics}
	if d.Matches(preGate) {
		t.Fatal("unannounced pool matched before the router announced it")
	}

	announce := events.Event{
		ContractID: MainnetRouter,
		Ledger:     63_000_000,
		Topic:      []string{TopicSymbolAddPool},
		Value:      encodeAddPoolBody(t, newPool),
	}
	if !d.Matches(announce) {
		t.Fatal("Matches(router add_pool) = false, want true")
	}
	out, err := d.Decode(announce)
	if err != nil {
		t.Fatalf("Decode(router add_pool): %v", err)
	}
	if len(out) != 0 {
		t.Fatalf("Decode(router add_pool) emitted %d events, want 0", len(out))
	}

	if !d.Matches(preGate) {
		t.Fatal("router-announced pool still rejected after add_pool")
	}

	// A foreign contract emitting add_pool must not register anything.
	forged := events.Event{
		ContractID: "CFOREIGNFAKEROUTER000000000000000000000000000000000000000",
		Topic:      []string{TopicSymbolAddPool},
		Value:      encodeAddPoolBody(t, makeContractStrkey(t, 0x7B)),
	}
	if d.Matches(forged) {
		t.Fatal("foreign add_pool matched — a fake router could inject pools into the gate")
	}
}

// TestDecoder_ChHashOrderCanDropRegisteredPoolTrade documents Q040
// (NEEDS-COORDINATION — the fix needs internal/projector/projector.go's
// processEventSafely, out of this unit's scope; see also
// internal/storage/clickhouse/event_reader.go's
// `ORDER BY ledger_seq, tx_hash, op_index, event_index`).
//
// The lake read that feeds the live decoder orders events within a
// ledger by tx_hash — a lexical string sort, not Stellar's actual
// intra-ledger transaction-apply order (which the lake CAN resolve, via
// stellar.tx_hash_index / clickhouse.TxIndexReader, but the projector's
// forward stream does not consult it — docs/architecture/
// contract-call-coverage-audit.md "Walker terrain check"). A genuinely
// legitimate pool creation (add_pool) and its first trade can land in
// the SAME ledger in different transactions; if the trade's tx_hash
// lexically precedes the add_pool's, the lake delivers the trade FIRST.
// Because this decoder's registry is grown live from add_pool alone
// (Decode(), self-seed), that ordering flips the trade from "will
// register successfully" to "permanently, silently dropped": Matches()
// returns false for the not-yet-registered pool and nothing downstream
// (this decoder has no logger/metrics hook) ever signals it happened.
//
// This test proves the divergence exists deterministically, at the
// Decoder alone, independent of any live ClickHouse connection: same
// two events, only the callback ORDER changes, with the same outcome
// the lake's tx_hash sort would produce.
func TestDecoder_ChHashOrderCanDropRegisteredPoolTrade(t *testing.T) {
	newPool := makeContractStrkey(t, 0x9C)
	announce := events.Event{
		ContractID: MainnetRouter,
		Ledger:     64_000_000,
		TxHash:     "b_same_ledger_pool_create", // lexically AFTER the trade's hash
		Topic:      []string{TopicSymbolAddPool},
		Value:      encodeAddPoolBody(t, newPool),
	}
	trade := events.Event{
		ContractID: newPool,
		Ledger:     64_000_000,
		TxHash:     "a_same_ledger_pool_trade", // lexically BEFORE the add_pool's hash
		Topic: []string{
			TopicSymbolTrade,
			encodeContractAddrFromStrkey(t, makeContractStrkey(t, 0x01)),
			encodeContractAddrFromStrkey(t, makeContractStrkey(t, 0x02)),
			encodeAccountAddrFromStrkey(t, makeAccountStrkey(t, 0x03)),
		},
		Value:          encodeTradeBody(t, big.NewInt(1_000_000), big.NewInt(2_000_000), big.NewInt(0)),
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	}
	if trade.TxHash >= announce.TxHash {
		t.Fatalf("test setup: want trade.TxHash < announce.TxHash lexically, got %q >= %q", trade.TxHash, announce.TxHash)
	}

	// True chronological order (add_pool really did happen first
	// on-chain — a pool must exist before it can be traded): the trade
	// matches once the pool is registered.
	chrono := NewDecoder()
	if !chrono.Matches(announce) {
		t.Fatal("chronological order: Matches(add_pool) = false, want true")
	}
	if _, err := chrono.Decode(announce); err != nil {
		t.Fatalf("chronological order: Decode(add_pool): %v", err)
	}
	if !chrono.Matches(trade) {
		t.Fatal("chronological order: Matches(trade) = false after add_pool registered it, want true")
	}

	// stellar.contract_events' ORDER BY (ledger_seq, tx_hash, op_index,
	// event_index) delivers these two SAME-LEDGER events in the
	// opposite (hash) order: trade, then add_pool. That is what the
	// live projector actually streams.
	hashOrder := NewDecoder()
	if hashOrder.Matches(trade) {
		t.Fatal("hash order: Matches(trade) = true before add_pool was ever seen — test setup invalid")
	}
	// The projector's caller treats this Matches()=false miss as
	// (0, false, nil) — no error, no count (internal/projector/
	// projector.go processEventSafely) — so this trade is now gone:
	// the cursor advances past it and it is never offered again.
	//
	// The pool DOES register once its own add_pool is later seen, but
	// that is too late for the trade that already came and went.
	if _, err := hashOrder.Decode(announce); err != nil {
		t.Fatalf("hash order: Decode(add_pool): %v", err)
	}
	if !hashOrder.Matches(trade) {
		t.Fatal("hash order: pool never registered — test setup invalid")
	}
	// The point: a real one-pass forward stream calls Matches()/Decode()
	// once per event, in stream order, and never replays a miss. The
	// registration succeeding on a SECOND look proves the trade's
	// original miss was a stream-ordering artifact, not a real
	// unregistered-pool rejection — exactly the silent loss Q040 names.
}

// TestDecoder_AddPoolMalformedBody: a router add_pool whose body
// isn't Vec[Address(contract), …] is a decode error (skip + count),
// never a registration.
func TestDecoder_AddPoolMalformedBody(t *testing.T) {
	d := NewDecoder()
	for name, body := range map[string]string{
		"not-base64":  "not-base64",
		"empty-vec":   encodeEmptyVec(t),
		"g-address":   encodeAddPoolBodyAccount(t, makeAccountStrkey(t, 0x03)),
		"i128-scalar": encodeTradeBody(t, big.NewInt(1), big.NewInt(1), big.NewInt(0)),
	} {
		t.Run(name, func(t *testing.T) {
			ev := events.Event{ContractID: MainnetRouter, Topic: []string{TopicSymbolAddPool}, Value: body}
			if _, err := d.Decode(ev); err == nil {
				t.Error("Decode(malformed add_pool body) err = nil, want error")
			}
		})
	}
}

func TestDecoder_Decode_HappyPathProducesOneTradeEvent(t *testing.T) {
	d := NewDecoder()
	tokenIn := makeContractStrkey(t, 0x01)
	tokenOut := makeContractStrkey(t, 0x02)
	user := makeAccountStrkey(t, 0x03)
	ev := events.Event{
		Topic: []string{
			TopicSymbolTrade,
			encodeContractAddrFromStrkey(t, tokenIn),
			encodeContractAddrFromStrkey(t, tokenOut),
			encodeAccountAddrFromStrkey(t, user),
		},
		Value:          encodeTradeBody(t, big.NewInt(1_000_000), big.NewInt(2_000_000), big.NewInt(0)),
		Ledger:         62_000_000,
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
	if te.Trade.Taker != user {
		t.Errorf("Trade.Taker = %q, want %q", te.Trade.Taker, user)
	}
}

func TestDecoder_Decode_MalformedClosedAtReturnsError(t *testing.T) {
	d := NewDecoder()
	tokenIn := makeContractStrkey(t, 0x01)
	tokenOut := makeContractStrkey(t, 0x02)
	user := makeAccountStrkey(t, 0x03)
	ev := events.Event{
		Topic: []string{
			TopicSymbolTrade,
			encodeContractAddrFromStrkey(t, tokenIn),
			encodeContractAddrFromStrkey(t, tokenOut),
			encodeAccountAddrFromStrkey(t, user),
		},
		Value:          encodeTradeBody(t, big.NewInt(1), big.NewInt(1), big.NewInt(0)),
		LedgerClosedAt: "not-a-timestamp",
	}
	if _, err := d.Decode(ev); err == nil {
		t.Error("expected EventClosedAt error on malformed timestamp, got nil")
	}
}

// TestDecoder_Decode_UnrecognizedKindFailsClosed pins Q041: Decode()'s
// switch must fail closed on a kind it has no explicit case for, not
// force it through decodeTrade just because the topic shape happens to
// resemble one. Before the fix, an event whose topic[0] classify()
// doesn't recognize — but which otherwise has trade-shaped topics/body —
// was silently decoded as a genuine trade (ADR-0035/CS-026 violation).
func TestDecoder_Decode_UnrecognizedKindFailsClosed(t *testing.T) {
	d := NewDecoder()
	tokenIn := makeContractStrkey(t, 0x01)
	tokenOut := makeContractStrkey(t, 0x02)
	user := makeAccountStrkey(t, 0x03)
	ev := events.Event{
		Topic: []string{
			encodeSymbol(t, "totally_unrecognized_topic"),
			encodeContractAddrFromStrkey(t, tokenIn),
			encodeContractAddrFromStrkey(t, tokenOut),
			encodeAccountAddrFromStrkey(t, user),
		},
		Value:          encodeTradeBody(t, big.NewInt(1_000_000), big.NewInt(2_000_000), big.NewInt(0)),
		Ledger:         62_000_000,
		TxHash:         "unrecognized-kind",
		OperationIndex: 0,
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	}
	if classify(&ev) != "" {
		t.Fatalf("test setup: classify() recognized the topic, want unclassified")
	}
	out, err := d.Decode(ev)
	if err == nil {
		t.Fatalf("Decode(unrecognized kind, trade-shaped) = (%v, nil), want an error — it was silently decoded as a trade", out)
	}
}

func TestDecoder_Decode_MalformedBodyReturnsError(t *testing.T) {
	d := NewDecoder()
	tokenIn := makeContractStrkey(t, 0x01)
	tokenOut := makeContractStrkey(t, 0x02)
	user := makeAccountStrkey(t, 0x03)
	ev := events.Event{
		Topic: []string{
			TopicSymbolTrade,
			encodeContractAddrFromStrkey(t, tokenIn),
			encodeContractAddrFromStrkey(t, tokenOut),
			encodeAccountAddrFromStrkey(t, user),
		},
		Value:          "not-base64",
		LedgerClosedAt: "2026-04-23T12:00:00Z",
	}
	if _, err := d.Decode(ev); err == nil {
		t.Error("expected decode error on malformed body, got nil")
	}
}
