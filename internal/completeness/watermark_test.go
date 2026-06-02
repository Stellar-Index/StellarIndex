package completeness

import (
	"math"
	"testing"
)

func TestComputeWatermark(t *testing.T) {
	const genesis, tip = 100, 200

	tests := []struct {
		name         string
		problems     []uint32
		wantLedger   uint32
		wantComplete bool
		wantCoverage float64
		wantFirst    uint32
	}{
		{
			name:         "no problems → complete",
			problems:     nil,
			wantLedger:   200,
			wantComplete: true,
			wantCoverage: 1,
			wantFirst:    0,
		},
		{
			name:         "single problem mid-range",
			problems:     []uint32{150},
			wantLedger:   149,
			wantComplete: false,
			wantCoverage: float64(149-100+1) / float64(200-100+1), // 50/101
			wantFirst:    150,
		},
		{
			name:         "earliest of several wins",
			problems:     []uint32{180, 130, 199},
			wantLedger:   129,
			wantComplete: false,
			wantCoverage: float64(129-100+1) / float64(101),
			wantFirst:    130,
		},
		{
			name:         "problems outside range ignored",
			problems:     []uint32{50, 250},
			wantLedger:   200,
			wantComplete: true,
			wantCoverage: 1,
			wantFirst:    0,
		},
		{
			name:         "problem at genesis → zero coverage",
			problems:     []uint32{100},
			wantLedger:   99,
			wantComplete: false,
			wantCoverage: 0,
			wantFirst:    100,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := ComputeWatermark(genesis, tip, tc.problems)
			if w.Ledger != tc.wantLedger {
				t.Errorf("Ledger = %d, want %d", w.Ledger, tc.wantLedger)
			}
			if w.Complete != tc.wantComplete {
				t.Errorf("Complete = %v, want %v", w.Complete, tc.wantComplete)
			}
			if math.Abs(w.CoveragePct-tc.wantCoverage) > 1e-9 {
				t.Errorf("CoveragePct = %v, want %v", w.CoveragePct, tc.wantCoverage)
			}
			if w.FirstProblem != tc.wantFirst {
				t.Errorf("FirstProblem = %d, want %d", w.FirstProblem, tc.wantFirst)
			}
		})
	}
}
