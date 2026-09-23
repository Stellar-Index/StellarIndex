package phoenix

import (
	"encoding/base64"
	"math/big"
	"testing"

	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/scval"
)

// eightFieldSwap returns the 8 String-schema field events of one swap
// under makeFieldEvent's shared groupKey.
func eightFieldSwap(t *testing.T, sell, buy string, offer, received, ret *big.Int) []events.Event {
	t.Helper()
	zero := i128Body(t, big.NewInt(0))
	fields := []struct{ topic, body string }{
		{TopicSymbolSender, addrBody(t, makeC(t, 0x10))},
		{TopicSymbolSellToken, addrBody(t, sell)},
		{TopicSymbolOfferAmount, i128Body(t, offer)},
		{TopicSymbolActualReceived, i128Body(t, received)},
		{TopicSymbolBuyToken, addrBody(t, buy)},
		{TopicSymbolReturnAmount, i128Body(t, ret)},
		{TopicSymbolSpreadAmount, zero},
		{TopicSymbolReferralFee, zero},
	}
	evs := make([]events.Event, len(fields))
	for i, f := range fields {
		evs[i] = makeFieldEvent(t, f.topic, f.body)
	}
	return evs
}

// mapSwapWith returns the real CBENABXP Map-schema swap event with its
// body fields rewritten by mutate.
func mapSwapWith(t *testing.T, mutate func(fields map[string]*xdr.ScVal)) events.Event {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(mapSwapBodyB64)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	var sv xdr.ScVal
	if err := sv.UnmarshalBinary(raw); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	entries := **sv.Map
	fields := make(map[string]*xdr.ScVal, len(entries))
	for i := range entries {
		fields[string(*entries[i].Key.Sym)] = &entries[i].Val
	}
	mutate(fields)
	ev := mapSwapEvent()
	ev.Value = b64Marshal(t, sv)
	return ev
}

func i128Val(n int64) xdr.ScVal {
	parts := xdr.Int128Parts{Hi: 0, Lo: xdr.Uint64(n)}
	return xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &parts}
}

// decodeAll feeds evs through Decode under the dispatcher's contract:
// outputs returned alongside an error are DISCARDED (dispatcher.go,
// projector.processEventSafely, ch_rebuild, ch_reproject all do this).
func decodeAll(t *testing.T, d *Decoder, evs []events.Event) (delivered []consumer.Event, errs int) {
	t.Helper()
	for _, ev := range evs {
		out, err := d.Decode(ev)
		if err != nil {
			errs++
			continue
		}
		delivered = append(delivered, out...)
	}
	return delivered, errs
}

// TestDecode_zeroAmountSwapIsRecognisedNoOp: a fully-decoded Phoenix
// swap with a ZERO leg (the dust shape aquarius pins at ledger
// 53,626,410) is not a trade, and not undecodable either. It must
// return (nil, nil) so the dispatcher does not bump decode_errors and
// the ADR-0033 re-derive counts expected=0 instead of failing the
// source's verdict closed (the INV-3 trap comet documents).
func TestDecode_zeroAmountSwapIsRecognisedNoOp(t *testing.T) {
	sell, buy := makeC(t, 0x20), makeC(t, 0x30)
	d := newTestDecoder()
	out, errs := decodeAll(t, d, eightFieldSwap(t, sell, buy,
		big.NewInt(1_000), big.NewInt(1_000), big.NewInt(0)))
	if errs != 0 {
		t.Fatalf("zero return_amount swap produced %d decode error(s), want 0 (recognised no-op)", errs)
	}
	if len(out) != 0 {
		t.Fatalf("zero return_amount swap emitted %d events, want 0", len(out))
	}

	mapEv := mapSwapWith(t, func(f map[string]*xdr.ScVal) {
		*f["return_amount"] = i128Val(0)
	})
	got, err := newTestDecoder().Decode(mapEv)
	if err != nil || len(got) != 0 {
		t.Fatalf("Map swap with zero return_amount: Decode = (%d events, %v), want (0, nil)", len(got), err)
	}
}

