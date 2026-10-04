package chops

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/clickhouse"
)

func TestRunInstanceBackfill_GenesisWatermark(t *testing.T) {
	origB, origS, origR, origT := backfillInstanceChanges, setInstanceGenesisMark, readInstanceStart, resolveInstanceTop
	t.Cleanup(func() {
		backfillInstanceChanges, setInstanceGenesisMark, readInstanceStart, resolveInstanceTop = origB, origS, origR, origT
	})
	logf := func(string, ...any) {}

	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	good := clickhouse.InstanceBackfillStart{
		RawMaxLedger: 500, MVExists: true, MVUUID: "u1", MVModified: created, MVAge: time.Hour,
	}
	with := func(f func(*clickhouse.InstanceBackfillStart)) clickhouse.InstanceBackfillStart {
		st := good
		f(&st)
		return st
	}
	recreated := with(func(s *clickhouse.InstanceBackfillStart) {
		s.MVUUID, s.MVModified, s.MVAge = "u2", created.Add(2*time.Hour), time.Second
	})
	dropped := clickhouse.InstanceBackfillStart{RawMaxLedger: 500}
	tests := []struct {
		name     string
		from, to uint32
		tip      uint32 // contiguous tip resolved after the start read; 0 = 500
		start    clickhouse.InstanceBackfillStart
		end      *clickhouse.InstanceBackfillStart // state after the run; nil = start
		startErr error
		backfill error
		wantMark bool
		wantErr  bool
	}{
		{name: "complete genesis run", from: 2, start: good, wantMark: true},
		{name: "ledgers closed between the start read and the tip", from: 2, tip: 510, start: good, wantMark: true},
		{name: "partial run above genesis", from: 1000, tip: 1500, start: good},
		{name: "explicit -to", from: 2, to: 500, start: good},
		{name: "hole below the raw tip", from: 2, start: with(func(s *clickhouse.InstanceBackfillStart) { s.RawMaxLedger = 900 })},
		{name: "no materialized view", from: 2, start: clickhouse.InstanceBackfillStart{RawMaxLedger: 500}},
		{name: "view younger than the settle margin", from: 2, start: with(func(s *clickhouse.InstanceBackfillStart) { s.MVAge = time.Minute })},
		{name: "view dropped during the run", from: 2, start: good, end: &dropped},
		{name: "view recreated during the run", from: 2, start: good, end: &recreated},
		{name: "start state unreadable", from: 2, startErr: errors.New("down")},
		{name: "failed run", from: 2, start: good, backfill: errors.New("boom"), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var gotThru uint32
			marked := false
			reads := 0
			readInstanceStart = func(context.Context, string, string) (clickhouse.InstanceBackfillStart, error) {
				reads++
				if reads > 1 && tc.end != nil {
					return *tc.end, nil
				}
				return tc.start, tc.startErr
			}
			tip := tc.tip
			if tip == 0 {
				tip = 500
			}
			resolveInstanceTop = func(context.Context, string, uint32, uint32) (uint32, error) {
				if reads != 1 {
					t.Fatalf("tip resolved after %d start reads, want 1: the raw tip must be read first", reads)
				}
				return tip, nil
			}
			backfillInstanceChanges = func(context.Context, string, string, uint32, uint32, uint32, func(string, ...any)) error {
				return tc.backfill
			}
			setInstanceGenesisMark = func(_ context.Context, _, _ string, thru uint32) error {
				marked, gotThru = true, thru
				return nil
			}
			confirm := func(uint32) bool { return true }
			err := runInstanceBackfill(context.Background(), "addr", "contract_instance_changes", tc.from, tc.to, 100, confirm, logf)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if marked != tc.wantMark || (marked && gotThru != tip) {
				t.Fatalf("marked=%v thru=%d, want marked=%v thru=%d", marked, gotThru, tc.wantMark, tip)
			}
		})
	}
}
