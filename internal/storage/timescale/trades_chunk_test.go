package timescale

import "testing"

// One multi-row INSERT may carry at most 65,535 bind parameters; at 13 per
// row the sub-batch cap has to sit under 5,041, and every row must land in
// exactly one sub-batch.
func TestTradeInsertChunkBounds(t *testing.T) {
	if tradeInsertMaxRows*13 > 65535 {
		t.Fatalf("tradeInsertMaxRows=%d × 13 params exceeds the 65,535-parameter ceiling", tradeInsertMaxRows)
	}
	for _, n := range []int{0, 1, tradeInsertMaxRows - 1, tradeInsertMaxRows, tradeInsertMaxRows + 1, 100_000} {
		bounds := tradeInsertChunkBounds(n)
		covered, prevEnd := 0, 0
		for _, b := range bounds {
			if b[0] != prevEnd || b[1] <= b[0] || b[1]-b[0] > tradeInsertMaxRows {
				t.Fatalf("n=%d: bad chunk %v after end %d", n, b, prevEnd)
			}
			covered += b[1] - b[0]
			prevEnd = b[1]
		}
		if covered != n {
			t.Errorf("n=%d: chunks cover %d rows", n, covered)
		}
		if n == 100_000 && len(bounds) != 20 {
			t.Errorf("100,000 rows → %d chunks, want 20", len(bounds))
		}
	}
}
