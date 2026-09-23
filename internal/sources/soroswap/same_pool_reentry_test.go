package soroswap

import (
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// A route that re-enters ONE pair inside one op shares a groupKey
// (ledger, tx, op, pair) across both swaps. With non-contiguous emission
// (swap1, swap2, sync1, sync2) the second swap used to overwrite the
// first in place: one trade instead of two, and the lost one never
// counted as an orphan. The first swap must be rotated out and emitted
// on its own — decodeSwap reads only the swap body — and both trades
// must land on distinct op_index values.
func TestDecoder_Decode_samePoolTwiceInOp_nonContiguous(t *testing.T) {
	pool := makeContractStrkey(t, 0x20)
	t0, _ := canonical.NewSorobanAsset(makeContractStrkey(t, 0x10))
	t1, _ := canonical.NewSorobanAsset(makeContractStrkey(t, 0x11))
	d := NewDecoder(WithSeededPairTokensDecoder(map[string]PairTokens{
		pool: {Token0: t0, Token1: t1},
	}))

	at := func(ev events.Event, idx int) events.Event {
		ev.EventIndex = idx
		return ev
	}
	feed := []events.Event{
		at(makeSwapEvent(t, pool, big.NewInt(100), big.NewInt(200)), 0),
		at(makeSwapEvent(t, pool, big.NewInt(300), big.NewInt(400)), 1),
		at(makeSyncEvent(t, pool), 2),
		at(makeSyncEvent(t, pool), 3),
	}
	byBase := map[string]canonical.Trade{}
	ops := map[uint32]bool{}
	for i, ev := range feed {
		out, err := d.Decode(ev)
		if err != nil {
			t.Fatalf("Decode event %d: %v", i, err)
		}
		for _, o := range out {
			tr := o.(TradeEvent).Trade
			byBase[tr.BaseAmount.String()] = tr
			ops[tr.OpIndex] = true
		}
	}
	if len(byBase) != 2 {
		t.Fatalf("got %d distinct trades, want 2 (the second same-pair swap overwrote the first)", len(byBase))
	}
	for base, quote := range map[string]string{"100": "200", "300": "400"} {
		tr, ok := byBase[base]
		if !ok {
			t.Errorf("swap with base %s missing", base)
			continue
		}
		if tr.QuoteAmount.String() != quote {
			t.Errorf("swap base %s: quote = %s, want %s", base, tr.QuoteAmount, quote)
		}
	}
	if len(ops) != 2 {
		t.Errorf("trades share %d op_index value(s), want 2 distinct", len(ops))
	}
}

// A redelivered swap (same EventIndex) is idempotent: it must not rotate
// the group and emit the same trade twice.
func TestDecoder_Decode_redeliveredSwapIsIdempotent(t *testing.T) {
	pool := makeContractStrkey(t, 0x20)
	t0, _ := canonical.NewSorobanAsset(makeContractStrkey(t, 0x10))
	t1, _ := canonical.NewSorobanAsset(makeContractStrkey(t, 0x11))
	d := NewDecoder(WithSeededPairTokensDecoder(map[string]PairTokens{
		pool: {Token0: t0, Token1: t1},
	}))
	swap := makeSwapEvent(t, pool, big.NewInt(100), big.NewInt(200))
	sync := makeSyncEvent(t, pool)
	sync.EventIndex = 1
	var n int
	for _, ev := range []events.Event{swap, swap, sync} {
		out, err := d.Decode(ev)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		n += len(out)
	}
	if n != 1 {
		t.Fatalf("emitted %d trades, want 1", n)
	}
}
