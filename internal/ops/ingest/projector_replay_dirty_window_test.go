package ingest

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeReplayRewinder models the cursor row: RewindCursor reports whatever
// the row held at the moment of the rewind, which a projector cycle may
// have advanced past the replay's earlier read.
type fakeReplayRewinder struct {
	cursorAtRewind uint32
	windows        []timescale.ProjectionDirtyWindow
	rewindTo       uint32
	rewound        bool
	recordErr      error
}

func (f *fakeReplayRewinder) RecordProjectionDirtyWindow(_ context.Context, w timescale.ProjectionDirtyWindow) error {
	if f.recordErr != nil {
		return f.recordErr
	}
	f.windows = append(f.windows, w)
	return nil
}

func (f *fakeReplayRewinder) RewindCursor(_ context.Context, _, _ string, lastLedger uint32) (uint32, error) {
	f.rewound = true
	f.rewindTo = lastLedger
	return f.cursorAtRewind, nil
}

// widest folds the recorded windows the way RecordProjectionDirtyWindow's
// conflict arm does (LEAST from, GREATEST to).
func (f *fakeReplayRewinder) widest() timescale.ProjectionDirtyWindow {
	out := f.windows[0]
	for _, w := range f.windows[1:] {
		out.From = min(out.From, w.From)
		out.To = max(out.To, w.To)
	}
	return out
}

// A projector cycle that commits between the replay's cursor read and its
// rewind makes the re-walk rewrite ledgers above the read. The recorded
// window and the returned re-walk bound must both cover them, or
// compute-completeness carries its old clean claim over rewritten rows.
func TestRewindRecordingDirtyWindow_CoversACycleCommittedAfterTheRead(t *testing.T) {
	const target, read, advanced = 100, 500, 520
	f := &fakeReplayRewinder{cursorAtRewind: advanced}

	got, err := rewindRecordingDirtyWindow(context.Background(), io.Discard, f, "cctp", target, read)
	if err != nil {
		t.Fatalf("rewind: %v", err)
	}
	if got != advanced {
		t.Errorf("returned re-walk bound = %d, want %d (the cursor at rewind time)", got, advanced)
	}
	if f.rewindTo != target-1 {
		t.Errorf("rewound to %d, want %d", f.rewindTo, target-1)
	}
	if len(f.windows) == 0 {
		t.Fatal("no dirty window recorded")
	}
	if w := f.widest(); w.From != target || w.To != advanced {
		t.Errorf("dirty window = [%d,%d], want [%d,%d]: ledgers %d..%d are re-projected with no pending re-reconcile",
			w.From, w.To, target, advanced, read+1, advanced)
	}
}

func TestRewindRecordingDirtyWindow_NoRaceRecordsTheReadOnce(t *testing.T) {
	f := &fakeReplayRewinder{cursorAtRewind: 500}
	got, err := rewindRecordingDirtyWindow(context.Background(), io.Discard, f, "cctp", 100, 500)
	if err != nil {
		t.Fatalf("rewind: %v", err)
	}
	if got != 500 || len(f.windows) != 1 || f.windows[0].To != 500 {
		t.Errorf("got bound %d, windows %+v; want 500 and one window [100,500]", got, f.windows)
	}
}

// Record-then-rewind is the fail-closed order: no record, no rewind.
func TestRewindRecordingDirtyWindow_RefusesToRewindWithoutARecord(t *testing.T) {
	f := &fakeReplayRewinder{cursorAtRewind: 500, recordErr: errors.New("db down")}
	if _, err := rewindRecordingDirtyWindow(context.Background(), io.Discard, f, "cctp", 100, 500); err == nil {
		t.Fatal("want an error when the dirty window cannot be recorded")
	}
	if f.rewound {
		t.Error("cursor was rewound although the dirty window was never recorded")
	}
}
