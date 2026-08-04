package statsflush

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// stubStatsSource is a deterministic StatsSource for tests. The
// Flusher is exercised via direct flushAt calls; the goroutine
// loop in Run() is straightforward enough that the table-driven
// flushAt tests cover the contract.
type stubStatsSource struct {
	stats dispatcher.Stats
}

func (s *stubStatsSource) Stats() dispatcher.Stats { return s.stats }

// fakeStatsWriter is a [statsWriter] fake that fails on demand.
// *timescale.Store can't play this role in a unit test: Store.Open
// pings the DB before returning, so there's no way to construct one
// that succeeds and then fails a later call without a real reachable
// Postgres.
type fakeStatsWriter struct {
	fail  bool
	calls [][]timescale.DecoderStatsBucket
	// ctxErrs records ctx.Err() as observed at write time, one entry
	// per call. A non-nil entry means the real *timescale.Store would
	// have been rejected by database/sql before reaching Postgres —
	// which is how the shutdown drain silently wrote nothing.
	ctxErrs []error
}

func (w *fakeStatsWriter) InsertDecoderStats(ctx context.Context, rows []timescale.DecoderStatsBucket) error {
	cp := append([]timescale.DecoderStatsBucket(nil), rows...)
	w.calls = append(w.calls, cp)
	w.ctxErrs = append(w.ctxErrs, ctx.Err())
	if w.fail {
		return errors.New("simulated postgres outage")
	}
	return nil
}

func newTestFlusher(source StatsSource, store statsWriter) *Flusher {
	return New(source, store, slog.New(slog.NewTextHandler(io.Discard, nil)), Options{Interval: 5 * time.Minute})
}

// TestFlushAt_WriteFailure_RetainsLastSnapshot is the regression test
// for INT-05 (audit-2026-07-23): flushAt used to advance f.last
// unconditionally, even when InsertDecoderStats failed — permanently
// discarding that window's counter deltas the moment the next tick
// computed its own delta against the now-advanced (but never durably
// written) snapshot.
//
// Drives two ticks where the write fails, then a third where it
// succeeds, and asserts the THIRD (successful) write's row carries
// the FULL cumulative delta across all three windows — proof that
// nothing from the two failed windows was silently dropped.
func TestFlushAt_WriteFailure_RetainsLastSnapshot(t *testing.T) {
	src := &stubStatsSource{stats: dispatcher.Stats{
		EventsSeen: map[string]int{"band": 10},
	}}
	w := &fakeStatsWriter{fail: true}
	f := newTestFlusher(src, w)

	base := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)

	// Tick 1: EventsSeen band=10 (delta vs zero-value last = 10). Write fails.
	f.flushAt(context.Background(), base)
	if got := f.last.EventsSeen["band"]; got != 0 {
		t.Fatalf("after failed flush 1, f.last.EventsSeen[band] = %d, want 0 (must not advance on write failure)", got)
	}

	// Tick 2: EventsSeen band=25 (cumulative). Write fails again.
	src.stats = dispatcher.Stats{EventsSeen: map[string]int{"band": 25}}
	f.flushAt(context.Background(), base.Add(5*time.Minute))
	if got := f.last.EventsSeen["band"]; got != 0 {
		t.Fatalf("after failed flush 2, f.last.EventsSeen[band] = %d, want 0 (must still not advance)", got)
	}

	// Tick 3: EventsSeen band=40 (cumulative). Write succeeds.
	w.fail = false
	src.stats = dispatcher.Stats{EventsSeen: map[string]int{"band": 40}}
	f.flushAt(context.Background(), base.Add(10*time.Minute))

	if got := f.last.EventsSeen["band"]; got != 40 {
		t.Errorf("after successful flush 3, f.last.EventsSeen[band] = %d, want 40 (snapshot must advance on success)", got)
	}

	// InsertDecoderStats is attempted on every tick regardless of
	// outcome (the fake records all 3 attempts), but only the THIRD
	// call's rows durably landed (fail=false at that point). Its delta
	// must be the FULL 40 (from the zero-value baseline before flush
	// 1) — not just 15 (band=40 minus the never-durable band=25
	// snapshot flush 2 would have left behind under the bug): flush 1
	// fails but "advances" to 10, flush 2 fails but "advances" to 25 as
	// its own last, flush 3 succeeds and computes its delta against 25
	// → reports 15, silently losing the first 25 events forever.
	if len(w.calls) != 3 {
		t.Fatalf("InsertDecoderStats called %d times, want 3 (attempted on every tick)", len(w.calls))
	}
	lastCall := w.calls[2]
	found := false
	for _, row := range lastCall {
		if row.Source != "band" {
			continue
		}
		found = true
		if row.EventsSeen != 40 {
			t.Errorf("landed row EventsSeen = %d, want 40 (the full cumulative delta across both failed windows)", row.EventsSeen)
		}
	}
	if !found {
		t.Fatalf("no band row in the successful (3rd) InsertDecoderStats call: %+v", lastCall)
	}
}

// TestFlushAt_WriteSuccess_AdvancesSnapshot pins the baseline (already
// correct) behaviour for contrast with the failure-path regression
// test above: a successful write DOES advance f.last so the next
// tick's delta starts fresh.
func TestFlushAt_WriteSuccess_AdvancesSnapshot(t *testing.T) {
	src := &stubStatsSource{stats: dispatcher.Stats{
		EventsSeen: map[string]int{"redstone": 7},
	}}
	w := &fakeStatsWriter{}
	f := newTestFlusher(src, w)

	f.flushAt(context.Background(), time.Now())

	if got := f.last.EventsSeen["redstone"]; got != 7 {
		t.Errorf("f.last.EventsSeen[redstone] = %d, want 7", got)
	}
	if len(w.calls) != 1 {
		t.Fatalf("InsertDecoderStats called %d times, want 1", len(w.calls))
	}
}

// TestRun_ShutdownDrain_UsesLiveContext is the regression test for the
// cold audit of 2026-08-04: Run's ctx.Done() arm called f.flush(ctx)
// with the very context that had just fired. database/sql checks
// ctx.Err() before acquiring a connection, so the "one last flush
// before exiting so a clean shutdown captures the final partial
// bucket" could never write anything — every indexer restart dropped
// up to a full interval of events_seen / decode_errors / orphan_events
// while logging the retain-snapshot warning, a promise the exiting
// process cannot keep.
//
// Asserts the drain both happens AND carries a context that is still
// live at write time.
func TestRun_ShutdownDrain_UsesLiveContext(t *testing.T) {
	src := &stubStatsSource{stats: dispatcher.Stats{
		EventsSeen: map[string]int{"band": 7},
	}}
	w := &fakeStatsWriter{}
	// A long interval guarantees the ticker never fires, so any write
	// observed here came from the shutdown drain and nothing else.
	f := New(src, w, slog.New(slog.NewTextHandler(io.Discard, nil)),
		Options{Interval: time.Hour})

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}

	if len(w.calls) != 1 {
		t.Fatalf("InsertDecoderStats called %d times, want 1 (the shutdown drain)", len(w.calls))
	}
	if w.ctxErrs[0] != nil {
		t.Errorf("drain wrote with ctx.Err() = %v, want nil — an already-cancelled context is rejected by database/sql before it reaches Postgres, so the final bucket is silently lost", w.ctxErrs[0])
	}
	if len(w.calls[0]) != 1 || w.calls[0][0].EventsSeen != 7 {
		t.Errorf("drain rows = %+v, want one band row with EventsSeen 7", w.calls[0])
	}
}
