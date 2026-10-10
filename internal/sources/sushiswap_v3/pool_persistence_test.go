package sushiswap_v3

import (
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/contractid"
)

// A pool restored from the durable pool table must price its swaps without
// its creation event being replayed.
func TestNewDecoder_PersistedPoolRowRestoresTokenMapping(t *testing.T) {
	const warmedPool = "CDVBYETOFG7UYJAD6CMOAQZXBHEK3PD5ZDZKWMWIY5OXIWATPX4VGMY3"
	d := NewDecoder(contractid.WithAttrSeed(map[string]contractid.Attrs{
		warmedPool: {AttrToken0: tokenXLM, AttrToken1: tokenUSDC},
	}))

	ev := swapEvent(warmedPool, goldenSwapSellToken0, 64_200_014, 0, 2)
	if !d.Matches(ev) {
		t.Fatal("a persisted pool was not gated in")
	}
	out, err := d.Decode(ev)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("got %d events, want 1", len(out))
	}
}

// A persisted row with an unparseable token must leave the pool gated but
// failing closed, never mapped to an invented asset.
func TestNewDecoder_CorruptPersistedRowFailsClosed(t *testing.T) {
	const warmedPool = "CDVBYETOFG7UYJAD6CMOAQZXBHEK3PD5ZDZKWMWIY5OXIWATPX4VGMY3"
	d := NewDecoder(contractid.WithAttrSeed(map[string]contractid.Attrs{
		warmedPool: {AttrToken0: "not-a-contract", AttrToken1: tokenUSDC},
	}))
	if _, err := d.Decode(swapEvent(warmedPool, goldenSwapSellToken0, 64_200_014, 0, 2)); err == nil {
		t.Fatal("a pool with a corrupt persisted row decoded a swap")
	}
}
