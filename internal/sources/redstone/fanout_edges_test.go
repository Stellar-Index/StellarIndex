package redstone

import (
	"errors"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/events"
)

// Each checkFanoutBounds limit admits its last in-range value and
// refuses the next one; an off-by-one either way either drops a real
// full batch or lets OpIndex blocks overlap.
func TestCheckFanoutBounds_Edges(t *testing.T) {
	for name, tc := range map[string]struct {
		op, ev, prices int
		wantErr        error
		wantAnyErr     bool
	}{
		"all at last valid value": {opIndexFanoutMax - 1, eventFanoutStride - 1, opIndexFanoutStride, nil, false},
		"one price too many":      {0, 0, opIndexFanoutStride + 1, nil, true},
		"EventIndex at stride":    {0, eventFanoutStride, 1, ErrEventIndexOverflow, true},
		"negative EventIndex":     {0, -1, 1, ErrEventIndexOverflow, true},
		"OperationIndex at max":   {opIndexFanoutMax, 0, 1, ErrOperationIndexOverflow, true},
		"negative OperationIndex": {-1, 0, 1, ErrOperationIndexOverflow, true},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkFanoutBounds(&events.Event{OperationIndex: tc.op, EventIndex: tc.ev}, tc.prices)
			if !tc.wantAnyErr {
				if err != nil {
					t.Fatalf("got %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("got nil, want a refusal")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("got %v, want %v", err, tc.wantErr)
			}
		})
	}
	// The largest admitted (op, event, slot) still fits in uint32.
	if maxOp := (uint64(opIndexFanoutMax-1)*eventFanoutStride+eventFanoutStride-1)*opIndexFanoutStride + opIndexFanoutStride - 1; maxOp > 1<<32-1 {
		t.Fatalf("largest OpIndex %d overflows uint32", maxOp)
	}
}
