package projector

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
	"github.com/Stellar-Index/StellarIndex/internal/worker/guardscan"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/sources/comet"
)

// fakeDecoder is a configurable dispatcher.Decoder for the X9
// panic-isolation tests.
type fakeDecoder struct {
	matches  bool
	panics   bool
	err      error
	outs     []consumer.Event
	decodeHi int // count of Decode calls
}

func (f *fakeDecoder) Name() string              { return "fake" }
func (f *fakeDecoder) Matches(events.Event) bool { return f.matches }
func (f *fakeDecoder) Decode(events.Event) ([]consumer.Event, error) {
	f.decodeHi++
	if f.panics {
		panic("boom: poison / upgraded-WASM row")
	}
	return f.outs, f.err
}

// fakeEvent is a minimal consumer.Event for sink-counting in tests.
type fakeEvent struct{}

func (fakeEvent) EventKind() string { return "fake.event" }
func (fakeEvent) Source() string    { return "fake" }

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// TestProcessEventSafely_RecoversDecoderPanic pins X9:
// a decoder panic on one poison lake row must be recovered + counted as a
// soft-fail, NOT crash the live indexer.
func TestProcessEventSafely_RecoversDecoderPanic(t *testing.T) {
	src := Source{Name: "x", Decoder: &fakeDecoder{matches: true, panics: true}}
	sinked := 0
	emitted, decodeFail, sinkErr := processEventSafely(src, events.Event{Ledger: 42},
		func(consumer.Event) error { sinked++; return nil }, discardLog())
	if !decodeFail {
		t.Error("a decoder panic must be a decode soft-fail (counted), not propagate")
	}
	if sinkErr != nil {
		t.Errorf("sinkErr = %v, want nil on a decode panic (nothing was written)", sinkErr)
	}
	if emitted != 0 || sinked != 0 {
		t.Errorf("emitted=%d sinked=%d, want 0/0 on panic", emitted, sinked)
	}
}

// TestProcessEventSafely_SinkErrorPropagates pins that a downstream sink
// write failure must be RETURNED to the caller (not swallowed), with
// `emitted` counting only the outputs that committed before it.
func TestProcessEventSafely_SinkErrorPropagates(t *testing.T) {
	boom := errors.New("deadlock detected")
	// Two decoded outputs; the sink fails on the SECOND, so the first is
	// counted committed and the error surfaces.
	d := &fakeDecoder{matches: true, outs: []consumer.Event{fakeEvent{}, fakeEvent{}}}
	calls := 0
	emitted, decodeFail, sinkErr := processEventSafely(Source{Name: "x", Decoder: d}, events.Event{Ledger: 7},
		func(consumer.Event) error {
			calls++
			if calls == 2 {
				return boom
			}
			return nil
		}, discardLog())
	if decodeFail {
		t.Error("a sink failure is NOT a decode failure")
	}
	if !errors.Is(sinkErr, boom) {
		t.Errorf("sinkErr = %v, want the propagated sink error", sinkErr)
	}
	if emitted != 1 {
		t.Errorf("emitted = %d, want 1 (only the pre-failure output committed)", emitted)
	}
}

// TestProcessEventSafely_DecodeErrorIsSoftFail — a returned decode error is
// the existing soft-fail path; it must not sink and must flag softFail.
func TestProcessEventSafely_DecodeErrorIsSoftFail(t *testing.T) {
	src := Source{Name: "x", Decoder: &fakeDecoder{matches: true, err: errors.New("bad row")}}
	sinked := 0
	emitted, decodeFail, sinkErr := processEventSafely(src, events.Event{}, func(consumer.Event) error { sinked++; return nil }, discardLog())
	if !decodeFail || emitted != 0 || sinked != 0 || sinkErr != nil {
		t.Errorf("decode error: decodeFail=%v emitted=%d sinked=%d sinkErr=%v, want true/0/0/nil", decodeFail, emitted, sinked, sinkErr)
	}
}

