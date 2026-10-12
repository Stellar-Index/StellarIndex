package timescale

import (
	"errors"
	"testing"
)

// TestGapVerdictTrustworthy pins that a zero-gap result for a target whose density comes from a
// DistinctLedgerCountSQL override (soroban-events) is not trusted as clean without positive census evidence,
// since a TRUNCATE leaves no row to bracket a gap.
func TestGapVerdictTrustworthy(t *testing.T) {
	cases := []struct {
		name              string
		hasCensusOverride bool
		distinctErr       error
		distinct          int64
		want              bool
	}{
		{"no override: always trusted", false, nil, 0, true},
		{"no override: trusted even with distinct=0", false, nil, 0, true},
		{"census override, positive census: trusted", true, nil, 5, true},
		{"census override, zero census: NOT trusted (the finding)", true, nil, 0, false},
		{"census override, count-distinct errored: NOT trusted", true, errors.New("boom"), 5, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := gapVerdictTrustworthy(tc.hasCensusOverride, tc.distinctErr, tc.distinct); got != tc.want {
				t.Errorf("gapVerdictTrustworthy(hasCensusOverride=%v, err=%v, distinct=%d) = %v, want %v",
					tc.hasCensusOverride, tc.distinctErr, tc.distinct, got, tc.want)
			}
		})
	}
}
