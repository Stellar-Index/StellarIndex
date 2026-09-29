package diagnostics

import (
	"math"
	"testing"
)

func TestEventsProbeStart(t *testing.T) {
	for _, tc := range []struct {
		seq, want uint32
		ok        bool
	}{
		{0, 0, false},
		{1, 0, true},
		{2, 1, true},
		{math.MaxUint32, math.MaxUint32 - 1, true},
	} {
		got, ok := eventsProbeStart(tc.seq)
		if got != tc.want || ok != tc.ok {
			t.Errorf("eventsProbeStart(%d) = (%d, %v), want (%d, %v)", tc.seq, got, ok, tc.want, tc.ok)
		}
	}
}
