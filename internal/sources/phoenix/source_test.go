package phoenix

import (
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// mustClosed is the test-side equivalent of the processPage
// `e.EventClosedAt() + bail on error` guard. Test fixtures always
// have well-formed RFC 3339 timestamps, so parse failure here is a
// fixture bug.
func mustClosed(t *testing.T, e *events.Event) time.Time {
	t.Helper()
	ts, err := e.EventClosedAt()
	if err != nil {
		t.Fatalf("fixture LedgerClosedAt %q: %v", e.LedgerClosedAt, err)
	}
	return ts
}

const (
	phoenixTxHash = "fadefadefadefadefadefadefadefadefadefadefadefadefadefadefadefade"
	testAddress   = "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN"
	xlmSAC        = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	usdcContract  = "CAQCFVLOBK5GIULPNZRGATJJMIZL5BSP7X5YJVMGCVZLMIDLVJELAVIF"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name       string
		topics     []string
		wantField  string
		wantIsSwap bool
	}{
		{"swap sender", []string{TopicSymbolSwap, TopicSymbolSender}, TopicSymbolSender, true},
		{"swap buy_token", []string{TopicSymbolSwap, TopicSymbolBuyToken}, TopicSymbolBuyToken, true},
		{"not swap", []string{"something_else", TopicSymbolSender}, "", false},
		{"too few topics", []string{TopicSymbolSwap}, "", false},
		{"empty topics", []string{}, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			field, isSwap := classify(&events.Event{Topic: tc.topics})
			if isSwap != tc.wantIsSwap {
				t.Errorf("isSwap = %v, want %v", isSwap, tc.wantIsSwap)
			}
			if field != tc.wantField {
				t.Errorf("field = %q, want %q", field, tc.wantField)
			}
		})
	}
}

func TestRawSwapCompleteCount(t *testing.T) {
	var r RawSwap
	if r.Complete() || r.fieldsPresent() != 0 {
		t.Fatal("zero value should be empty")
	}
	r.Sender = &events.Event{}
	r.BuyToken = &events.Event{}
	if r.Complete() {
		t.Fatal("2/8 should not be complete")
	}
	if r.fieldsPresent() != 2 {
		t.Errorf("fieldsPresent = %d, want 2", r.fieldsPresent())
	}
}

func TestBufferCollectsEightFieldsInOrder(t *testing.T) {
	buf := newBuffer()
	events := allEightSwapEvents()

	var completed *RawSwap
	for i, e := range events {
		fieldTopic := e.Topic[1]
		got, _, err := buf.absorb(e, fieldTopic, mustClosed(t, e))
		if err != nil {
			t.Fatalf("event %d absorb: %v", i, err)
		}
		if i < 7 && got != nil {
			t.Fatalf("got complete after %d/8 events", i+1)
		}
		if i == 7 {
			completed = got
		}
	}
	if completed == nil {
		t.Fatal("8th event should have completed the RawSwap")
	}
	if !completed.Complete() {
		t.Fatal("returned RawSwap reports itself incomplete")
	}
	if len(buf.m) != 0 {
		t.Errorf("buffer should be empty after completion, has %d", len(buf.m))
	}
}

func TestBufferHandlesOutOfOrderArrival(t *testing.T) {
	// Arrive in reverse of contract emission order (Q1: we don't
	// rely on order).
	buf := newBuffer()
	events := allEightSwapEvents()
	var completed *RawSwap
	for i := len(events) - 1; i >= 0; i-- {
		e := events[i]
		got, _, _ := buf.absorb(e, e.Topic[1], mustClosed(t, e))
		if i == 0 {
			completed = got
		}
	}
	if completed == nil {
		t.Fatal("out-of-order arrival should still complete on 8th event")
	}
	if !completed.Complete() {
		t.Fatal("completed RawSwap reports itself incomplete")
	}
}

func TestBufferSeparatesSwapsByGroupKey(t *testing.T) {
	// Two independent swaps in the same ledger but different
	// op_index — the 8-field correlation must not collide them.
	buf := newBuffer()
	evsA := allEightSwapEventsKeyed(100, "txA", 0)
	evsB := allEightSwapEventsKeyed(100, "txB", 0)

	// Interleave: one from A, one from B, repeat.
	var completedA, completedB *RawSwap
	for i := 0; i < 8; i++ {
		eA := evsA[i]
		got, _, _ := buf.absorb(eA, eA.Topic[1], mustClosed(t, eA))
		if i == 7 {
			completedA = got
		}
		eB := evsB[i]
		got, _, _ = buf.absorb(eB, eB.Topic[1], mustClosed(t, eB))
		if i == 7 {
			completedB = got
		}
	}
	if completedA == nil || completedB == nil {
		t.Fatalf("both swaps should complete: A=%v B=%v", completedA != nil, completedB != nil)
	}
	if completedA.TxHash != "txA" || completedB.TxHash != "txB" {
		t.Errorf("identity mixed: A.TxHash=%q B.TxHash=%q", completedA.TxHash, completedB.TxHash)
	}
}

