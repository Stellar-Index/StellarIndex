package band

import (
	"testing"
	"time"
)

// Exactly opIndexFanoutStride symbol_rates fit one call's OpIndex block;
// one more would spill into the next operation's block and is refused.
func TestDecodeRelay_FanoutStrideEdges(t *testing.T) {
	closedAt := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	decode := func(n int) (int, error) {
		pairs := make([]struct {
			Symbol string
			Rate   uint64
		}, n)
		for i := range pairs {
			pairs[i].Symbol, pairs[i].Rate = "BTC", uint64(i+1)
		}
		args := []string{
			encodeAddressArg(t, relayerG),
			encodeSymbolRatesArg(t, pairs),
			encodeU64Arg(t, uint64(closedAt.Unix())),
			encodeU64Arg(t, 1),
		}
		updates, err := decodeRelayArgs(FnRelay, args, adapterC, 52_000_000, "abcd", 1, "", "", closedAt)
		if err == nil {
			if last := updates[len(updates)-1].OpIndex; last != 2*opIndexFanoutStride-1 {
				t.Errorf("last OpIndex = %d, want %d", last, 2*opIndexFanoutStride-1)
			}
		}
		return len(updates), err
	}
	if n, err := decode(opIndexFanoutStride); err != nil || n != opIndexFanoutStride {
		t.Fatalf("%d pairs: got (%d, %v), want all decoded", opIndexFanoutStride, n, err)
	}
	if _, err := decode(opIndexFanoutStride + 1); err == nil {
		t.Fatalf("%d pairs decoded; must be refused", opIndexFanoutStride+1)
	}
}
