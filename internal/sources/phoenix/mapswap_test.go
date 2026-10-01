package phoenix

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Golden test for the NEWER single-event Map-body swap schema (Q5).
//
// Real mainnet fixture captured READ-ONLY from the r1 ClickHouse lake
// (stellar.contract_events):
//
//	contract_id CBENABXP6C4C7WG6KB7JQOTDS5GIIXF3IX3PIYNZFCDZDWUHITO2HZ4S
//	ledger_seq  63307899   tx 3cb06db3…   op_index 0   event_index 3
//	close_time  2026-07-03T09:16:57Z
//	topics_xdr  ['AAAADwAAAARzd2Fw']            (ScvSymbol("swap"))
//	data_xdr    (the ScvMap below)
//
// Decoded body (via internal/scval):
//
//	sender                 = GDCRZPZYBZ24RHRO3WBPJGFDL7NDFKUQBS3ZDB6YGBJB3TGKMFYBQ3LD
//	sell_token             = CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75  (base)
//	offer_amount           = 55194571
//	buy_token              = CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA  (XLM SAC, quote)
//	return_amount          = 273730773   ← QuoteAmount (output the taker received)
//	actual_received_amount = 55194571    == offer_amount (INPUT the pool received — must NOT be the quote)
//	referral_fee_amount    = 0
//	spread_amount          = 1928293
const (
	cbenabxpPool     = "CBENABXP6C4C7WG6KB7JQOTDS5GIIXF3IX3PIYNZFCDZDWUHITO2HZ4S"
	mapSwapTopic0B64 = "AAAADwAAAARzd2Fw" // ScvSymbol("swap")
	mapSwapSender    = "GDCRZPZYBZ24RHRO3WBPJGFDL7NDFKUQBS3ZDB6YGBJB3TGKMFYBQ3LD"
	mapSwapSellToken = "CCW67TSZV3SSS2HXMBQ5JFGCKJNXKZM7UQUWUZPUTHXSTZLEO7SJMI75"
	mapSwapBuyToken  = "CAS3J7GYLGXMF6TDJBBYYSE3HQ6BBSMLNUQ34T6TZMYMW2EVH34XOWMA"
	mapSwapOffer     = 55194571
	mapSwapReturn    = 273730773
	mapSwapClosedAt  = "2026-07-03T09:16:57Z"
	mapSwapBodyB64   = "AAAAEQAAAAEAAAAIAAAADwAAABZhY3R1YWxfcmVjZWl2ZWRfYW1vdW50AAAAAAAKAAAAAAAAAAAAAAAAA0ozywAAAA8AAAAJYnV5X3Rva2VuAAAAAAAAEgAAAAEltPzYWa7C+mNIQ4xImzw8EMmLbSG+T9PLMMtolT75dwAAAA8AAAAMb2ZmZXJfYW1vdW50AAAACgAAAAAAAAAAAAAAAANKM8sAAAAPAAAAE3JlZmVycmFsX2ZlZV9hbW91bnQAAAAACgAAAAAAAAAAAAAAAAAAAAAAAAAPAAAADXJldHVybl9hbW91bnQAAAAAAAAKAAAAAAAAAAAAAAAAEFDM1QAAAA8AAAAKc2VsbF90b2tlbgAAAAAAEgAAAAGt785ZruUpaPdgYdSUwlJbdWWfpClqZfSZ7ynlZHfklgAAAA8AAAAGc2VuZGVyAAAAAAASAAAAAAAAAADFHL84DnXIni7dgvSYo1/aMqqQDLeRh9gwUh3MymFwGAAAAA8AAAANc3ByZWFkX2Ftb3VudAAAAAAAAAoAAAAAAAAAAAAAAAAAHWxl"
)

func mapSwapEvent() events.Event {
	return events.Event{
		Ledger:         63307899,
		TxHash:         "3cb06db36a19e4eabe12e8d3ed80f326932afc289407b135ed5254f398d29180",
		OperationIndex: 0,
		EventIndex:     3,
		ContractID:     cbenabxpPool,
		LedgerClosedAt: mapSwapClosedAt,
		Topic:          []string{mapSwapTopic0B64},
		Value:          mapSwapBodyB64,
	}
}