// TestDecoderSeparatesMultiPoolSameOpSwaps pins the multi-hop-router
// data-corruption bug both auditors found independently: two DIFFERENT
// pool contracts emitting swaps within the SAME (ledger, tx_hash,
// op_index) must reassemble into SEPARATE buffer entries. Before
// ContractID was part of groupKey, an INCOMPLETE leg from pool A (a
// lost field-event, or an upgraded WASM emitting <8 fields) shared a
// buffer slot with pool B keyed only on (ledger,tx,op): pool B's
// field-events hijacked pool A's entry in place, so the completing
// event produced ONE Frankenstein trade — pool A's amounts + one of
// pool B's tokens, stamped with pool A's frozen first-field EventIndex
// (wrong FanoutOpIndex) — while pool A's leg was silently lost.
//
// This drives the FULL Decoder path (absorb → decodeSwap → TradeEvent)
// so it asserts the corrected tokens/amounts/op_index — not merely that
// something completed. The interleave is chosen so the un-fixed code is
// unambiguously red: pool A is missing SellToken and pool B emits
// SellToken FIRST, so without the ContractID key pool B's SellToken
// completes pool A's stub into a mixed trade.
func TestDecoderSeparatesMultiPoolSameOpSwaps(t *testing.T) {
	// Distinct assets per pool so contamination is observable.
	poolAbuy, err := canonical.NewClassicAsset("EURC", testAddress)
	if err != nil {
		t.Fatal(err)
	}
	poolBsell := canonical.NativeAsset()
	poolBbuy, err := canonical.NewClassicAsset("USDC", testAddress)
	if err != nil {
		t.Fatal(err)
	}

	prevAddr, prevAsset, prevI128 := decodeAddress, decodeAsset, decodeI128
	defer func() { decodeAddress, decodeAsset, decodeI128 = prevAddr, prevAsset, prevI128 }()
	decodeAddress = func(string) (string, error) { return "GTAKER", nil }
	decodeAsset = func(v string) (canonical.Asset, error) {
		switch v {
		case "asset:xlm":
			return poolBsell, nil
		case "asset:usdc":
			return poolBbuy, nil
		case "asset:eurc":
			return poolAbuy, nil
		}
		t.Fatalf("fake decodeAsset: unexpected body %q", v)
		return canonical.Asset{}, nil
	}
	decodeI128 = func(v string) (canonical.Amount, error) {
		switch v {
		case "i128:1000":
			return canonical.NewAmount(big.NewInt(1000)), nil
		case "i128:2000":
			return canonical.NewAmount(big.NewInt(2000)), nil
		case "i128:111":
			return canonical.NewAmount(big.NewInt(111)), nil
		case "i128:222":
			return canonical.NewAmount(big.NewInt(222)), nil
		case "i128:1", "i128:2", "i128:5", "i128:7":
			return canonical.NewAmount(big.NewInt(1)), nil
		}
		t.Fatalf("fake decodeI128: unexpected body %q", v)
		return canonical.NewAmount(big.NewInt(0)), nil
	}

	const (
		poolA = "CBHCRSVX3ZZ7EGTSYMKPEFGZNWRVCSESQR3UABET4MIW52N4EVU6BIZX" // MainnetPools[0]
		poolB = "CBCZGGNOEUZG4CAAE7TGTQQHETZMKUT4OIPFHHPKEUX46U4KXBBZ3GLH" // MainnetPools[1]
		op    = 4
	)
	closedAt := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC).Format(time.RFC3339)
	mk := func(pool string, evtIdx int, field, value string) events.Event {
		return events.Event{
			Ledger: 63_000_000, TxHash: phoenixTxHash, OperationIndex: op,
			EventIndex:     evtIdx,
			LedgerClosedAt: closedAt,
			ContractID:     pool,
			Topic:          []string{TopicSymbolSwap, field},
			Value:          value,
		}
	}

	// Pool A: 7 of 8 fields — SellToken never arrives (lost / <8-field WASM).
	// Pool B: all 8, SellToken FIRST (idx 7) so the un-fixed code hijacks A.
	feed := []events.Event{
		mk(poolA, 0, TopicSymbolSender, "addr:A"),
		mk(poolA, 1, TopicSymbolOfferAmount, "i128:1000"),
		mk(poolA, 2, TopicSymbolActualReceived, "i128:1000"),
		mk(poolA, 3, TopicSymbolBuyToken, "asset:eurc"),
		mk(poolA, 4, TopicSymbolReturnAmount, "i128:2000"),
		mk(poolA, 5, TopicSymbolSpreadAmount, "i128:5"),
		mk(poolA, 6, TopicSymbolReferralFee, "i128:1"),

		mk(poolB, 7, TopicSymbolSellToken, "asset:xlm"),
		mk(poolB, 8, TopicSymbolSender, "addr:B"),
		mk(poolB, 9, TopicSymbolOfferAmount, "i128:111"),
		mk(poolB, 10, TopicSymbolActualReceived, "i128:111"),
		mk(poolB, 11, TopicSymbolBuyToken, "asset:usdc"),
		mk(poolB, 12, TopicSymbolReturnAmount, "i128:222"),
		mk(poolB, 13, TopicSymbolSpreadAmount, "i128:7"),
		mk(poolB, 14, TopicSymbolReferralFee, "i128:2"),
	}

	d := NewDecoder() // curated set includes both pools; Decode doesn't re-gate
	var trades []canonical.Trade
	for _, ev := range feed {
		out, derr := d.Decode(ev)
		if derr != nil {
			t.Fatalf("Decode(%s idx%d): %v", ev.ContractID, ev.EventIndex, derr)
		}
		for _, ce := range out {
			te, ok := ce.(TradeEvent)
			if !ok {
				t.Fatalf("unexpected event type %T", ce)
			}
			trades = append(trades, te.Trade)
		}
	}

	// Exactly one swap completes: pool B's. Pool A's leg stays incomplete
	// (SellToken never arrived) and must NOT be resurrected as a mongrel.
	if len(trades) != 1 {
		t.Fatalf("expected exactly 1 completed trade (pool B), got %d: %+v", len(trades), trades)
	}
	got := trades[0]

	// Pool B's OWN tokens — a Frankenstein carries pool A's buy token (EURC).
	if !got.Pair.Quote.Equal(poolBbuy) {
		t.Errorf("Quote = %v, want pool B buy token %v — pool A's buy token contaminated the trade",
			got.Pair.Quote, poolBbuy)
	}
	if !got.Pair.Base.Equal(poolBsell) {
		t.Errorf("Base = %v, want pool B sell token %v", got.Pair.Base, poolBsell)
	}
	// Pool B's OWN amounts — a Frankenstein carries pool A's 1000/2000.
	if got.BaseAmount.Cmp(canonical.NewAmount(big.NewInt(111))) != 0 {
		t.Errorf("BaseAmount = %s, want 111 (pool B) — pool A's offer_amount leaked", got.BaseAmount)
	}
	if got.QuoteAmount.Cmp(canonical.NewAmount(big.NewInt(222))) != 0 {
		t.Errorf("QuoteAmount = %s, want 222 (pool B) — pool A's return_amount leaked", got.QuoteAmount)
	}
	// Pool B's OWN first-field event index (7), not pool A's frozen 0.
	wantOp := canonical.FanoutOpIndex(op, 7)
	if got.OpIndex != wantOp {
		t.Errorf("OpIndex = %d, want %d (pool B's own first-field event_index); frozen to pool A's would give %d",
			got.OpIndex, wantOp, canonical.FanoutOpIndex(op, 0))
	}

	// Pool A's incomplete leg is preserved as its OWN orphan — not silently
	// consumed into pool B's entry.
	orphans := d.buf.orphans()
	if len(orphans) != 1 {
		t.Fatalf("expected exactly 1 orphan (pool A's incomplete leg), got %d", len(orphans))
	}
	if orphans[0].Pool != poolA {
		t.Errorf("orphan Pool = %q, want pool A %q — the incomplete leg was hijacked/lost", orphans[0].Pool, poolA)
	}
	if orphans[0].fieldsPresent() != 7 {
		t.Errorf("orphan fieldsPresent = %d, want 7 (pool A's 7/8)", orphans[0].fieldsPresent())
	}
}

