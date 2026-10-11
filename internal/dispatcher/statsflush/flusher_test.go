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
// *timescale.Store cannot play this role: Store.Open pings the DB, so a store that succeeds and
// then fails a later call needs a real reachable Postgres.
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

// TestFlushAt_WriteFailure_RetainsLastSnapshot pins that flushAt does not advance f.last when
// InsertDecoderStats fails; that would permanently discard the window's deltas, since the next
// tick would diff against a snapshot that was never durably written.
//
// Two failing ticks then a successful one: the third write must carry the full cumulative delta
// across all three windows.
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

	// InsertDecoderStats is attempted on every tick (the fake records all 3), but only the third
	// call's rows landed. Its delta must be the full 40 from the zero baseline, not 15: if failed
	// flushes advanced the snapshot (10, then 25), flush 3 would diff against 25 and lose the
	// first 25 events forever.
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

// TestRun_ShutdownDrain_UsesLiveContext guards the shutdown drain: Run's ctx.Done() arm must not
// call f.flush(ctx) with the context that just fired. database/sql checks ctx.Err() before
// acquiring a connection, so the final flush would never write and every restart would drop up
// to a full interval of events_seen / decode_errors / orphan_events.
//
// Asserts the drain happens and carries a context still live at write time.
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

// TestFlushAt_PromotesDispatcherCountersToPrometheus pins that
// TxReadErrors, TxEventReadErrors and EntryMetaUnsupported are not WARN-log-only: each needs a
// Prometheus series an alert rule can key off. One window with all three nonzero must log the
// WARN and add the exact delta to the matching counter.
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

// TestFlushAt_PromotesEvictedKeysUnreadable pins that a ledger whose
// evicted-key list failed to read reaches the alertable counter and the WARN
// exactly once, and that a second tick at the same total adds nothing.
func TestFlushAt_PromotesEvictedKeysUnreadable(t *testing.T) {
	before := testutil.ToFloat64(obs.DispatcherEvictedKeysUnreadableTotal)
	src := &stubStatsSource{stats: dispatcher.Stats{EvictedKeysUnreadable: 2}}
	var buf bytes.Buffer
	f := New(src, &fakeStatsWriter{}, slog.New(slog.NewTextHandler(&buf, nil)), Options{Interval: 5 * time.Minute})

	f.flushAt(context.Background(), time.Now())
	f.flushAt(context.Background(), time.Now())

	if got := testutil.ToFloat64(obs.DispatcherEvictedKeysUnreadableTotal) - before; got != 2 {
		t.Errorf("DispatcherEvictedKeysUnreadableTotal delta = %v, want 2", got)
	}
	if n := strings.Count(buf.String(), "evicted ledger keys unreadable"); n != 1 {
		t.Errorf("WARN emitted %d times over two ticks at the same total, want 1\nfull log: %s", n, buf.String())
	}
}

// TestFlushAt_PromotesLedgerUpgradeEntries pins that upgrade entries reach
// the counter once, and a second tick at the same total adds nothing.
func TestFlushAt_PromotesLedgerUpgradeEntries(t *testing.T) {
	before := testutil.ToFloat64(obs.DispatcherLedgerUpgradeEntriesTotal)
	src := &stubStatsSource{stats: dispatcher.Stats{LedgerUpgradeEntries: 3}}
	f := New(src, &fakeStatsWriter{}, slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)), Options{Interval: 5 * time.Minute})

	f.flushAt(context.Background(), time.Now())
	f.flushAt(context.Background(), time.Now())

	if got := testutil.ToFloat64(obs.DispatcherLedgerUpgradeEntriesTotal) - before; got != 3 {
		t.Errorf("DispatcherLedgerUpgradeEntriesTotal delta = %v, want 3", got)
	}
}

// TestFlushAt_EntryMetaUnsupported_SnapshotAdvances_NoLatch pins that the
// end-of-flush snapshot includes EntryMetaUnsupported; otherwise f.last stays 0, every delta
// equals the full cumulative total, and the WARN fires every window instead of only on new
// occurrences.
//
// Two ticks with the same cumulative value must WARN exactly once, from the first.
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

