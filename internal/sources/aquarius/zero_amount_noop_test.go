package aquarius

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Real r1-lake bytes: ledger 53,626,410 (closed 2024-09-23T11:34:58Z),
// tx 3870e2fc05a8a37f7ec7b01faaf222f91a353e8ca5273afe03c86b56963ba760,
// op 0 event 2 — a `trade` event from REGISTERED pool CCY2PXGM… whose
// body is (sold=2, bought=0, fee=0): a genuine dust swap whose output
// rounded to zero. First blind ledger of the 2026-08-01 full-range
// completeness reconcile — 40 of its 41 undecodable-but-matched events
// are this zero-amount class (probe over the full range, 2026-08-02:
// all 40 are bought=0 dust swaps, ledgers 53,626,410 → 57,323,650; the
// 41st is the set_privileged_addrs v2 arity, see decode_admin_test.go).
//
// The event must decode as a RECOGNIZED NO-OP (nil rows, nil error) —
// not an error, which blinds the projection reconcile; and not a
// canonical.Trade, which Validate would reject (zero side breaks price
// derivation). Same classification pattern as redstone's empty
// write_prices batch (commit 78486ae6).
const (
	zeroAmountTradeContract = "CCY2PXGMKNQHO7WNYXEWX76L2C5BH3JUW3RCATGUYKY7QQTRILBZIFWV"
	zeroAmountTradeValue    = "AAAAEAAAAAEAAAADAAAACgAAAAAAAAAAAAAAAAAAAAIAAAAKAAAAAAAAAAAAAAAAAAAAAAAAAAoAAAAAAAAAAAAAAAAAAAAA"
)

var zeroAmountTradeTopics = []string{
	"AAAADwAAAAV0cmFkZQAAAA==",
	"AAAAEgAAAAEohS9owZhIjjRvsSEu1QKQU3Ycwk9FM5LjU5ggGwgl5w==",
	"AAAAEgAAAAEltPzYWa7C+mNIQ4xImzw8EMmLbSG+T9PLMMtolT75dw==",
	"AAAAEgAAAAAAAAAA06CkWF2KJY6l5M8FwHOcDBTyiduhNO0HT1eyazNchIM=",
}

func zeroAmountTradeEvent() events.Event {
	return events.Event{
		Type:           "contract",
		Ledger:         53626410,
		LedgerClosedAt: "2024-09-23T11:34:58Z",
		ContractID:     zeroAmountTradeContract,
		TxHash:         "3870e2fc05a8a37f7ec7b01faaf222f91a353e8ca5273afe03c86b56963ba760",
		OperationIndex: 0,
		EventIndex:     2,
		Topic:          zeroAmountTradeTopics,
		Value:          zeroAmountTradeValue,
	}
}

func TestDecodeTrade_zeroAmountIsRecognizedNoOpSentinel(t *testing.T) {
	ev := zeroAmountTradeEvent()
	closedAt, err := time.Parse(time.RFC3339, ev.LedgerClosedAt)
	if err != nil {
		t.Fatal(err)
	}
	_, err = decodeTrade(&ev, closedAt)
	if !errors.Is(err, ErrZeroAmountTrade) {
		t.Fatalf("decodeTrade err = %v, want ErrZeroAmountTrade", err)
	}
	if errors.Is(err, ErrMalformedPayload) {
		t.Fatalf("zero-amount trade must NOT classify as malformed: %v", err)
	}
}

func TestDecoderDecode_zeroAmountTradeIsNoOp(t *testing.T) {
	dec := NewDecoder()
	ev := zeroAmountTradeEvent()
	if !dec.Matches(ev) {
		t.Fatal("gated decoder must match the registered pool's trade event")
	}
	outs, err := dec.Decode(ev)
	if err != nil {
		t.Fatalf("Decode = %v, want recognized no-op (nil error)", err)
	}
	if len(outs) != 0 {
		t.Fatalf("Decode emitted %d events, want 0 (zero-amount trade projects nothing)", len(outs))
	}
}

// Negative amounts remain a hard schema violation — the no-op carve-out
// is for ZERO only.
func TestDecodeTrade_negativeAmountStillMalformed(t *testing.T) {
	ev := zeroAmountTradeEvent()
	// body (sold=-1, bought=1, fee=0).
	ev.Value = encodeTradeBody(t, big.NewInt(-1), big.NewInt(1), big.NewInt(0))
	closedAt := time.Date(2024, 9, 23, 11, 34, 58, 0, time.UTC)
	_, err := decodeTrade(&ev, closedAt)
	if !errors.Is(err, ErrMalformedPayload) {
		t.Fatalf("negative amount err = %v, want ErrMalformedPayload", err)
	}
	if errors.Is(err, ErrZeroAmountTrade) {
		t.Fatalf("negative amount must not be the zero-amount no-op: %v", err)
	}
}
