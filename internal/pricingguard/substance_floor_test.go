package pricingguard

import (
	"math/big"
	"testing"
)

// TestFailedFloor_NamesTheFloor pins the `floor` label: persistence
// floors first, then volume, split by whether the market was valued.
func TestFailedFloor_NamesTheFloor(t *testing.T) {
	pol := testPolicy()
	rat := func(n int64) *big.Rat { return new(big.Rat).SetInt64(n) }
	const span = 6 * 3600
	cases := []struct {
		name                   string
		vol                    *big.Rat
		buckets, valued, spanS int64
		want                   SubstanceFloor
	}{
		{"clears every floor", rat(1000), 20, 20, span, FloorNone},
		{"one burst: fails every floor, buckets named", rat(0), 1, 0, 0, FloorBuckets},
		{"enough buckets, too short", rat(5000), 20, 20, span - 1, FloorSpan},
		{"valued and thin", rat(999), 20, 20, span, FloorVolume},
		{"half valued is still valued", rat(999), 20, 10, span, FloorVolume},
		{"mostly unvalued", rat(999), 20, 9, span, FloorVolumeUnvalued},
		{"SEP-41/SEP-41: never valued", rat(0), 800, 0, 22 * 3600, FloorVolumeUnvalued},
	}
	for _, tc := range cases {
		if got := FailedFloor(tc.vol, tc.buckets, tc.valued, tc.spanS, pol); got != tc.want {
			t.Errorf("%s: FailedFloor = %q, want %q", tc.name, got, tc.want)
		}
		if ok := SubstanceOK(tc.vol, tc.buckets, tc.spanS, pol); ok != (tc.want == FloorNone) {
			t.Errorf("%s: SubstanceOK = %v disagrees with FailedFloor %q", tc.name, ok, tc.want)
		}
	}
}
