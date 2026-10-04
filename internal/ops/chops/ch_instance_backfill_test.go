package chops

import (
	"context"
	"errors"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func TestRunInstanceBackfill_GenesisWatermark(t *testing.T) {
	origB, origS, origR := backfillInstanceChanges, setInstanceGenesisMark, readInstanceStart
	t.Cleanup(func() { backfillInstanceChanges, setInstanceGenesisMark, readInstanceStart = origB, origS, origR })
	logf := func(string, ...any) {}

	good := clickhouse.InstanceBackfillStart{RawMaxLedger: 500, MVExists: true}
	tests := []struct {
		name     string
		from, to uint32
		start    clickhouse.InstanceBackfillStart
		startErr error
		backfill error
		wantMark bool
		wantErr  bool
	}{
		{name: "complete genesis run", from: 2, start: good, wantMark: true},
		{name: "partial run above genesis", from: 1000, start: good},
		{name: "explicit -to", from: 2, to: 500, start: good},
		{name: "hole below the raw tip", from: 2, start: clickhouse.InstanceBackfillStart{RawMaxLedger: 900, MVExists: true}},
		{name: "no materialized view", from: 2, start: clickhouse.InstanceBackfillStart{RawMaxLedger: 500}},
		{name: "start state unreadable", from: 2, startErr: errors.New("down")},
		{name: "failed run", from: 2, start: good, backfill: errors.New("boom"), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotThru uint32
			marked := false
			readInstanceStart = func(context.Context, string, string) (clickhouse.InstanceBackfillStart, error) {
				return tc.start, tc.startErr
			}
			backfillInstanceChanges = func(context.Context, string, string, uint32, uint32, uint32, func(string, ...any)) error {
				return tc.backfill
			}
			setInstanceGenesisMark = func(_ context.Context, _, _ string, thru uint32) error {
				marked, gotThru = true, thru
				return nil
			}
			err := runInstanceBackfill(context.Background(), "addr", "contract_instance_changes", tc.from, tc.to, 500, 100, logf)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if marked != tc.wantMark || (marked && gotThru != 500) {
				t.Fatalf("marked=%v thru=%d, want marked=%v thru=500", marked, gotThru, tc.wantMark)
			}
		})
	}
}