// TestProcessEventSafely_HappyPathSinks — a clean decode sinks each output and
// reports no soft-fail.
func TestProcessEventSafely_HappyPathSinks(t *testing.T) {
	src := Source{Name: "x", Decoder: &fakeDecoder{matches: true, outs: []consumer.Event{fakeEvent{}, fakeEvent{}}}}
	sinked := 0
	emitted, decodeFail, sinkErr := processEventSafely(src, events.Event{}, func(consumer.Event) error { sinked++; return nil }, discardLog())
	if decodeFail || emitted != 2 || sinked != 2 || sinkErr != nil {
		t.Errorf("happy: decodeFail=%v emitted=%d sinked=%d sinkErr=%v, want false/2/2/nil", decodeFail, emitted, sinked, sinkErr)
	}
}

// TestProcessEventSafely_NonMatchSkips — a non-matching row is neither sinked
// nor a soft-fail (and Decode is never called).
func TestProcessEventSafely_NonMatchSkips(t *testing.T) {
	d := &fakeDecoder{matches: false}
	sinked := 0
	emitted, decodeFail, sinkErr := processEventSafely(Source{Name: "x", Decoder: d}, events.Event{},
		func(consumer.Event) error { sinked++; return nil }, discardLog())
	if decodeFail || emitted != 0 || sinked != 0 || d.decodeHi != 0 || sinkErr != nil {
		t.Errorf("non-match: decodeFail=%v emitted=%d sinked=%d decodeCalls=%d sinkErr=%v, want false/0/0/0/nil", decodeFail, emitted, sinked, d.decodeHi, sinkErr)
	}
}

func oracleConfigEmpty() config.OracleConfig { return config.OracleConfig{} }

// TestNew_DefaultsLogger checks the nil-logger branch picks up
// slog.Default rather than panicking on the first Info call.
func TestNew_DefaultsLogger(t *testing.T) {
	p := New(nil, Registry{}, func(context.Context, consumer.Event) error { return nil }, nil)
	if p == nil {
		t.Fatal("New returned nil")
	}
	if p.logger == nil {
		t.Fatal("expected logger to default to slog.Default")
	}
}

// TestRun_NilStoreReturnsError checks the guard in Run: a Projector
// constructed with a nil store should reject Run immediately rather
// than panicking later when a goroutine touches the store.
func TestRun_NilStoreReturnsError(t *testing.T) {
	p := New(nil, Registry{Sources: []Source{{Name: "x"}}},
		func(context.Context, consumer.Event) error { return nil }, slog.Default())
	if err := p.Run(context.Background()); err == nil {
		t.Fatal("expected non-nil error from Run with nil store")
	}
}

// TestRun_NilSinkReturnsError checks the guard in Run rejects a
// nil sink before launching any goroutines.
func TestRun_NilSinkReturnsError(t *testing.T) {
	p := &Projector{
		registry: Registry{Sources: []Source{{Name: "x"}}},
		logger:   slog.Default(),
	}
	if err := p.Run(context.Background()); err == nil {
		t.Fatal("expected non-nil error from Run with nil sink")
	}
}

// TestBuildRegistry_UnknownSourceIsSilent confirms enabled-sources
// names that aren't in the projector's dispatch table (sdex, band,
// external CEX/FX) are silently skipped — they're handled
// elsewhere per ADR-0032 § "Out of scope".
func TestBuildRegistry_UnknownSourceIsSilent(t *testing.T) {
	reg, err := BuildRegistry([]string{"sdex", "binance", "kraken", "band"}, oracleConfigEmpty(), nil, nil)
	if err != nil {
		t.Fatalf("BuildRegistry: unexpected error: %v", err)
	}
	if len(reg.Sources) != 0 {
		t.Fatalf("expected 0 in-scope sources for sdex/binance/kraken/band, got %d", len(reg.Sources))
	}
}

// TestBuildRegistry_SEP41NeedsWatchedSet pins the sep41 projector
// sources reproduce the dispatcher's WATCHED set, not a firehose. With no
// watched contracts they're skipped (the dispatcher writes nothing
// either); with a watched set they're registered.
func TestBuildRegistry_SEP41NeedsWatchedSet(t *testing.T) {
	names := []string{"sep41_transfers", "sep41_supply"}

	reg, err := BuildRegistry(names, oracleConfigEmpty(), nil, nil)
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	if len(reg.Sources) != 0 {
		t.Fatalf("no watched sep41 contracts → expected 0 sources, got %d", len(reg.Sources))
	}

	reg, err = BuildRegistry(names, oracleConfigEmpty(), []string{"CWATCHEDCONTRACT0000000000000000000000000000000000000000"}, nil)
	if err != nil {
		t.Fatalf("BuildRegistry (watched): %v", err)
	}
	if len(reg.Sources) != 2 {
		t.Fatalf("watched sep41 contracts → expected 2 sources, got %d", len(reg.Sources))
	}
}

