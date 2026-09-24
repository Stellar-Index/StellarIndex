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

// fakeReplayStore is a replayFinisher: a scripted projector cursor plus a
// recording CAGG refresher.
type fakeReplayStore struct {
	fakeProjectorCursor
	rangeFrom, rangeTo uint32
	refreshed          []string
}

func (f *fakeReplayStore) LedgerRangeToTimeRange(_ context.Context, from, to uint32) (time.Time, time.Time, error) {
	f.rangeFrom, f.rangeTo = from, to
	t0 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	return t0, t0.Add(6 * time.Hour), nil
}

func (f *fakeReplayStore) LedgerRangeToOracleTimeRange(context.Context, uint32, uint32) (time.Time, time.Time, error) {
	return time.Time{}, time.Time{}, timescale.ErrNotFound
}

func (f *fakeReplayStore) Prices1mRetentionArmed(context.Context) (bool, error) { return false, nil }

func (f *fakeReplayStore) RefreshContinuousAggregateForced(ctx context.Context, name string, from, to time.Time) error {
	return f.RefreshContinuousAggregate(ctx, name, from, to)
}

func (f *fakeReplayStore) RefreshContinuousAggregate(_ context.Context, name string, _, _ time.Time) error {
	f.refreshed = append(f.refreshed, name)
	return nil
}

// TestRematerializeReplayedRange_RefreshesEveryViewOverTheReplayedRange:
// once the projector is back at the pre-rewind ledger, the tail refreshes
// every long-lived price CAGG over exactly the replayed ledger range.
func TestRematerializeReplayedRange_RefreshesEveryViewOverTheReplayedRange(t *testing.T) {
	f := &fakeReplayStore{fakeProjectorCursor: fakeProjectorCursor{ledgers: []uint32{63_500_000}}}
	replayed := chunkRange{from: 62_000_000, to: 63_500_000}
	err := rematerializeReplayedRange(discardLogger(), f, "cctp", replayed,
		replayFollowUp{refreshCAGGs: true, wait: true, waitTimeout: time.Minute})
	if err != nil {
		t.Fatalf("rematerializeReplayedRange: %v", err)
	}
	if f.rangeFrom != replayed.from || f.rangeTo != replayed.to {
		t.Errorf("refreshed ledgers [%d,%d], want the replayed range [%d,%d]",
			f.rangeFrom, f.rangeTo, replayed.from, replayed.to)
	}
	if got, want := len(f.refreshed), len(timescale.TradesCAGGs); got != want {
		t.Errorf("refreshed %d views %v, want all %d trades CAGGs", got, f.refreshed, want)
	}
}

// TestRematerializeReplayedRange_NeverRefreshesAheadOfTheProjector: a
// projector still short of the pre-rewind ledger when the budget runs out
// is an ERROR, and no view is refreshed — a refresh over rows not yet
// re-projected succeeds and materializes the short answer.
func TestRematerializeReplayedRange_NeverRefreshesAheadOfTheProjector(t *testing.T) {
	f := &fakeReplayStore{fakeProjectorCursor: fakeProjectorCursor{ledgers: []uint32{62_400_000}}}
	err := rematerializeReplayedRange(discardLogger(), f, "cctp",
		chunkRange{from: 62_000_000, to: 63_500_000},
		replayFollowUp{refreshCAGGs: true, wait: true, waitTimeout: 0})
	if err == nil {
		t.Fatal("want an error while the projector is short of the pre-rewind ledger, got nil")
	}
	if !strings.Contains(err.Error(), "NOT refreshed") {
		t.Errorf("error does not say the CAGGs were left unrefreshed: %v", err)
	}
	if len(f.refreshed) != 0 {
		t.Errorf("refreshed %v ahead of the projector — that materializes the short answer", f.refreshed)
	}
}

// TestRematerializeReplayedRange_OptOutsTouchNothing: -refresh-caggs=false
// and -wait=false both return nil without reading the cursor or
// refreshing a view; the rewind they follow is already durable.
func TestRematerializeReplayedRange_OptOutsTouchNothing(t *testing.T) {
	for name, opts := range map[string]replayFollowUp{
		"-refresh-caggs=false": {refreshCAGGs: false, wait: true, waitTimeout: time.Minute},
		"-wait=false":          {refreshCAGGs: true, wait: false, waitTimeout: time.Minute},
	} {
		f := &fakeReplayStore{fakeProjectorCursor: fakeProjectorCursor{ledgers: []uint32{1}}}
		if err := rematerializeReplayedRange(discardLogger(), f, "cctp", chunkRange{from: 10, to: 20}, opts); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if f.calls != 0 || len(f.refreshed) != 0 {
			t.Errorf("%s: cursor reads=%d refreshed=%v, want neither", name, f.calls, f.refreshed)
		}
	}
}