// TopicSymbolSwapMap must byte-equal the real on-wire ScvSymbol("swap")
// topic[0]; if a refactor flipped it to ScvString, classifyAny would
// stop recognising Map-schema swaps and every one would silently drop.
func TestTopicSymbolSwapMap_matchesWire(t *testing.T) {
	if TopicSymbolSwapMap != mapSwapTopic0B64 {
		t.Fatalf("TopicSymbolSwapMap = %q, want on-wire %q", TopicSymbolSwapMap, mapSwapTopic0B64)
	}
	raw, err := base64.StdEncoding.DecodeString(TopicSymbolSwapMap)
	if err != nil || len(raw) < 4 {
		t.Fatalf("decode: %v", err)
	}
	// Discriminator 15 == ScVal::Symbol (0x0F); the legacy String
	// schema is 14 (0x0E).
	if disc := uint32(raw[3]); disc != 15 {
		t.Errorf("disc = %d, want 15 (ScvSymbol)", disc)
	}
}

func TestDecodeSwapMap_realFixture(t *testing.T) {
	ev := mapSwapEvent()
	closedAt, err := ev.EventClosedAt()
	if err != nil {
		t.Fatalf("closed_at: %v", err)
	}

	trade, err := decodeSwapMap(&ev, closedAt)
	if err != nil {
		t.Fatalf("decodeSwapMap: %v", err)
	}

	if trade.Source != SourceName {
		t.Errorf("Source = %q", trade.Source)
	}
	if trade.BaseAmount.BigInt().Int64() != mapSwapOffer {
		t.Errorf("BaseAmount = %s, want offer_amount %d", trade.BaseAmount, mapSwapOffer)
	}
	// The whole point: QuoteAmount is return_amount (the output), NOT
	// actual_received_amount (== offer). Assert the value AND that it
	// isn't equal to base — the base==quote pricing bug.
	if trade.QuoteAmount.BigInt().Int64() != mapSwapReturn {
		t.Errorf("QuoteAmount = %s, want return_amount %d", trade.QuoteAmount, mapSwapReturn)
	}
	if trade.BaseAmount.Cmp(trade.QuoteAmount) == 0 {
		t.Fatal("base_amount == quote_amount — Map decode used actual_received, not return_amount")
	}
	if trade.Pair.Base.Type != canonical.AssetSoroban || trade.Pair.Base.ContractID != mapSwapSellToken {
		t.Errorf("base asset = %+v, want soroban %s", trade.Pair.Base, mapSwapSellToken)
	}
	if trade.Pair.Quote.Type != canonical.AssetSoroban || trade.Pair.Quote.ContractID != mapSwapBuyToken {
		t.Errorf("quote asset = %+v, want soroban %s", trade.Pair.Quote, mapSwapBuyToken)
	}
	if trade.Taker != mapSwapSender {
		t.Errorf("Taker = %q, want %q", trade.Taker, mapSwapSender)
	}
	// Single event per swap, but fan out by event index for router
	// multihop safety (same PK discipline as the String path).
	if trade.OpIndex != canonical.FanoutOpIndex(0, 3) {
		t.Errorf("OpIndex = %d, want fanout(0,3)=%d", trade.OpIndex, canonical.FanoutOpIndex(0, 3))
	}
	if !trade.Timestamp.Equal(closedAt) {
		t.Errorf("Timestamp = %v, want %v", trade.Timestamp, closedAt)
	}
}

// TestDecoder_MapSwap_gatedAndDecoded proves the Map-schema swap flows
// through the production Decode seam: a single event from the gated
// CBENABXP pool emits one TradeEvent immediately (no buffer), and the
// same event from an unregistered contract is NOT attributed
// (ADR-0035/0040 gating).
func TestDecoder_MapSwap_gatedAndDecoded(t *testing.T) {
	d := NewDecoder() // production gate: curated mainnet set incl. MainnetMapPools
	ev := mapSwapEvent()

	if !d.Matches(ev) {
		t.Fatal("gated Map-schema pool CBENABXP should Match")
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1 (Map swap is a single event)", len(out))
	}
	te, ok := out[0].(TradeEvent)
	if !ok {
		t.Fatalf("got %T, want TradeEvent", out[0])
	}
	if te.Trade.QuoteAmount.BigInt().Int64() != mapSwapReturn {
		t.Errorf("QuoteAmount = %s, want %d", te.Trade.QuoteAmount, mapSwapReturn)
	}

	// Same event shape from a foreign contract → not attributed.
	foreign := ev
	foreign.ContractID = "CFOREIGNFAKEPOOL0000000000000000000000000000000000000000"
	if d.Matches(foreign) {
		t.Error("foreign contract emitting the Map swap shape must NOT match (CS-026 gating)")
	}
}

