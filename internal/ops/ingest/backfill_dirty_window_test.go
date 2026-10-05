package ingest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

type fakeDirtyWindowRecorder struct {
	err     error
	windows []timescale.ProjectionDirtyWindow
}

func (f *fakeDirtyWindowRecorder) RecordProjectionDirtyWindow(_ context.Context, w timescale.ProjectionDirtyWindow) error {
	if f.err != nil {
		return f.err
	}
	f.windows = append(f.windows, w)
	return nil
}

// memoisedGate returns a run gate already decided as verdict, so
// buildChunkDispatcher stops at the gate without touching ClickHouse or the
// store: everything before the gate has run, nothing that writes has.
func memoisedGate(verdict error) *replayGateOnce {
	g := &replayGateOnce{}
	_ = g.check(func() error { return verdict })
	return g
}

// TestBackfillRecordsDirtyWindowBeforeFirstWrite pins INV-2600: a backfill
// that rewrites served rows for a range records a dirty window covering that
// range before its dispatcher (the only path to a write) exists, and never
// gets that far if recording fails.
func TestBackfillRecordsDirtyWindowBeforeFirstWrite(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	gateReached := errors.New("reached the wasm gate")
	sources := []string{"sdex", SorobanEventsPseudoSource}

	t.Run("records the audited source over the range", func(t *testing.T) {
		rec := &fakeDirtyWindowRecorder{}
		opts := backfillOpts{from: 60_000_000, to: 60_100_000, sources: sources, wasmGate: memoisedGate(gateReached)}
		_, _, err := buildChunkDispatcher(context.Background(), logger, opts, config.Config{}, nil, rec, false)
		if !errors.Is(err, gateReached) {
			t.Fatalf("err = %v, want the memoised gate verdict", err)
		}
		want := timescale.ProjectionDirtyWindow{
			Source: "sdex", From: 60_000_000, To: 60_100_000,
			Reason: timescale.BackfillWriteReason(60_000_000, 60_100_000),
		}
		if len(rec.windows) != 1 || rec.windows[0] != want {
			t.Fatalf("recorded %+v, want exactly [%+v]", rec.windows, want)
		}
	})

	t.Run("a recording failure stops before the dispatcher", func(t *testing.T) {
		recErr := errors.New("postgres down")
		rec := &fakeDirtyWindowRecorder{err: recErr}
		opts := backfillOpts{from: 60_000_000, to: 60_100_000, sources: sources, wasmGate: memoisedGate(gateReached)}
		disp, _, err := buildChunkDispatcher(context.Background(), logger, opts, config.Config{}, nil, rec, false)
		if !errors.Is(err, recErr) || errors.Is(err, gateReached) || disp != nil {
			t.Fatalf("err = %v, disp = %v; want the recording error and no dispatcher", err, disp)
		}
	})
}

func TestRecordBackfillDirtyWindows(t *testing.T) {
	var bandCfg config.Config
	bandCfg.Oracle.Band.StandardReferenceContract = "CBAND"

	cases := []struct {
		name    string
		cfg     config.Config
		sources []string
		dryRun  bool
		want    []string
	}{
		{"dry run records nothing", config.Config{}, []string{"sdex"}, true, nil},
		{"non-catalogue source records nothing", config.Config{}, []string{SorobanEventsPseudoSource}, false, nil},
		{"band unconfigured is not audited", config.Config{}, []string{"band"}, false, nil},
		{"band configured is audited", bandCfg, []string{"band", "sdex"}, false, []string{"band", "sdex"}},
		{"backfill-router -write", config.Config{}, []string{"soroswap-router"}, false, []string{"soroswap-router"}},
		{"backfill-router dry run", config.Config{}, []string{"soroswap-router"}, true, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &fakeDirtyWindowRecorder{}
			opts := backfillOpts{from: 100, to: 200, sources: tc.sources, dryRun: tc.dryRun}
			if err := recordBackfillDirtyWindows(context.Background(), rec, tc.cfg, opts); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, w := range rec.windows {
				if w.From != 100 || w.To != 200 || w.Reason != timescale.BackfillWriteReason(100, 200) {
					t.Errorf("window %+v, want [100,200] with the backfill reason", w)
				}
				got = append(got, w.Source)
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("recorded %v, want %v", got, tc.want)
			}
		})
	}
}

type fakeRouterStartStore struct {
	fakeDirtyWindowRecorder
	cursor    timescale.Cursor
	cursorErr error
}

func (f *fakeRouterStartStore) GetCursor(context.Context, string, string) (timescale.Cursor, error) {
	return f.cursor, f.cursorErr
}

// TestRouterWriteStart_RecordsBeforeWalk: a backfill-router -write run records
// its window before it hands back the start ledger the walk needs, a dry run or an already-finished range records nothing, and a
// recording failure stops the run.
func TestRouterWriteStart_RecordsBeforeWalk(t *testing.T) {
	ctx := context.Background()
	want := timescale.ProjectionDirtyWindow{
		Source: "soroswap-router", From: 100, To: 200,
		Reason: timescale.BackfillWriteReason(100, 200),
	}
	cases := []struct {
		name      string
		store     *fakeRouterStartStore
		write     bool
		wantStart uint32
		wantDone  bool
		wantWins  int
	}{
		{"fresh write", &fakeRouterStartStore{cursorErr: timescale.ErrNotFound}, true, 100, false, 1},
		{"resumed write keeps the full window", &fakeRouterStartStore{cursor: timescale.Cursor{LastLedger: 150}}, true, 151, false, 1},
		{"dry run", &fakeRouterStartStore{cursorErr: timescale.ErrNotFound}, false, 100, false, 0},
		{"range already walked", &fakeRouterStartStore{cursor: timescale.Cursor{LastLedger: 200}}, true, 201, true, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start, done, err := routerWriteStart(ctx, tc.store, config.Config{}, 100, 200, true, tc.write)
			if err != nil || start != tc.wantStart || done != tc.wantDone {
				t.Fatalf("= (%d, %v, %v), want (%d, %v, nil)", start, done, err, tc.wantStart, tc.wantDone)
			}
			if len(tc.store.windows) != tc.wantWins || (tc.wantWins == 1 && tc.store.windows[0] != want) {
				t.Fatalf("recorded %+v, want %d window(s) of %+v", tc.store.windows, tc.wantWins, want)
			}
		})
	}

	t.Run("a recording failure stops the run", func(t *testing.T) {
		recErr := errors.New("postgres down")
		store := &fakeRouterStartStore{fakeDirtyWindowRecorder: fakeDirtyWindowRecorder{err: recErr}, cursorErr: timescale.ErrNotFound}
		if _, _, err := routerWriteStart(ctx, store, config.Config{}, 100, 200, true, true); !errors.Is(err, recErr) {
			t.Fatalf("err = %v, want the recording error", err)
		}
	})
}