// TestDecode_selfPairSwapIsRecognisedNoOp: sell_token == buy_token is
// the self-swap primitive comet already treats as a determinate
// zero-row event (canonical.ErrPairMismatch) — same contract here.
func TestDecode_selfPairSwapIsRecognisedNoOp(t *testing.T) {
	tok := makeC(t, 0x20)
	d := newTestDecoder()
	out, errs := decodeAll(t, d, eightFieldSwap(t, tok, tok,
		big.NewInt(1_000), big.NewInt(1_000), big.NewInt(900)))
	if errs != 0 || len(out) != 0 {
		t.Fatalf("self-pair String swap: %d events, %d errors, want 0/0", len(out), errs)
	}

	mapEv := mapSwapWith(t, func(f map[string]*xdr.ScVal) {
		*f["buy_token"] = *f["sell_token"]
	})
	got, err := newTestDecoder().Decode(mapEv)
	if err != nil || len(got) != 0 {
		t.Fatalf("self-pair Map swap: Decode = (%d events, %v), want (0, nil)", len(got), err)
	}
}

// TestDecode_negativeAmountStaysMalformed: the no-op classification is
// scoped to ZERO; a negative leg from a genuine pool is indeterminate
// and must remain a visible decode error (aquarius draws the same line).
func TestDecode_negativeAmountStaysMalformed(t *testing.T) {
	mapEv := mapSwapWith(t, func(f map[string]*xdr.ScVal) {
		parts := xdr.Int128Parts{Hi: -1, Lo: xdr.Uint64(^uint64(0))} // -1
		*f["return_amount"] = xdr.ScVal{Type: xdr.ScValTypeScvI128, I128: &parts}
	})
	if _, err := newTestDecoder().Decode(mapEv); err == nil {
		t.Fatal("negative return_amount decoded without error, want ErrMalformedPayload")
	}
}

// TestDecode_rescuedTradeSurvivesCurrentEventError: a sweep-rescued
// pre-upgrade trade must reach the caller even when the event that
// triggered the sweep fails its own decode. Every Decode caller drops
// the outputs of an erroring call, so returning (rescued, err) lost a
// complete trade with no counter.
func TestDecode_rescuedTradeSurvivesCurrentEventError(t *testing.T) {
	d := newTestDecoder()
	sell, buy, sender := makeC(t, 0x20), makeC(t, 0x30), makeC(t, 0x10)
	zero := i128Body(t, big.NewInt(0))
	preUpgrade := []struct{ topic, body string }{
		{TopicSymbolSender, addrBody(t, sender)},
		{TopicSymbolSellToken, addrBody(t, sell)},
		{TopicSymbolOfferAmount, i128Body(t, big.NewInt(1_000_000))},
		{TopicSymbolBuyToken, addrBody(t, buy)},
		{TopicSymbolReturnAmount, i128Body(t, big.NewInt(2_000_000))},
		{TopicSymbolSpreadAmount, zero},
		{TopicSymbolReferralFee, zero},
	}
	var evs []events.Event
	for _, f := range preUpgrade {
		evs = append(evs, makeFieldEventAt(t, f.topic, f.body, "pre-upgrade-tx", "2026-04-23T12:00:00Z"))
	}
	// Past maxAge: sweeps the 7-field group, then fails its own assign.
	evs = append(evs, makeFieldEventAt(t, scval.MustEncodeString("bogus"), zero, "later-tx", "2026-04-23T12:10:00Z"))
	// An ordinary later event.
	evs = append(evs, makeFieldEventAt(t, TopicSymbolSender, addrBody(t, sender), "later-tx-2", "2026-04-23T12:10:01Z"))

	out, errs := decodeAll(t, d, evs)
	if errs != 1 {
		t.Fatalf("got %d decode errors, want exactly 1 (the unknown-field event stays visible)", errs)
	}
	if len(out) != 1 {
		t.Fatalf("delivered %d trades, want 1 (the rescued pre-upgrade swap must not be dropped with the erroring event's outputs)", len(out))
	}
	te, ok := out[0].(TradeEvent)
	if !ok {
		t.Fatalf("delivered %T, want TradeEvent", out[0])
	}
	if te.Trade.BaseAmount.BigInt().Int64() != 1_000_000 || te.Trade.QuoteAmount.BigInt().Int64() != 2_000_000 {
		t.Errorf("rescued trade = %s/%s, want 1000000/2000000", te.Trade.BaseAmount, te.Trade.QuoteAmount)
	}
	if got := d.EvictedOrphans(); got != 0 {
		t.Errorf("EvictedOrphans = %d, want 0 (the rescued group is a trade, not an orphan)", got)
	}
}
