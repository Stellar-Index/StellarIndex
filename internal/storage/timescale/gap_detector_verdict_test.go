package timescale

import (
	"errors"
	"testing"
)

// TestGapVerdictTrustworthy pins CODE-M #803: a zero-gap FindPerSourceLedgerGaps
// result for a target whose density comes from a DistinctLedgerCountSQL
// override (soroban-events, from ledger_ingest_log rather than the table
// itself) must NOT be trusted as "clean" unless the census also saw positive
// evidence. Without this a TRUNCATE of soroban_events — which leaves
// FindPerSourceLedgerGaps with no present row to bracket a gap with, so it
// reports zero missing ledgers — reads as healthy coverage.
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