func TestBufferOrphansReportIncompletes(t *testing.T) {
	buf := newBuffer()
	events := allEightSwapEvents()
	// Only absorb 5 of the 8 — the other 3 never arrive.
	for _, e := range events[:5] {
		_, _, _ = buf.absorb(e, e.Topic[1], mustClosed(t, e))
	}
	orphans := buf.orphans()
	if len(orphans) != 1 {
		t.Fatalf("expected 1 orphan, got %d", len(orphans))
	}
	if orphans[0].Complete() {
		t.Fatal("orphan should not report Complete")
	}
	if orphans[0].fieldsPresent() != 5 {
		t.Errorf("orphan fieldsPresent = %d, want 5", orphans[0].fieldsPresent())
	}
}

func TestBufferRejectsUnknownField(t *testing.T) {
	buf := newBuffer()
	e := &events.Event{
		Ledger: 1, TxHash: phoenixTxHash, OperationIndex: 0,
		LedgerClosedAt: time.Now().UTC().Format(time.RFC3339),
		Topic:          []string{TopicSymbolSwap, "nonexistent_field"},
	}
	_, _, err := buf.absorb(e, "nonexistent_field", mustClosed(t, e))
	if err == nil {
		t.Fatal("expected ErrUnknownField for nonsense topic")
	}
}

