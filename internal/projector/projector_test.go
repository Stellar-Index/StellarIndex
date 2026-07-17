package projector

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
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

// TestProcessEventSafely_RecoversDecoderPanic pins X9 (audit-2026-06-14):
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

// TestProcessEventSafely_SinkErrorPropagates pins C2-1: a downstream sink
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

// TestBuildRegistry_SEP41NeedsWatchedSet pins F-1316: the sep41 projector
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
// 2026-07-10 dense-window stall: deadline-exceeded halves toward
// MinBatchLimit and never below; success doubles back to BatchLimit.
func TestAdaptiveWindow(t *testing.T) {
	w := uint32(BatchLimit)
	for i := 0; i < 20; i++ {
		next, shrunk := shrinkWindow(w, context.DeadlineExceeded)
		if w > MinBatchLimit && !shrunk {
			t.Fatalf("expected shrink at window %d", w)
		}
		if w <= MinBatchLimit && shrunk {
			t.Fatalf("shrunk below floor from %d", w)
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
