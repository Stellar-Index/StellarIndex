package chops

import (
	"context"
	"errors"
	"testing"
)

func TestRunInstanceBackfill_GenesisWatermark(t *testing.T) {
	origB, origS := backfillInstanceChanges, setInstanceGenesisMark
	t.Cleanup(func() { backfillInstanceChanges, setInstanceGenesisMark = origB, origS })
	logf := func(string, ...any) {}

	tests := []struct {
		name     string
		from     uint32
		backfill error
		wantMark bool
		wantErr  bool
		wantThru uint32
	}{
		{"complete genesis run", 2, nil, true, false, 500},
		{"partial run above genesis", 1000, nil, false, false, 0},
		{"failed run", 2, errors.New("boom"), false, true, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotThru uint32
			marked := false
			backfillInstanceChanges = func(context.Context, string, string, uint32, uint32, uint32, func(string, ...any)) error {
				return tc.backfill
			}
			setInstanceGenesisMark = func(_ context.Context, _, _ string, thru uint32) error {
				marked, gotThru = true, thru
				return nil
			}
			err := runInstanceBackfill(context.Background(), "addr", "contract_instance_changes", tc.from, 500, 100, logf)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if marked != tc.wantMark || gotThru != tc.wantThru {
				t.Fatalf("marked=%v thru=%d, want marked=%v thru=%d", marked, gotThru, tc.wantMark, tc.wantThru)
			}
		})
	}
}