// Real CBENABXP Map-schema liquidity events, same READ-ONLY lake capture
// as the swap above:
//
//	provide_liquidity  ledger 63295145  tx d4a3c6f9…  op 0  event 4
//	withdraw_liquidity ledger 63295946  tx 58225db8…  op 0  event 4
//
// provide body  = {actual_received_a, actual_received_b, sender, token_a, token_b}
// withdraw body = {auto_unstake_amount: void, auto_unstake_timestamp: void,
// return_amount_a, return_amount_b, sender, shares_amount}
const (
	mapProvideTopic0B64  = "AAAADwAAABFwcm92aWRlX2xpcXVpZGl0eQAAAA==" // ScvSymbol("provide_liquidity")
	mapWithdrawTopic0B64 = "AAAADwAAABJ3aXRoZHJhd19saXF1aWRpdHkAAA==" // ScvSymbol("withdraw_liquidity")
	mapLiquiditySender   = "GDXWIY7YU776ETADATRWVRBMXIIQQ2DAMSYE7BRVLXJE6CM55HXMRD6A"
	mapProvideBodyB64    = "AAAAEQAAAAEAAAAFAAAADwAAABFhY3R1YWxfcmVjZWl2ZWRfYQAAAAAAAAoAAAAAAAAAAAAAAAZbNPEAAAAADwAAABFhY3R1YWxfcmVjZWl2ZWRfYgAAAAAAAAoAAAAAAAAAAAAAAAFEMbBdAAAADwAAAAZzZW5kZXIAAAAAABIAAAAAAAAAAO9kY/in/+JMAwTjasQsuhEIaGBksE+GNV3STwmd6e7IAAAADwAAAAd0b2tlbl9hAAAAABIAAAABJbT82FmuwvpjSEOMSJs8PBDJi20hvk/TyzDLaJU++XcAAAAPAAAAB3Rva2VuX2IAAAAAEgAAAAGt785ZruUpaPdgYdSUwlJbdWWfpClqZfSZ7ynlZHfklg=="
	mapWithdrawBodyB64   = "AAAAEQAAAAEAAAAGAAAADwAAABNhdXRvX3Vuc3Rha2VfYW1vdW50AAAAAAEAAAAPAAAAFmF1dG9fdW5zdGFrZV90aW1lc3RhbXAAAAAAAAEAAAAPAAAAD3JldHVybl9hbW91bnRfYQAAAAAKAAAAAAAAAAAAAAAAQsShVwAAAA8AAAAPcmV0dXJuX2Ftb3VudF9iAAAAAAoAAAAAAAAAAAAAAAANTWyoAAAADwAAAAZzZW5kZXIAAAAAABIAAAAAAAAAAO9kY/in/+JMAwTjasQsuhEIaGBksE+GNV3STwmd6e7IAAAADwAAAA1zaGFyZXNfYW1vdW50AAAAAAAACgAAAAAAAAAAAAAAAB3NZQA="
)

func mapProvideEvent() events.Event {
	return events.Event{
		Ledger:         63295145,
		TxHash:         "d4a3c6f9b4ca881bb3a99fec0efb0d8e7ba4c207d4b9b84de7406755826b3b1e",
		EventIndex:     4,
		ContractID:     cbenabxpPool,
		LedgerClosedAt: "2026-07-02T12:44:22Z",
		Topic:          []string{mapProvideTopic0B64},
		Value:          mapProvideBodyB64,
	}
}

func mapWithdrawEvent() events.Event {
	return events.Event{
		Ledger:         63295946,
		TxHash:         "58225db889448f78adb5b3eb39e82d1ff4f1b5c4af88014f0e7bff2cec898511",
		EventIndex:     4,
		ContractID:     cbenabxpPool,
		LedgerClosedAt: "2026-07-02T14:02:37Z",
		Topic:          []string{mapWithdrawTopic0B64},
		Value:          mapWithdrawBodyB64,
	}
}

func TestTopicSymbolLiquidityMap_matchesWire(t *testing.T) {
	if TopicSymbolProvideLiquidityMap != mapProvideTopic0B64 {
		t.Errorf("TopicSymbolProvideLiquidityMap = %q, want on-wire %q", TopicSymbolProvideLiquidityMap, mapProvideTopic0B64)
	}
	if TopicSymbolWithdrawLiquidityMap != mapWithdrawTopic0B64 {
		t.Errorf("TopicSymbolWithdrawLiquidityMap = %q, want on-wire %q", TopicSymbolWithdrawLiquidityMap, mapWithdrawTopic0B64)
	}
}