// TestFlushAt_ObsCounters_SurviveWriteFailure_NoLatch pins that flushAt does not derive the
// dispatcher-level obs-counter deltas (TxReadErrors, TxEventReadErrors, EntryMetaUnsupported)
// from f.last, which is deliberately held back on an InsertDecoderStats failure; otherwise the
// next stable-count tick recomputes the same positive delta and re-emits obs.Add and the WARN.
//
// A failing tick with a fresh delta, then a recovered tick with no movement, must fire the
// counter and the WARN once, from the first tick.
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

// TestFlushAt_UncorroboratedCalls_EmitsWarnAndMetric proves
// UncorroboratedCalls now has a consumer: a rejected oracle price-forgery
// attempt (or a routing-shape change) reaches both a Prometheus counter and
// a WARN on a fresh delta, instead of vanishing silently.
func TestFlushAt_UncorroboratedCalls_EmitsWarnAndMetric(t *testing.T) {
	before := testutil.ToFloat64(obs.SourceUncorroboratedCallsTotal.WithLabelValues("band"))

	src := &stubStatsSource{stats: dispatcher.Stats{
		UncorroboratedCalls: map[string]int{"band": 2},
	}}
	w := &fakeStatsWriter{}
	var buf bytes.Buffer
	f := New(src, w, slog.New(slog.NewTextHandler(&buf, nil)), Options{Interval: 5 * time.Minute})

	f.flushAt(context.Background(), time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC))

	if got := testutil.ToFloat64(obs.SourceUncorroboratedCallsTotal.WithLabelValues("band")) - before; got != 2 {
		t.Errorf("SourceUncorroboratedCallsTotal{source=band} delta = %v, want 2", got)
	}
	if got := strings.Count(buf.String(), "uncorroborated oracle calls during this flush window"); got != 1 {
		t.Errorf("WARN logged %d times, want 1", got)
	}
	if !strings.Contains(buf.String(), "source=band") {
		t.Errorf("WARN missing source label, got: %s", buf.String())
	}
}

// TestFlushAt_UncorroboratedCalls_SurviveWriteFailure_NoLatch proves the
// obsLast baseline for this counter is independent of f.last: a failed
// InsertDecoderStats must not make the NEXT, flat-count tick recompute a
// stale positive delta against a held-back baseline.
func TestFlushAt_UncorroboratedCalls_SurviveWriteFailure_NoLatch(t *testing.T) {
	before := testutil.ToFloat64(obs.SourceUncorroboratedCallsTotal.WithLabelValues("band"))

	src := &stubStatsSource{stats: dispatcher.Stats{
		EventsSeen:          map[string]int{"band": 10},
		UncorroboratedCalls: map[string]int{"band": 5},
	}}
	w := &fakeStatsWriter{fail: true}
	var buf bytes.Buffer
	f := New(src, w, slog.New(slog.NewTextHandler(&buf, nil)), Options{Interval: 5 * time.Minute})

	base := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)

	// Tick 1: fresh delta of 5. InsertDecoderStats fails, so f.last (the
	// DB-row baseline) is retained — but the obs-counter baseline must
	// still advance.
	f.flushAt(context.Background(), base)
	if got := f.obsLast.UncorroboratedCalls["band"]; got != 5 {
		t.Fatalf("after flush 1, f.obsLast.UncorroboratedCalls[band] = %d, want 5 (must advance even on write failure)", got)
	}

	// Tick 2: store recovers, but the count hasn't moved (still 5). Must
	// NOT recompute a stale positive delta against a held-back baseline.
	w.fail = false
	f.flushAt(context.Background(), base.Add(5*time.Minute))

	if got := testutil.ToFloat64(obs.SourceUncorroboratedCallsTotal.WithLabelValues("band")) - before; got != 5 {
		t.Errorf("SourceUncorroboratedCallsTotal{source=band} delta across both ticks = %v, want 5 (must not re-add on the second, flat-count tick)", got)
	}
	if got := strings.Count(buf.String(), "uncorroborated oracle calls during this flush window"); got != 1 {
		t.Errorf("WARN logged %d times across write-failure + flat-count ticks, want 1", got)
	}
}