// TestBuildRegistry_SEP41RegisteredWithoutEnabledSourcesEntry pins the
// zero-writer case: the sep41 names are NOT in
// config.KnownSources, so production's enabled_sources can never carry
// them — yet the dispatcher unconditionally cedes the sep41 domain to
// the projector. BuildRegistry must therefore
// register the sep41 sources from the watched-contract set alone, with
// no enabled-sources entry. r1 ran ~14 days (ledgers 63,419,139+) with
// zero sep41 writers because only the explicit-name path was tested.
func TestBuildRegistry_SEP41RegisteredWithoutEnabledSourcesEntry(t *testing.T) {
	watched := []string{"CWATCHEDCONTRACT0000000000000000000000000000000000000000"}

	// Production shape: enabled_sources has no sep41 names.
	reg, err := BuildRegistry([]string{"soroswap"}, oracleConfigEmpty(), watched, nil)
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	got := map[string]int{}
	for _, s := range reg.Sources {
		got[s.Name]++
	}
	for _, want := range []string{"sep41_transfers", "sep41_supply"} {
		if got[want] != 1 {
			t.Fatalf("source %q: want exactly 1 registration with a watched set and no enabled_sources entry, got %d (registry: %v)", want, got[want], got)
		}
	}

	// Explicit-name shape (projected-rebuild -source): no duplicates.
	reg, err = BuildRegistry([]string{"sep41_supply"}, oracleConfigEmpty(), watched, nil)
	if err != nil {
		t.Fatalf("BuildRegistry (explicit): %v", err)
	}
	count := 0
	for _, s := range reg.Sources {
		if s.Name == "sep41_supply" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("explicit sep41_supply request: want exactly 1 registration, got %d", count)
	}
}

// TestBuildRegistry_IncludesInScopeSources confirms an enabled-
// sources list with on-chain Soroban protocols produces matching
// projector.Source entries. Order-dependent so we map names.
func TestBuildRegistry_IncludesInScopeSources(t *testing.T) {
	names := []string{"aquarius", "phoenix", "comet", "blend", "blend_backstop", "cctp", "rozo", "soroswap", "defindex"}
	reg, err := BuildRegistry(names, oracleConfigEmpty(), nil, nil)
	if err != nil {
		t.Fatalf("BuildRegistry: %v", err)
	}
	if len(reg.Sources) != len(names) {
		t.Fatalf("expected %d sources, got %d", len(names), len(reg.Sources))
	}
	got := map[string]bool{}
	for _, s := range reg.Sources {
		got[s.Name] = true
	}
	for _, n := range names {
		if !got[n] {
			t.Errorf("expected registry to include source %q", n)
		}
	}
}

// TestAdaptiveWindow pins the shrink/recover arithmetic for the
// dense-window stall: deadline-exceeded halves exactly toward
// MinBatchLimit and never below; success doubles back to BatchLimit. The
// cycle-level sink-budget arm is pinned by
// TestCycle_SinkBudgetExhaustionShrinksWindowAndHoldsCursor.
func TestAdaptiveWindow(t *testing.T) {
	w := uint32(BatchLimit)
	for i := 0; i < 20; i++ {
		next, shrunk := shrinkWindow(w, context.DeadlineExceeded)
		want, wantShrunk := w, false
		if w > MinBatchLimit {
			want, wantShrunk = max(w/2, MinBatchLimit), true
		}
		if next != want || shrunk != wantShrunk {
			t.Fatalf("shrinkWindow(%d) = (%d, %v), want (%d, %v)", w, next, shrunk, want, wantShrunk)
		}
		w = next
	}
	if w != MinBatchLimit {
		t.Fatalf("converged to %d, want %d", w, MinBatchLimit)
	}
	if _, shrunk := shrinkWindow(BatchLimit, errors.New("boom")); shrunk {
		t.Fatal("non-deadline error must not shrink")
	}
	for i := 0; i < 20 && w < BatchLimit; i++ {
		w = recoverWindow(w)
	}
	if w != BatchLimit {
		t.Fatalf("recovered to %d, want %d", w, BatchLimit)
	}
}