// decodeLiquidityThroughDecoder runs ev through the production
// Matches/Decode seam and returns the single LiquidityChange it emits.
func decodeLiquidityThroughDecoder(t *testing.T, ev events.Event) LiquidityChange {
	t.Helper()
	d := NewDecoder()
	if !d.Matches(ev) {
		t.Fatal("gated Map-schema pool CBENABXP should Match")
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1 (Map liquidity is a single event)", len(out))
	}
	le, ok := out[0].(LiquidityEvent)
	if !ok {
		t.Fatalf("got %T, want LiquidityEvent", out[0])
	}
	return le.Change
}

func TestDecoder_MapProvideLiquidity_realFixture(t *testing.T) {
	c := decodeLiquidityThroughDecoder(t, mapProvideEvent())
	if c.Action != EventActionProvideLiquidity || c.Pool != cbenabxpPool {
		t.Errorf("action/pool = %q/%q", c.Action, c.Pool)
	}
	if c.Ledger != 63295145 || c.OpIndex != 0 || c.EventIndex != 4 {
		t.Errorf("ledger/op/event = %d/%d/%d", c.Ledger, c.OpIndex, c.EventIndex)
	}
	if c.Sender != mapLiquiditySender {
		t.Errorf("Sender = %q, want %q", c.Sender, mapLiquiditySender)
	}
	if c.TokenA != mapSwapBuyToken || c.TokenB != mapSwapSellToken {
		t.Errorf("tokens = %s / %s, want %s / %s", c.TokenA, c.TokenB, mapSwapBuyToken, mapSwapSellToken)
	}
	if c.AmountA.String() != "27300000000" || c.AmountB.String() != "5439074397" {
		t.Errorf("amounts = %s / %s, want 27300000000 / 5439074397", c.AmountA, c.AmountB)
	}
	if !c.SharesAmount.IsZero() {
		t.Errorf("SharesAmount = %s, want zero on provide", c.SharesAmount)
	}
}

func TestDecoder_MapWithdrawLiquidity_realFixture(t *testing.T) {
	c := decodeLiquidityThroughDecoder(t, mapWithdrawEvent())
	if c.Action != EventActionWithdrawLiquidity || c.Pool != cbenabxpPool {
		t.Errorf("action/pool = %q/%q", c.Action, c.Pool)
	}
	if c.Ledger != 63295946 || c.OpIndex != 0 || c.EventIndex != 4 {
		t.Errorf("ledger/op/event = %d/%d/%d", c.Ledger, c.OpIndex, c.EventIndex)
	}
	if c.Sender != mapLiquiditySender {
		t.Errorf("Sender = %q, want %q", c.Sender, mapLiquiditySender)
	}
	if c.AmountA.String() != "1120182615" || c.AmountB.String() != "223177896" {
		t.Errorf("amounts = %s / %s, want 1120182615 / 223177896", c.AmountA, c.AmountB)
	}
	if c.SharesAmount.String() != "500000000" {
		t.Errorf("SharesAmount = %s, want 500000000", c.SharesAmount)
	}
	if c.TokenA != "" || c.TokenB != "" {
		t.Errorf("tokens = %q / %q, want empty (withdraw carries no token addresses)", c.TokenA, c.TokenB)
	}
}

// Only the three Map-schema action symbols classify as single-topic
// events, and a Map liquidity body missing a required key is rejected.
func TestClassifyAny_MapSchemaFailsClosed(t *testing.T) {
	other := events.Event{Topic: []string{"AAAADwAAAARib25k"}} // ScvSymbol("bond")
	if a, _ := classifyAny(&other); a != actionUnknown {
		t.Errorf("1-topic Symbol(bond) classified %v, want actionUnknown", a)
	}
	legacy := events.Event{Topic: []string{TopicSymbolProvideLiquidity}}
	if a, _ := classifyAny(&legacy); a != actionUnknown {
		t.Errorf("1-topic String(provide_liquidity) classified %v, want actionUnknown", a)
	}

	ev := mapWithdrawEvent()
	ev.Value = mapProvideBodyB64 // no shares_amount / return_amount_*
	if _, err := decodeWithdrawLiquidityMap(&ev, time.Unix(0, 0)); err == nil {
		t.Error("decodeWithdrawLiquidityMap accepted a body without shares_amount")
	}
}
