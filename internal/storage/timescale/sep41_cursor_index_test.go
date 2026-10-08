package timescale

import (
	"math"
	"testing"
)

func TestCursorIndex16_ClampsInsteadOfWrapping(t *testing.T) {
	for in, want := range map[uint32]int16{0: 0, 7: 7, math.MaxInt16: math.MaxInt16, math.MaxInt16 + 1: math.MaxInt16, math.MaxUint16: math.MaxInt16, math.MaxUint32: math.MaxInt16} {
		if got := cursorIndex16(in); got != want {
			t.Errorf("cursorIndex16(%d) = %d, want %d", in, got, want)
		}
	}
}