// TestRun_PanicOutsideRowIsRecovered pins that a panic in a source's cycle
// machinery (not a row — those have their own recover) or in the replay-window
// watcher stops only that goroutine: the process survives, worker_panics_total
// moves, and once every goroutine has stopped Run returns an error rather than
// the nil a clean exit would give. A zero-value Store nil-derefs its *sql.DB on
// the first read in both.
func TestRun_PanicOutsideRowIsRecovered(t *testing.T) {
	const src = "panic-outside-row"
	srcCounter := obs.WorkerPanicsTotal.WithLabelValues(sourceWorkerName(src))
	watchCounter := obs.WorkerPanicsTotal.WithLabelValues(replayWindowWorkerName)
	srcBefore, watchBefore := testutil.ToFloat64(srcCounter), testutil.ToFloat64(watchCounter)

	p := New(&timescale.Store{}, Registry{Sources: []Source{{Name: src}}},
		func(context.Context, consumer.Event) error { return nil }, discardLog())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	deadline := time.Now().Add(5 * time.Second)
	for testutil.ToFloat64(srcCounter) == srcBefore || testutil.ToFloat64(watchCounter) == watchBefore {
		if time.Now().After(deadline) {
			t.Fatalf("worker_panics_total did not move: source %v→%v, watcher %v→%v",
				srcBefore, testutil.ToFloat64(srcCounter), watchBefore, testutil.ToFloat64(watchCounter))
		}
		time.Sleep(5 * time.Millisecond)
	}

	select {
	case err := <-done:
		if err == nil || errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned %v, want a non-cancellation error naming the stopped goroutines", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after every goroutine stopped — a panicked goroutine never released the WaitGroup")
	}
}

// TestProjectorGoroutinesRecover is the package-wide guard: every `go`
// statement in this package's non-test files must defer worker.Recover. The
// indexer's own guard test scans main.go only and cannot see the goroutines
// Run fans out. Proven red: removing either guard in Run fails it by line.
func TestProjectorGoroutinesRecover(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	scanner := guardscan.NewScanner(guardscan.Config{Guards: []string{"worker.Recover", "worker.Report"}})
	checked := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		sites, err := scanner.ScanFile(f)
		if err != nil {
			t.Fatalf("scan %s: %v", f, err)
		}
		for _, s := range sites {
			checked++
			if s.Kind == guardscan.KindUnresolved || !s.Recovers {
				t.Errorf("go %s at %s:%d does not defer worker.Recover — a panic there kills the "+
					"indexer without draining its sinks", s.Target, f, s.Line)
			}
		}
	}
	if checked < 2 {
		t.Errorf("discovered %d goroutine(s) in internal/projector, want at least 2 (Run's watcher "+
			"and per-source fan-out) — the scan has drifted from the code", checked)
	}
}

// TestBuildRegistry_FoldsWhitespaceAndCaseInSourceNames pins that the
// registry's own name lookup must normalise ingestion.enabled_sources
// entries the SAME way internal/config/validate.go's KnownSources check
// (which trims + lowercases) and internal/pipeline.BuildDispatcher already
// do. Lowercasing without TrimSpace would let a name with
// leading/trailing whitespace — accepted by config.Validate —
// silently miss its projector entry (buildSource's default case returns
// ok=false with no error) instead of registering comet.
func TestBuildRegistry_FoldsWhitespaceAndCaseInSourceNames(t *testing.T) {
	reg, err := BuildRegistry([]string{"  Comet  "}, config.OracleConfig{}, nil, nil)
	if err != nil {
		t.Fatalf("BuildRegistry(%q): %v", "  Comet  ", err)
	}
	for _, s := range reg.Sources {
		if s.Name == comet.SourceName {
			return
		}
	}
	t.Fatalf("BuildRegistry(%q) did not register the comet source; got %d sources", "  Comet  ", len(reg.Sources))
}

