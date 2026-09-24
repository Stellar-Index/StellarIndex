package orchestrator

import (
	"testing"
	"time"
)

// TestFreezeKeepsTheHeldValuesObservationStamp pins the writer half of
// RLT-357. The API stamps observed_at on a VWAP-cache serve from the
// value's `:observed_at` sibling, so every publish must write it with the
// value, and a freeze — which keeps the value alive for its hold without
// rewriting it — must keep the stamp alive for exactly as long, still
// saying when the held value was observed. The key is spelled out on the
// wire because the reader is another binary.
//
// Against the unfixed aggregator no stamp exists at all, and the API
// served the held value as observed at read time for the whole hold.
func TestFreezeKeepsTheHeldValuesObservationStamp(t *testing.T) {
	f := newFreezeFixture(t)
	stampKey := f.vwapKey() + ":observed_at"

	// A healthy two-venue bucket publishes; its stamp is its bucket end.
	f.feed(t, lkgQuoteAmount, "soroswap", "phoenix")
	f.tick(t, closedBucket)
	published := f.now.Truncate(closedBucket)
	assertStamp(t, f, stampKey, published, "after the publish")

	// A manipulated single-source print freezes the pair, and the hold
	// outlives several window-lengths of buckets.
	for i := 0; i < 3; i++ {
		f.feed(t, manipQuoteAmount, "soroswap")
		f.tick(t, closedBucket)
		if !f.state().Active() {
			t.Fatalf("bucket %d: manipulated print did not hold the freeze", i+1)
		}
		if got := f.served(t); got != lkgFormatted {
			t.Fatalf("bucket %d: served %q, want the held %q", i+1, got, lkgFormatted)
		}
		assertStamp(t, f, stampKey, published, "during the hold")
		if valTTL, atTTL := f.mr.TTL(f.vwapKey()), f.mr.TTL(stampKey); atTTL != valTTL || atTTL <= freezeTestWindow {
			t.Errorf("bucket %d: stamp TTL = %v, value TTL = %v — the stamp must be kept alive "+
				"with the held value, past its %v window", i+1, atTTL, valTTL, freezeTestWindow)
		}
	}
}

func assertStamp(t *testing.T, f *freezeFixture, key string, want time.Time, when string) {
	t.Helper()
	raw, err := f.mr.Get(key)
	if err != nil {
		t.Fatalf("%s: no observed_at stamp beside the served value: %v", when, err)
	}
	got, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t.Fatalf("%s: stamp %q is not RFC 3339: %v", when, raw, err)
	}
	if !got.Equal(want) {
		t.Errorf("%s: stamp = %s, want %s (the held value's own bucket end)", when, got, want)
	}
}
