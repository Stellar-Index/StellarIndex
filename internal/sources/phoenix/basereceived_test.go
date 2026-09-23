package phoenix

import (
	"encoding/base64"
	"math/big"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

func mustB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func receivedDivergence() float64 {
	return testutil.ToFloat64(obs.AMMSwapReceivedDivergenceTotal.WithLabelValues(SourceName))
}

// do_swap prices compute_swap off offer_amount before the sell-token
// transfer, so a fee-on-transfer sell token leaves the executed price at
// return/offer. The base leg stays offer_amount; the divergence is counted.
func TestDecode_baseStaysOfferWhenPoolReceivedDiverges(t *testing.T) {
	const offer, received, ret = 1_000_000, 990_000, 2_000_000
	sell, buy := makeC(t, 0x20), makeC(t, 0x30)

	before := receivedDivergence()
	out, errs := decodeAll(t, newTestDecoder(), eightFieldSwap(t, sell, buy,
		big.NewInt(offer), big.NewInt(received), big.NewInt(ret)))
	if errs != 0 || len(out) != 1 {
		t.Fatalf("String swap: %d events, %d errors, want 1/0", len(out), errs)
	}
	te := out[0].(TradeEvent)
	if got := te.Trade.BaseAmount.BigInt().Int64(); got != offer {
		t.Errorf("String swap BaseAmount = %d, want offer_amount %d (not actual received %d)", got, offer, received)
	}
	if got := te.Trade.QuoteAmount.BigInt().Int64(); got != ret {
		t.Errorf("String swap QuoteAmount = %d, want return_amount %d", got, ret)
	}
	if d := receivedDivergence() - before; d != 1 {
		t.Errorf("String swap divergence counter delta = %v, want 1", d)
	}

	before = receivedDivergence()
	mapEv := mapSwapWith(t, func(f map[string]*xdr.ScVal) {
		*f["actual_received_amount"] = i128Val(mapSwapOffer - 1_000)
	})
	got, err := newTestDecoder().Decode(mapEv)
	if err != nil || len(got) != 1 {
		t.Fatalf("Map swap: Decode = (%d events, %v), want (1, nil)", len(got), err)
	}
	if b := got[0].(TradeEvent).Trade.BaseAmount.BigInt().Int64(); b != mapSwapOffer {
		t.Errorf("Map swap BaseAmount = %d, want offer_amount %d (not actual_received_amount %d)", b, mapSwapOffer, mapSwapOffer-1_000)
	}
	if d := receivedDivergence() - before; d != 1 {
		t.Errorf("Map swap divergence counter delta = %v, want 1", d)
	}

	before = receivedDivergence()
	if got, err := newTestDecoder().Decode(mapSwapEvent()); err != nil || len(got) != 1 {
		t.Fatalf("fee-less Map swap: Decode = (%d events, %v), want (1, nil)", len(got), err)
	}
	if d := receivedDivergence() - before; d != 0 {
		t.Errorf("fee-less Map swap divergence counter delta = %v, want 0", d)
	}
}

// A zero pool-received amount still leaves a real trade: the taker paid
// offer_amount and received return_amount. It is counted, never dropped.
func TestDecode_zeroPoolReceivedIsStillATrade(t *testing.T) {
	sell, buy := makeC(t, 0x20), makeC(t, 0x30)
	before := receivedDivergence()
	out, errs := decodeAll(t, newTestDecoder(), eightFieldSwap(t, sell, buy,
		big.NewInt(1_000), big.NewInt(0), big.NewInt(900)))
	if errs != 0 || len(out) != 1 {
		t.Fatalf("zero actual-received swap: %d events, %d errors, want 1/0", len(out), errs)
	}
	if b := out[0].(TradeEvent).Trade.BaseAmount.BigInt().Int64(); b != 1_000 {
		t.Errorf("BaseAmount = %d, want offer_amount 1000", b)
	}
	if d := receivedDivergence() - before; d != 1 {
		t.Errorf("divergence counter delta = %v, want 1", d)
	}
}

// A Map body without actual_received_amount decodes on offer_amount,
// exactly as the 7-field String era does, and counts nothing.
func TestDecodeSwapMap_missingReceivedFallsBackToOffer(t *testing.T) {
	ev := mapSwapWith(t, func(map[string]*xdr.ScVal) {})
	var sv xdr.ScVal
	if err := sv.UnmarshalBinary(mustB64(t, ev.Value)); err != nil {
		t.Fatal(err)
	}
	var kept xdr.ScMap
	for _, e := range **sv.Map {
		if string(*e.Key.Sym) != "actual_received_amount" {
			kept = append(kept, e)
		}
	}
	m := &kept
	sv.Map = &m
	ev.Value = b64Marshal(t, sv)

	before := receivedDivergence()
	got, err := newTestDecoder().Decode(ev)
	if err != nil || len(got) != 1 {
		t.Fatalf("Decode = (%d events, %v), want (1, nil)", len(got), err)
	}
	if b := got[0].(TradeEvent).Trade.BaseAmount.BigInt().Int64(); b != mapSwapOffer {
		t.Errorf("BaseAmount = %d, want offer_amount %d", b, mapSwapOffer)
	}
	if d := receivedDivergence() - before; d != 0 {
		t.Errorf("divergence counter delta = %v, want 0", d)
	}
}