// The projector skips a failed row and advances the cursor past it,
// so the failure itself must reach the operator — the decode error text in the
// log, and a panic in the stellarindex_decoder_panicked page counter.
func TestProcessEventSafely_DecodeErrorIsLoggedWithRowCoordinate(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	src := Source{Name: "rlt133_err", Decoder: &fakeDecoder{matches: true, err: errors.New("unknown map field amount_v9")}}
	_, decodeFail, _ := processEventSafely(src, events.Event{Ledger: 61234567, TxHash: "abc"},
		func(consumer.Event) error { return nil }, log)
	if !decodeFail {
		t.Fatal("decode error must still be a soft-fail")
	}
	out := buf.String()
	for _, want := range []string{"unknown map field amount_v9", "source=rlt133_err", "ledger=61234567", "tx=abc"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q missing %q", out, want)
		}
	}
}

func TestProcessEventSafely_PanicCountsDecoderPanicsTotal(t *testing.T) {
	const name = "rlt133_panic"
	before := testutil.ToFloat64(obs.DecoderPanicsTotal.WithLabelValues(name))
	src := Source{Name: name, Decoder: &fakeDecoder{matches: true, panics: true}}
	_, decodeFail, _ := processEventSafely(src, events.Event{Ledger: 42},
		func(consumer.Event) error { return nil }, discardLog())
	if !decodeFail {
		t.Fatal("decode panic must still be a soft-fail")
	}
	if got := testutil.ToFloat64(obs.DecoderPanicsTotal.WithLabelValues(name)) - before; got != 1 {
		t.Errorf("DecoderPanicsTotal{source=%q} delta = %v, want 1", name, got)
	}
}

// A carried output from an earlier ledger that fails to sink must hold the
// cursor below ITS ledger, not the scanned row's, or the retry never re-reads
// the buffer state that produced it.
func TestProcessEventSafely_HeldLedgerIsOutputLedger(t *testing.T) {
	d := &fakeDecoder{matches: true, outs: []consumer.Event{carriedEvent{ledger: 90}, fakeEvent{}}}
	_, _, sinkErr := processEventSafely(Source{Name: "x", Decoder: d}, events.Event{Ledger: 100},
		func(consumer.Event) error { return errors.New("deadlock detected") }, discardLog())
	f := rowFaultsOf(sinkErr)
	if f.held == nil || f.heldLedger != 90 {
		t.Fatalf("held=%v heldLedger=%d, want a held fault at ledger 90", f.held, f.heldLedger)
	}
}

// TestProcessEventSafely_ContinuesPastPermanentStopsAtRetryable pins the loop
// contract at the unit: permanent drops are collected and skipped over, the
// first retryable/unclassified fault stops the row, and both are reachable
// from the returned error.
func TestProcessEventSafely_ContinuesPastPermanentStopsAtRetryable(t *testing.T) {
	deadlock := errors.New("deadlock detected") // unclassified → holds
	outs := []consumer.Event{fakeEvent{}, fakeEvent{}, fakeEvent{}, fakeEvent{}, fakeEvent{}}
	d := &fakeDecoder{matches: true, outs: outs}
	calls := 0
	emitted, decodeFail, sinkErr := processEventSafely(Source{Name: "x", Decoder: d}, events.Event{Ledger: 9},
		func(consumer.Event) error {
			calls++
			switch calls {
			case 1:
				return canonical.ErrInvalidTrade // permanent → skip, continue
			case 2:
				return nil
			case 3:
				return canonical.ErrInvalidAmount // permanent → skip, continue
			case 4:
				return deadlock // stops the row
			default:
				return nil // never reached
			}
		}, discardLog())
	if decodeFail {
		t.Error("a sink fault is not a decode failure")
	}
	if calls != 4 {
		t.Errorf("sink called %d times, want 4 (continue past 2 drops, stop at the unclassified fault)", calls)
	}
	if emitted != 1 {
		t.Errorf("emitted = %d, want 1 — only the output that landed", emitted)
	}
	// Asserted through the standard multi-error shape rather than the concrete
	// type, so this test compiles — and fails on the VALUES — against a loop
	// that still stops at the first error.
	multi, ok := sinkErr.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("sinkErr = %v (%T), want an error carrying every fault of the row (Unwrap() []error)", sinkErr, sinkErr)
	}
	if got := len(multi.Unwrap()); got != 3 {
		t.Errorf("sinkErr carries %d faults, want 3 (two permanent drops + the fault that stopped the row)", got)
	}
	if !errors.Is(sinkErr, deadlock) || !errors.Is(sinkErr, canonical.ErrInvalidTrade) {
		t.Errorf("sinkErr must unwrap to every fault it carries; got %v", sinkErr)
	}
}
