package ingest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// fakeProjectorCursor answers GetCursor from a scripted sequence, the
// way the real cursor advances while the projector re-walks a replayed
// range.
type fakeProjectorCursor struct {
	ledgers []uint32
	calls   int
	err     error
}

func (f *fakeProjectorCursor) GetCursor(_ context.Context, _, _ string) (timescale.Cursor, error) {
	if f.err != nil {
		return timescale.Cursor{}, f.err
	}
	i := f.calls
	if i >= len(f.ledgers) {
		i = len(f.ledgers) - 1
	}
	f.calls++
	return timescale.Cursor{LastLedger: f.ledgers[i]}, nil
}

// TestAwaitProjectorCursor_ReturnsOnlyOnceTheRangeIsReWalked: the
// post-replay CAGG refresh must not run against rows the projector has
// not written yet. A refresh over an un-re-projected range SUCCEEDS and
// materializes the short answer — worse than no refresh, because it
// leaves a green run to point at.
func TestAwaitProjectorCursor_ReturnsOnlyOnceTheRangeIsReWalked(t *testing.T) {
	f := &fakeProjectorCursor{ledgers: []uint32{60_000_000, 62_000_000, 63_500_000}}
	if err := awaitProjectorCursor(context.Background(), discardLogger(), f, "cctp", 63_500_000, 30*time.Second, time.Millisecond); err != nil {
		t.Fatalf("awaitProjectorCursor: %v", err)
	}
	if f.calls < 3 {
		t.Errorf("returned after %d cursor reads — it cannot have observed the cursor reach the target", f.calls)
	}
}

// A projector that never catches up must FAIL the command, naming the
// range left unmaterialized. The rewind is durable by then, so a silent
// return would leave every OHLC/VWAP read short over the replayed range
// with nothing saying so.
func TestAwaitProjectorCursor_TimeoutFailsLoudly(t *testing.T) {
	f := &fakeProjectorCursor{ledgers: []uint32{60_000_000}}
	err := awaitProjectorCursor(context.Background(), discardLogger(), f, "cctp", 63_500_000, 10*time.Millisecond, time.Millisecond)
	if err == nil {
		t.Fatal("a projector that never re-walked the range returned success")
	}
	for _, want := range []string{"63500000", "not fully re-projected", "were NOT refreshed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("timeout error lacks %q: %v", want, err)
		}
	}
}

func TestAwaitProjectorCursor_CursorReadErrorPropagates(t *testing.T) {
	f := &fakeProjectorCursor{err: errors.New("boom")}
	if err := awaitProjectorCursor(context.Background(), discardLogger(), f, "cctp", 1, time.Second, time.Millisecond); err == nil {
		t.Fatal("a failing cursor read returned success")
	}
}