func TestBufferEvictsStaleOrphans(t *testing.T) {
	buf := newBuffer()
	buf.maxAge = 100 * time.Millisecond

	// Seed an old partial swap (only 1 field arrived — classic
	// orphan). Its ClosedAt is well past the cutoff.
	oldTS := time.Now().UTC().Add(-time.Second).Format(time.RFC3339)
	events := allEightSwapEvents()
	old := events[0]
	old.LedgerClosedAt = oldTS

	if _, evicted, err := buf.absorb(old, old.Topic[1], mustClosed(t, old)); err != nil || len(evicted) != 0 {
		t.Fatalf("first insert: err=%v evicted=%d", err, len(evicted))
	}
	if buf.size() != 1 {
		t.Fatalf("buffer size = %d, want 1", buf.size())
	}

	// Fresh event from a different swap triggers sweepStale → evict.
	fresh := allEightSwapEventsKeyed(999, "txFresh", 0)[0]
	_, evicted, _ := buf.absorb(fresh, fresh.Topic[1], mustClosed(t, fresh))
	if len(evicted) != 1 {
		t.Fatalf("expected 1 eviction, got %d", len(evicted))
	}
	if buf.size() != 1 {
		t.Errorf("buffer size after eviction = %d, want 1 (fresh only)", buf.size())
	}
}

func TestDecodeSwap_happyPath(t *testing.T) {
	// Install fakes for the SCVal decoders so we can synthesise a
	// complete swap + decode it without the real XDR codec.
	prevAddr, prevAsset, prevI128 := decodeAddress, decodeAsset, decodeI128
	defer func() {
		decodeAddress, decodeAsset, decodeI128 = prevAddr, prevAsset, prevI128
	}()

	xlm := canonical.NativeAsset()
	usdc, err := canonical.NewClassicAsset("USDC", testAddress)
	if err != nil {
		t.Fatal(err)
	}

	decodeAddress = func(v string) (string, error) { return "GSENDER", nil }
	decodeAsset = func(v string) (canonical.Asset, error) {
		switch v {
		case "sell":
			return xlm, nil
		case "buy":
			return usdc, nil
		}
		return canonical.Asset{}, nil
	}
	decodeI128 = func(v string) (canonical.Amount, error) {
		switch v {
		case "offer":
			return canonical.NewAmount(big.NewInt(1_000_000_000)), nil // 100 XLM sold (base, input)
		case "received":
			// "actual received amount" is the INPUT the pool received of
			// sell_token (== offer), NOT the output — must NOT be the quote.
			return canonical.NewAmount(big.NewInt(1_000_000_000)), nil
		case "return":
			return canonical.NewAmount(big.NewInt(12_420_000)), nil // 12.42 USDC received (quote, output)
		}
		return canonical.NewAmount(big.NewInt(0)), nil
	}

	now := time.Now().UTC().Truncate(time.Second)
	r := &RawSwap{
		Ledger: 52_430_001, TxHash: phoenixTxHash, OpIndex: 0,
		Pool: usdcContract, ClosedAt: now,
		Sender:         &events.Event{Value: "sender"},
		SellToken:      &events.Event{Value: "sell"},
		OfferAmount:    &events.Event{Value: "offer"},
		ActualReceived: &events.Event{Value: "received"},
		BuyToken:       &events.Event{Value: "buy"},
		ReturnAmount:   &events.Event{Value: "return"},
		SpreadAmount:   &events.Event{Value: "spread"},
		ReferralFee:    &events.Event{Value: "refferral"},
	}

	trade, err := decodeSwap(r)
	if err != nil {
		t.Fatalf("decodeSwap: %v", err)
	}
	if trade.Source != SourceName {
		t.Errorf("source = %q", trade.Source)
	}
	if !trade.Pair.Base.Equal(xlm) || !trade.Pair.Quote.Equal(usdc) {
		t.Errorf("pair direction wrong: %+v", trade.Pair)
	}
	if trade.BaseAmount.Cmp(canonical.NewAmount(big.NewInt(1_000_000_000))) != 0 {
		t.Errorf("base_amount = %s", trade.BaseAmount)
	}
	if trade.QuoteAmount.Cmp(canonical.NewAmount(big.NewInt(12_420_000))) != 0 {
		t.Errorf("quote_amount = %s (want return_amount, not actual_received)", trade.QuoteAmount)
	}
	// Regression guard: QuoteAmount must be return_amount (the output),
	// never actual_received (== offer). base==quote was the Phoenix
	// pricing bug that mapped every trade to a ~1:1 price.
	if trade.BaseAmount.Cmp(trade.QuoteAmount) == 0 {
		t.Fatal("base_amount == quote_amount — QuoteAmount regressed to actual_received")
	}
	if trade.Taker != "GSENDER" {
		t.Errorf("taker = %q", trade.Taker)
	}
}

