package soroswap

import (
	"math/big"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// At the end of a bounded stream a swap with no following sync is final (its
// trade reads from the swap body alone); a bare sync is LP-traffic noise.
func TestDecoder_Drain_emitsSwapOnlyAndCountsBareSync(t *testing.T) {
	poolA := makeContractStrkey(t, 0x20)
	poolB := makeContractStrkey(t, 0x21)
	t0, _ := canonical.NewSorobanAsset(makeContractStrkey(t, 0x10))
	t1, _ := canonical.NewSorobanAsset(makeContractStrkey(t, 0x11))
	d := NewDecoder(WithSeededPairTokensDecoder(map[string]PairTokens{
		poolA: {Token0: t0, Token1: t1},
		poolB: {Token0: t0, Token1: t1},
	}))

	if out, err := d.Decode(makeSwapEvent(t, poolA, big.NewInt(100), big.NewInt(200))); err != nil || len(out) != 0 {
		t.Fatalf("swap: out=%d err=%v, want buffered", len(out), err)
	}
	if out, err := d.Decode(makeSyncEvent(t, poolB)); err != nil || len(out) != 0 {
		t.Fatalf("sync: out=%d err=%v, want buffered", len(out), err)
	}

	out := d.Drain()
	if len(out) != 1 {
		t.Fatalf("Drain emitted %d events, want 1 (the swap-only trade)", len(out))
	}
	if got := out[0].(TradeEvent).Trade.BaseAmount.String(); got != "100" {
		t.Errorf("drained trade base = %s, want 100", got)
	}
	if got := d.EvictedBareSync(); got != 1 {
		t.Errorf("EvictedBareSync = %d, want 1", got)
	}
	if d.buf.size() != 0 || len(d.Drain()) != 0 {
		t.Errorf("buffer not empty after Drain: size=%d", d.buf.size())
	}
}
