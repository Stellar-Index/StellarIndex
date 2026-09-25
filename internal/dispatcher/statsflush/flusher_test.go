package statsflush

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/dispatcher"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
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

// TestFlushAt_PromotesDispatcherCountersToPrometheus is the regression
// test for RLT-135: TxReadErrors, TxEventReadErrors and
// EntryMetaUnsupported used to be WARN-log-only, with no Prometheus
// series an alert rule or dashboard could key off. A single flush
// window with all three nonzero must both log the WARN (unchanged
// behaviour) and add the exact delta to the matching counter.
func TestFlushAt_PromotesDispatcherCountersToPrometheus(t *testing.T) {
	before := struct{ txRead, txEvent, entryMeta float64 }{
		testutil.ToFloat64(obs.DispatcherTxReadErrorsTotal),
		testutil.ToFloat64(obs.DispatcherTxEventReadErrorsTotal),
		testutil.ToFloat64(obs.DispatcherEntryMetaUnsupportedTotal),
	}

	src := &stubStatsSource{stats: dispatcher.Stats{
		TxReadErrors:         3,
		TxEventReadErrors:    2,
		EntryMetaUnsupported: 5,
	}}
	w := &fakeStatsWriter{}
	var buf bytes.Buffer
	f := New(src, w, slog.New(slog.NewTextHandler(&buf, nil)), Options{Interval: 5 * time.Minute})

	f.flushAt(context.Background(), time.Now())

	if got := testutil.ToFloat64(obs.DispatcherTxReadErrorsTotal) - before.txRead; got != 3 {
		t.Errorf("DispatcherTxReadErrorsTotal delta = %v, want 3", got)
	}
	if got := testutil.ToFloat64(obs.DispatcherTxEventReadErrorsTotal) - before.txEvent; got != 2 {
		t.Errorf("DispatcherTxEventReadErrorsTotal delta = %v, want 2", got)
	}
	if got := testutil.ToFloat64(obs.DispatcherEntryMetaUnsupportedTotal) - before.entryMeta; got != 5 {
		t.Errorf("DispatcherEntryMetaUnsupportedTotal delta = %v, want 5", got)
	}

	out := buf.String()
	for _, want := range []string{"tx-read errors", "tx-event read errors", "unsupported TransactionMeta version"} {
		if !strings.Contains(out, want) {
			t.Errorf("log output missing %q\nfull log: %s", want, out)
		}
	}
}

// TestFlushAt_EntryMetaUnsupported_SnapshotAdvances_NoLatch is the
// regression test for T110/RLT-135: the end-of-flush snapshot used to
// omit EntryMetaUnsupported, so f.last.EntryMetaUnsupported stayed 0
// forever and the delta at every subsequent tick equalled the full
// cumulative total — the WARN fired on every flush window for the
// life of the process instead of only when NEW occurrences appeared
// in that window.
//
// Drives two ticks with the SAME cumulative EntryMetaUnsupported value
// (i.e. no new occurrences between them) and asserts the WARN appears
// exactly once — from the first tick, which had a genuine delta — not
// twice.
func TestFlushAt_EntryMetaUnsupported_SnapshotAdvances_NoLatch(t *testing.T) {
	src := &stubStatsSource{stats: dispatcher.Stats{EntryMetaUnsupported: 4}}
	w := &fakeStatsWriter{}
	var buf bytes.Buffer
	f := New(src, w, slog.New(slog.NewTextHandler(&buf, nil)), Options{Interval: 5 * time.Minute})

	base := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	// Tick 1: fresh delta of 4 against the zero-value baseline. WARN fires.
	f.flushAt(context.Background(), base)
	if got := f.obsLast.EntryMetaUnsupported; got != 4 {
		t.Fatalf("after flush 1, f.obsLast.EntryMetaUnsupported = %d, want 4 (snapshot must advance)", got)
	}

	// Tick 2: no new occurrences — current stays at 4. Must NOT re-warn.
	f.flushAt(context.Background(), base.Add(5*time.Minute))

	got := strings.Count(buf.String(), "unsupported TransactionMeta version during this flush window")
	if got != 1 {
		t.Errorf("WARN logged %d times across 2 flat-delta ticks, want 1 (must not latch on the cumulative total forever)", got)
	}
}

// TestFlushAt_ObsCounters_SurviveWriteFailure_NoLatch is the
// regression test for CA2-A25-harden-3: flushAt used to derive the
// dispatcher-level obs-counter deltas (TxReadErrors,
// TxEventReadErrors, EntryMetaUnsupported) from the SAME baseline
// (f.last) that INT-05 deliberately holds back on an
// InsertDecoderStats failure. A failed-insert tick followed by a
// stable-count tick (no new occurrences) then recomputed the
// identical positive delta a second time, re-emitting the same
// obs.Add and WARN.
//
// Drives a failing tick with a fresh TxReadErrors delta, then a
// second tick where the store recovers but the counter hasn't moved,
// and asserts both the Prometheus counter and the WARN fire only
// once — from the first tick.
func TestFlushAt_ObsCounters_SurviveWriteFailure_NoLatch(t *testing.T) {
	before := testutil.ToFloat64(obs.DispatcherTxReadErrorsTotal)

	src := &stubStatsSource{stats: dispatcher.Stats{
		EventsSeen:   map[string]int{"band": 10},
		TxReadErrors: 3,
	}}
	w := &fakeStatsWriter{fail: true}
	var buf bytes.Buffer
	f := New(src, w, slog.New(slog.NewTextHandler(&buf, nil)), Options{Interval: 5 * time.Minute})

	base := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

	// Tick 1: fresh TxReadErrors delta of 3. InsertDecoderStats fails,
	// so f.last (the DB-row baseline) is retained — but the obs-counter
	// baseline must still advance.
	f.flushAt(context.Background(), base)
	if got := f.obsLast.TxReadErrors; got != 3 {
		t.Fatalf("after flush 1, f.obsLast.TxReadErrors = %d, want 3 (must advance even on write failure)", got)
	}

	// Tick 2: store recovers, but TxReadErrors hasn't moved (still 3).
	// Must NOT recompute a stale positive delta against the held-back
	// f.last.
	w.fail = false
	f.flushAt(context.Background(), base.Add(5*time.Minute))

	if got := testutil.ToFloat64(obs.DispatcherTxReadErrorsTotal) - before; got != 3 {
		t.Errorf("DispatcherTxReadErrorsTotal delta across both ticks = %v, want 3 (must not re-add on the second, flat-count tick)", got)
	}
	if got := strings.Count(buf.String(), "tx-read errors during this flush window"); got != 1 {
		t.Errorf("WARN logged %d times across write-failure + flat-count ticks, want 1", got)
	}
}