func TestDecodeSwap_incomplete(t *testing.T) {
	r := &RawSwap{Sender: &events.Event{}}
	_, err := decodeSwap(r)
	if err == nil {
		t.Fatal("expected error for incomplete swap")
	}
}

func TestDecoder_NameMatchesSourceName(t *testing.T) {
	if got := newTestDecoder().Name(); got != SourceName {
		t.Errorf("Name() = %q, want %q", got, SourceName)
	}
}

func TestBufferBackfillOldEventsComplete(t *testing.T) {
	// Regression: the planned backfill path replays ancient events;
	// without using the event's own ClosedAt as the eviction
	// reference, the first-absorbed field would evict immediately
	// when the 2nd through 8th arrived. Verify an 8-field set with
	// a 6-hour-old ClosedAt still completes in one buffer.
	buf := newBuffer()
	events := allEightSwapEventsAt(100, phoenixTxHash, 0, time.Now().UTC().Add(-6*time.Hour))

	var completed *RawSwap
	for i, e := range events {
		got, evicted, err := buf.absorb(e, e.Topic[1], mustClosed(t, e))
		if err != nil {
			t.Fatalf("step %d absorb: %v", i, err)
		}
		if len(evicted) != 0 {
			t.Errorf("step %d: evicted %d backfilled fields during correlation", i, len(evicted))
		}
		if got != nil {
			completed = got
		}
	}
	if completed == nil {
		t.Fatal("backfilled 8-field set failed to complete")
	}
}

// ─── helpers ──────────────────────────────────────────────────────

// allEightSwapEvents returns 8 synthetic events with stable
// (ledger, tx, op) = (100, phoenixTxHash, 0). Order: sender,
// sell_token, offer_amount, actual_received, buy_token,
// return_amount, spread, referral.
func allEightSwapEvents() []*events.Event {
	return allEightSwapEventsKeyed(100, phoenixTxHash, 0)
}

func allEightSwapEventsKeyed(ledger uint32, tx string, op int) []*events.Event {
	return allEightSwapEventsAt(ledger, tx, op, time.Now().UTC())
}

func allEightSwapEventsAt(ledger uint32, tx string, op int, ts time.Time) []*events.Event {
	closedAt := ts.Format(time.RFC3339)
	field := func(topic1 string) *events.Event {
		return &events.Event{
			Ledger: ledger, TxHash: tx, OperationIndex: op,
			LedgerClosedAt: closedAt,
			ContractID:     usdcContract,
			Topic:          []string{TopicSymbolSwap, topic1},
			Value:          "stub",
		}
	}
	return []*events.Event{
		field(TopicSymbolSender),
		field(TopicSymbolSellToken),
		field(TopicSymbolOfferAmount),
		field(TopicSymbolActualReceived),
		field(TopicSymbolBuyToken),
		field(TopicSymbolReturnAmount),
		field(TopicSymbolSpreadAmount),
		field(TopicSymbolReferralFee),
	}
}
