package chops

import (
	"context"
	"strings"
	"testing"
)

// An explicit -from above the resume point would derive [from, to] and stamp the watermark over [resume, from-1], which the
// movements API then serves from the (empty) archive arm. The first run is the case the storage advance cannot catch (wm 0).
func TestResolveDeriveRange_RefusesFromAboveTheResumePoint(t *testing.T) {
	orig := deriveLakeMinLedger
	deriveLakeMinLedger = func(context.Context, string) (uint32, error) { return 2, nil }
	t.Cleanup(func() { deriveLakeMinLedger = orig })
	const floor = uint32(58_762_517)
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		wm, from uint32
		refuse   bool
	}{
		{"first run above the floor", 0, 60_000_000, true},
		{"resume skipping a prefix", 59_000_000, 60_000_000, true},
		{"first run at the floor", 0, floor, false},
		{"re-derive below the watermark", 59_000_000, 58_900_000, false},
		{"resume at watermark+1", 59_000_000, 59_000_001, false},
	} {
		readWM := func(context.Context, string) (uint32, error) { return tc.wm, nil }
		// An unreachable lake: a run past the refusal fails on the contiguity read instead.
		_, _, err := resolveDeriveRange(ctx, "127.0.0.1:1", "ch-cap67-movements", tc.from, 0, floor, readWM)
		if got := err != nil && strings.Contains(err.Error(), "omit -from"); got != tc.refuse {
			t.Errorf("%s (wm %d, -from %d): refused=%v (err %v), want refused=%v", tc.name, tc.wm, tc.from, got, err, tc.refuse)
		}
	}
}
