package aquarius

import (
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Real lake bytes (ledger 53,626,410): a trade from a registered pool with body (sold=2, bought=0, fee=0), a dust
// swap. It must decode as a recognised no-op (nil rows, nil error): an error blinds the projection reconcile, and a
// zero side breaks price derivation.
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

// A self-pair trade (token_sold == token_bought) maps to zero rows (canonical.ErrPairMismatch), a recognised no-op as
// in comet; as an error it would count as undecodable-but-matched and fail the source's verdict closed.
func TestDecoderDecode_selfPairTradeIsNoOp(t *testing.T) {
	dec := NewDecoder()
	ev := zeroAmountTradeEvent()
	ev.Topic = append([]string(nil), zeroAmountTradeTopics...)
	ev.Topic[2] = ev.Topic[1]
	ev.Value = encodeTradeBody(t, big.NewInt(2), big.NewInt(1), big.NewInt(0))
	if !dec.Matches(ev) {
		t.Fatal("gated decoder must match the registered pool's trade event")
	}
	outs, err := dec.Decode(ev)
	if err != nil {
		t.Fatalf("Decode = %v, want recognized no-op (nil error)", err)
	}
	if len(outs) != 0 {
		t.Fatalf("Decode emitted %d events, want 0 (self-pair trade projects nothing)", len(outs))
	}
}
