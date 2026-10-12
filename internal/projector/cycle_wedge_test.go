package projector

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/obs"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"

	"github.com/stellar/go-stellar-sdk/strkey"
	"github.com/stellar/go-stellar-sdk/xdr"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap"
	"github.com/Stellar-Index/StellarIndex/internal/sources/spectra"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sushiswap_v3"
)

// Harness: an in-memory eventStore driving the cursor-durability state machine without Postgres; the tests pin WHEN the
// cursor may move, so the cursor is the assertion surface.

type fakeStore struct {
	mu sync.Mutex

	projectorCursor uint32 // last_ledger for ("projector", src)
	haveCursor      bool
	tipLedger       uint32 // last_ledger for ("ledgerstream", "")
	// rows back the soroban_events read, used only when the projector has
	// no lake source (soroban_events mode).
	rows    []sorobanevents.Row
	upserts int

	// cursorErr fails the ("projector", src) cursor read.
	cursorErr error

	// dirtyWindows / dirtyErr back ProjectionDirtyWindows — the
	// operator-recorded projector-replay rewind windows the
	// replay-window watcher reads (see replay_window_test.go).
	dirtyWindows map[string]timescale.ProjectionDirtyWindow
	dirtyErr     error

	// dirtyBlock, when non-nil, makes ProjectionDirtyWindows block until the channel closes
	// or ctx is done, standing in for a query stuck behind a lock wait. dirtyDeadline records
	// the deadline of the ctx it got (the read must not run on the un-timeout'd root ctx).
	dirtyBlock    chan struct{}
	dirtyDeadline time.Time
	dirtyHadDL    bool
	dirtySeen     bool
}

func (f *fakeStore) ProjectionDirtyWindows(ctx context.Context) (map[string]timescale.ProjectionDirtyWindow, error) {
	f.mu.Lock()
	dl, hadDL := ctx.Deadline()
	f.dirtyDeadline, f.dirtyHadDL, f.dirtySeen = dl, hadDL, true
	block := f.dirtyBlock
	f.mu.Unlock()

	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.dirtyErr != nil {
		return nil, f.dirtyErr
	}
	out := make(map[string]timescale.ProjectionDirtyWindow, len(f.dirtyWindows))
	for k, v := range f.dirtyWindows {
		out[k] = v
	}
	return out, nil
}

func (f *fakeStore) GetCursor(_ context.Context, source, _ string) (timescale.Cursor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch source {
	case "ledgerstream":
		return timescale.Cursor{Source: source, LastLedger: f.tipLedger}, nil
	case "projector":
		if f.cursorErr != nil {
			return timescale.Cursor{}, f.cursorErr
		}
		if !f.haveCursor {
			return timescale.Cursor{}, timescale.ErrNotFound
		}
		return timescale.Cursor{Source: source, LastLedger: f.projectorCursor}, nil
	default:
		return timescale.Cursor{}, timescale.ErrNotFound
	}
}

func (f *fakeStore) UpsertCursor(_ context.Context, _, _ string, lastLedger uint32) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.upserts++
	if lastLedger > f.projectorCursor {
		f.projectorCursor = lastLedger
	}
	f.haveCursor = true
	return nil
}

func (f *fakeStore) StreamSorobanEvents(_ context.Context, from, to uint32,
	_, _, _ []string, fn func(row sorobanevents.Row) error,
) error {
	f.mu.Lock()
	rows := append([]sorobanevents.Row(nil), f.rows...)
	f.mu.Unlock()
	for _, r := range rows {
		if r.Ledger < from || r.Ledger > to {
			continue
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return nil
}

// FirstSorobanEventLedger mirrors StreamSorobanEvents' row set (filters
// ignored, as there): the lowest row ledger in [from, to].
func (f *fakeStore) FirstSorobanEventLedger(_ context.Context, from, to uint32,
	_, _, _ []string,
) (uint32, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var first uint32
	found := false
	for _, r := range f.rows {
		if r.Ledger >= from && r.Ledger <= to && (!found || r.Ledger < first) {
			first, found = r.Ledger, true
		}
	}
	return first, found, nil
}

func (f *fakeStore) cursor() uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.projectorCursor
}

// fakeEvents is an in-memory [eventSource]: StreamEvents and FirstEventLedger serve events in [from, to] passing the
// contract-id prefilter, as the lake reads do (topic filters ignored); OpenLake returns lake, or fails when nil.
type fakeEvents struct {
	mu  sync.Mutex
	evs []events.Event
	// filters records each StreamEvents call's contract-id prefilter.
	filters [][]string
	// seeks counts FirstEventLedger calls; seekErr fails them.
	seeks   int
	seekErr error
	// streamErr fails StreamEvents.
	streamErr error
	lake      *fakeLake
}

func (f *fakeEvents) StreamEvents(_ context.Context, from, to uint32, contractIDs, _, _ []string,
	_ bool, fn func(events.Event) error,
) error {
	f.mu.Lock()
	f.filters = append(f.filters, slices.Clone(contractIDs))
	evs := f.matching(from, to, contractIDs)
	streamErr := f.streamErr
	f.mu.Unlock()
	if streamErr != nil {
		return streamErr
	}
	for _, ev := range evs {
		if err := fn(ev); err != nil {
			return err
		}
	}
	return nil
}

func (f *fakeEvents) FirstEventLedger(_ context.Context, from, to uint32, contractIDs, _, _ []string) (uint32, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seeks++
	if f.seekErr != nil {
		return 0, false, f.seekErr
	}
	var first uint32
	found := false
	for _, ev := range f.matching(from, to, contractIDs) {
		if !found || ev.Ledger < first {
			first, found = ev.Ledger, true
		}
	}
	return first, found, nil
}

func (f *fakeEvents) OpenLake(context.Context) (lakeReader, error) {
	if f.lake == nil {
		return nil, errors.New("fakeEvents: no lake")
	}
	return f.lake, nil
}

// matching must be called with f.mu held.
func (f *fakeEvents) matching(from, to uint32, contractIDs []string) []events.Event {
	var out []events.Event
	for _, ev := range f.evs {
		if ev.Ledger < from || ev.Ledger > to {
			continue
		}
		if len(contractIDs) > 0 && !slices.Contains(contractIDs, ev.ContractID) {
			continue
		}
		out = append(out, ev)
	}
	return out
}

func (f *fakeEvents) add(evs ...events.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.evs = append(f.evs, evs...)
}

// lakeEvent builds a contract event keyed by (ledger, tag): tag sets the tx
// hash's first byte and the event index, so two tags never collide.
func lakeEvent(ledger uint32, tag byte) events.Event {
	txHash := make([]byte, 32)
	txHash[0] = tag
	txHash[1] = byte(ledger)
	return events.Event{
		Type:           "contract",
		Ledger:         ledger,
		LedgerClosedAt: time.Unix(int64(ledger), 0).UTC().Format(time.RFC3339),
		ContractID:     "CTEST0000000000000000000000000000000000000000000000000000",
		TxHash:         hex.EncodeToString(txHash),
		EventIndex:     int(tag),
		Topic:          []string{base64.StdEncoding.EncodeToString([]byte{0x00, 0x00, 0x00, 0x0f})},
		Value:          base64.StdEncoding.EncodeToString([]byte{0x00}),
	}
}

// lakeRow builds a soroban_events row that Reconstruct accepts: non-empty
// contract id, 32-byte tx hash, non-empty topic[0] XDR.
func lakeRow(ledger uint32, tag byte) sorobanevents.Row {
	txHash := make([]byte, 32)
	txHash[0] = tag
	txHash[1] = byte(ledger)
	return sorobanevents.Row{
		Ledger:          ledger,
		LedgerCloseTime: time.Unix(int64(ledger), 0).UTC(),
		TxHash:          txHash,
		OpIndex:         0,
		EventIndex:      int16(tag),
		ContractID:      "CTEST0000000000000000000000000000000000000000000000000000",
		TopicCount:      1,
		Topic0Sym:       "transfer",
		Topic0XDR:       []byte{0x00, 0x00, 0x00, 0x0f},
		BodyXDR:         []byte{0x00},
	}
}

// openLake is a lake whose watermark never clamps the ledgerstream tip.
func openLake() *fakeLake { return &fakeLake{wm: math.MaxUint32} }

// newLakeEventsProjector is a projector in CH feed-switch mode over store and
// the in-memory lake source evs, plus that source's lake connection.
func newLakeEventsProjector(store eventStore, evs *fakeEvents, sink SinkFunc) (*Projector, *sourceLake) {
	if evs.lake == nil {
		evs.lake = openLake()
	}
	p := &Projector{store: store, lakeEvents: evs, logger: discardLog(), sink: sink}
	return p, p.newSourceLake()
}

// wedgeHarness wires a projector in CH feed-switch mode over the fake store
// and an in-memory lake, with a sink whose error is chosen per event ledger.
type wedgeHarness struct {
	store   *fakeStore
	events  *fakeEvents
	proj    *Projector
	src     Source
	window  uint32
	tracker poisonTracker
	wedge   wedgeTracker
	// lake is the source's CH lake connection; sorobanEventsMode clears it.
	lake *sourceLake
}

func newWedgeHarness(t *testing.T, name string, evs []events.Event, tip uint32, sink func(ev consumer.Event) error) *wedgeHarness {
	t.Helper()
	store := &fakeStore{
		projectorCursor: 100,
		haveCursor:      true,
		tipLedger:       tip,
	}
	fe := &fakeEvents{evs: evs}
	p, lake := newLakeEventsProjector(store, fe, func(_ context.Context, ev consumer.Event) error { return sink(ev) })
	return &wedgeHarness{
		store:  store,
		events: fe,
		proj:   p,
		src:    Source{Name: name, Decoder: &ledgerEchoDecoder{}},
		window: BatchLimit,
		lake:   lake,
	}
}

// sorobanEventsMode switches the harness to the legacy soroban_events read
// over rows, for the tests that pin that path.
func (h *wedgeHarness) sorobanEventsMode(rows ...sorobanevents.Row) {
	h.proj.lakeEvents = nil
	h.lake = nil
	h.store.rows = rows
}

func (h *wedgeHarness) cycle() {
	h.cycleCtx(context.Background())
}

func (h *wedgeHarness) cycleCtx(ctx context.Context) {
	h.proj.cycleOneSource(ctx, h.src, &h.window, &h.tracker, &h.wedge, h.lake)
}

// ledgerEchoDecoder matches every row and emits one consumer.Event carrying
// the source row's ledger, so the sink can fail a chosen row.
type ledgerEchoDecoder struct{}

func (*ledgerEchoDecoder) Name() string              { return "ledger-echo" }
func (*ledgerEchoDecoder) Matches(events.Event) bool { return true }
func (*ledgerEchoDecoder) Decode(ev events.Event) ([]consumer.Event, error) {
	return []consumer.Event{ledgerEvent{ledger: ev.Ledger}}, nil
}

type ledgerEvent struct{ ledger uint32 }

func (ledgerEvent) EventKind() string { return "ledger.echo" }
func (ledgerEvent) Source() string    { return "ledger-echo" }

func decodedCount(t *testing.T, source, outcome string) float64 {
	t.Helper()
	return testutil.ToFloat64(obs.ProjectorEventsDecoded.WithLabelValues(source, outcome))
}

func runsCount(t *testing.T, source, outcome string) float64 {
	t.Helper()
	return testutil.ToFloat64(obs.ProjectorRunsTotal.WithLabelValues(source, outcome))
}

// decodeErrDecoder matches every row and returns a decode error for all of them: a shipped decoder regression breaking a
// whole class of events (the phoenix 5,161-orphaned-swap shape), unlike an odd scattered poison row.
type decodeErrDecoder struct{}

func (*decodeErrDecoder) Name() string              { return "decode-err" }
func (*decodeErrDecoder) Matches(events.Event) bool { return true }
func (*decodeErrDecoder) Decode(events.Event) ([]consumer.Event, error) {
	return nil, errors.New("field-mapping regression: cannot decode 7-field swap")
}

// A Validate-failing row (e.g. OracleUpdate rejected by canonical validation, no *pgconn.PgError) is a deterministic
// data fault: skipped and counted on cycle one. The cursor advances and later cycles progress; ledger 102 commits beside
// it as the sink-health proof (a lone poison row is the holding global-fault shape).
func TestCycle_ValidationErrorDoesNotWedge(t *testing.T) {
	const source = "cor11-validation"
	rows := []events.Event{lakeEvent(101, 1), lakeEvent(102, 2)}
	before := decodedCount(t, source, "sink_permanent")

	h := newWedgeHarness(t, source, rows, 105, func(ev consumer.Event) error {
		if ev.(ledgerEvent).ledger == 101 {
			return fmt.Errorf("%w: price must be positive, got 0", canonical.ErrInvalidOracle)
		}
		return nil
	})

	h.cycle()
	if got := h.store.cursor(); got != 105 {
		t.Fatalf("cycle 1: cursor = %d, want 105 (advance past a deterministic validation failure)", got)
	}
	if got := decodedCount(t, source, "sink_permanent") - before; got != 1 {
		t.Errorf("sink_permanent counter delta = %v, want 1 (the skipped row must be counted)", got)
	}

	h.store.mu.Lock()
	h.store.tipLedger = 205
	h.store.mu.Unlock()
	h.events.add(lakeEvent(150, 3))

	h.cycle()
	if got := h.store.cursor(); got != 205 {
		t.Fatalf("cycle 2: cursor = %d, want 205 (a poisoned range must not re-wedge the source)", got)
	}
}

// A negative SEP-41 amount rejected by the store's pre-SQL validation (plain fmt.Errorf, no sentinel, no PgError) must
// not wedge the transfers projection forever.

// The store rejects a malformed negative transfer amount before the statement runs, so the projector can't classify the
// error. It is retried under a budget, then quarantined, so the sole-writer transfers projection resumes.
func TestCycle_NegativeSEP41AmountQuarantinesAfterBudget(t *testing.T) {
	const source = "cor01-negative-amount"
	// Ledger 101 is poison; 102 commits fine — that success is the sink-health
	// proof that lets the short budget apply.
	rows := []events.Event{lakeEvent(101, 1), lakeEvent(102, 2)}
	beforeQuarantined := decodedCount(t, source, "sink_quarantined")

	h := newWedgeHarness(t, source, rows, 105, func(ev consumer.Event) error {
		if ev.(ledgerEvent).ledger == 101 {
			return errors.New("timescale: InsertSEP41TransferBatch: row 0 transfer negative Amount -1")
		}
		return nil
	})

	// While the budget is unspent the cursor MUST hold below the failing
	// ledger — a genuinely transient fault deserves its retries, and dropping
	// the row immediately would be the silent-loss bug.
	for i := 1; i < QuarantineAfterCycles; i++ {
		h.cycle()
		if got := h.store.cursor(); got != 100 {
			t.Fatalf("cycle %d: cursor = %d, want 100 (hold for retry while the budget lasts)", i, got)
		}
	}

	// Budget exhausted: give up on the row, advance past it.
	h.cycle()
	if got := h.store.cursor(); got != 105 {
		t.Fatalf("cursor = %d after %d cycles, want 105 (an un-processable row must not wedge a sole-writer source forever)", got, QuarantineAfterCycles)
	}
	if got := decodedCount(t, source, "sink_quarantined") - beforeQuarantined; got != 1 {
		t.Errorf("sink_quarantined counter delta = %v, want 1 (a skipped row must be counted, not silently dropped)", got)
	}
}

// The other half of the fix: do not over-correct into dropping recoverable work.

// TestCycle_HoldsCursorAndNeverQuarantines pins the anti-over-correction
// property: an infrastructure outage, or a global sink failure where nothing
// commits (no health proof), must hold the cursor and never shed rows.
func TestCycle_HoldsCursorAndNeverQuarantines(t *testing.T) {
	tests := []struct {
		name      string
		source    string
		rows      []events.Event
		cycles    int
		sinkErr   error
		perCycle  bool
		wantRetry bool
	}{
		{
			name:      "infra outage retries forever",
			source:    "infra-retry-forever",
			rows:      []events.Event{lakeEvent(101, 1), lakeEvent(102, 2)},
			cycles:    QuarantineAfterCycles * 3,
			sinkErr:   errors.New("dial tcp 127.0.0.1:5432: connect: connection refused"),
			perCycle:  true,
			wantRetry: true,
		},
		{
			name:    "global failure stalls visibly",
			source:  "global-failure-stalls",
			rows:    []events.Event{lakeEvent(101, 1), lakeEvent(102, 2), lakeEvent(103, 3)},
			cycles:  QuarantineAfterCycles * 2,
			sinkErr: &pgconn.PgError{Code: "42703", Message: `column "amount" does not exist`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			beforeQuarantined := decodedCount(t, tt.source, "sink_quarantined")
			h := newWedgeHarness(t, tt.source, tt.rows, 105, func(consumer.Event) error { return tt.sinkErr })

			for i := 0; i < tt.cycles; i++ {
				h.cycle()
				if tt.perCycle {
					if got := h.store.cursor(); got != 100 {
						t.Fatalf("cycle %d: cursor = %d, want 100", i, got)
					}
				}
			}
			if got := h.store.cursor(); got != 100 {
				t.Fatalf("cursor = %d, want 100 (must stall visibly, never shed rows)", got)
			}
			if got := decodedCount(t, tt.source, "sink_quarantined") - beforeQuarantined; got != 0 {
				t.Errorf("sink_quarantined delta = %v, want 0", got)
			}
			if tt.wantRetry && decodedCount(t, tt.source, "sink_retry") == 0 {
				t.Error("sink_retry counter did not move; a held row must be visible as a retry")
			}
		})
	}
}

// TestCycle_DeadlockRetriesBeforeQuarantine pins that a classic retryable
// Postgres fault still gets its retries: a deadlock resolves on a later cycle
// and the row commits — nothing is skipped.
func TestCycle_DeadlockRetriesBeforeQuarantine(t *testing.T) {
	const source = "deadlock-retry"
	rows := []events.Event{lakeEvent(101, 1), lakeEvent(102, 2)}
	beforeQuarantined := decodedCount(t, source, "sink_quarantined")

	beforeOK := decodedCount(t, source, "ok")
	failing := true
	h := newWedgeHarness(t, source, rows, 105, func(ev consumer.Event) error {
		if failing && ev.(ledgerEvent).ledger == 101 {
			return &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}
		}
		return nil
	})

	for i := 0; i < 3; i++ {
		h.cycle()
		if got := h.store.cursor(); got != 100 {
			t.Fatalf("cycle %d: cursor = %d, want 100 (a deadlock must be retried, not skipped)", i, got)
		}
	}
	failing = false // the retry wins
	h.cycle()
	if got := h.store.cursor(); got != 105 {
		t.Fatalf("cursor = %d, want 105 after the deadlock cleared", got)
	}
	if got := decodedCount(t, source, "sink_quarantined") - beforeQuarantined; got != 0 {
		t.Errorf("sink_quarantined delta = %v, want 0 (a recovered deadlock must not have been dropped)", got)
	}
	// "ok" only counts events from a cycle that committed cursor progress
	// so the held cycles contribute nothing and the recovering cycle
	// projects both rows exactly once.
	if got := decodedCount(t, source, "ok") - beforeOK; got != 2 {
		t.Errorf("ok delta = %v, want 2 (both rows project once the deadlock clears)", got)
	}
}

// Sink-side adaptive shrink, cycle-level half of the shrinkWindow unit test: a window whose CH scan finishes but whose
// sink writes exhaust PerSourceTimeout ends the cycle with a dead cycleCtx and held transient rows; the window pointer
// must still shrink, or the identical dense range is retried forever.

// Drives cycleOneSource under an already-expired budget with a sink fast-failing every write with DeadlineExceeded. Each
// cycle must halve the adaptive window, flooring at MinBatchLimit, while the cursor holds (dispositionRetry: never a
// skip, quarantine or advance-past-loss).
func TestCycle_SinkBudgetExhaustionShrinksWindowAndHoldsCursor(t *testing.T) {
	const source = "sink-budget-shrink"
	rows := []events.Event{lakeEvent(101, 1), lakeEvent(102, 2)}
	beforeQuarantined := decodedCount(t, source, "sink_quarantined")

	h := newWedgeHarness(t, source, rows, 2000, func(consumer.Event) error {
		// Every write fast-fails on the spent cycle budget, the shape the
		// sink sees once cycleCtx is dead mid-batch.
		return context.DeadlineExceeded
	})

	// The parent ctx is already expired, so cycleCtx is born expired. The fake store ignores
	// ctx, so only the sink "spends" the budget, isolating the sink-side shrink arm from the
	// stream-side one (which needs the stream itself to return DeadlineExceeded).
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	want := uint32(BatchLimit)
	for i := 0; i < 10; i++ {
		h.cycleCtx(expired)
		if next := want / 2; next >= MinBatchLimit {
			want = next
		} else if want > MinBatchLimit {
			want = MinBatchLimit
		}
		if h.window != want {
			t.Fatalf("cycle %d: window = %d, want %d (budget-exhausted sink writes must halve the window toward the floor)", i+1, h.window, want)
		}
		if got := h.store.cursor(); got != 100 {
			t.Fatalf("cycle %d: cursor = %d, want 100 (a deadline fault must hold the cursor, never advance past held rows)", i+1, got)
		}
	}
	if h.window != MinBatchLimit {
		t.Fatalf("window = %d after repeated exhausted cycles, want the MinBatchLimit floor %d", h.window, MinBatchLimit)
	}
	if got := decodedCount(t, source, "sink_quarantined") - beforeQuarantined; got != 0 {
		t.Errorf("sink_quarantined delta = %v, want 0 (deadline faults are dispositionRetry — never quarantined)", got)
	}

	// Once the dense stretch clears (healthy budget, healthy sink) the cycle
	// commits and the window recovers by doubling — the retry converged
	// instead of wedging, which is the whole point of the shrink.
	h.proj.sink = func(context.Context, consumer.Event) error { return nil }
	h.cycle()
	if got := h.store.cursor(); got != 101+MinBatchLimit {
		t.Fatalf("recovery cycle: cursor = %d, want %d (fromLedger 101 + the floored window)", got, 101+MinBatchLimit)
	}
	if h.window != 2*MinBatchLimit {
		t.Fatalf("recovery cycle: window = %d, want %d (success doubles back toward BatchLimit)", h.window, 2*MinBatchLimit)
	}
}

// When a decoder regression fails a class of valid events, the cursor still advances (holding would re-wedge the sole-
// writer source) but the cycle is marked runs_total{outcome="decode_degraded"}, never "ok", so the per-source
// decode_error rate alert can tell a regression from scattered poison rows.
func TestCycle_DecoderRegressionMarksRunDegradedNotOK(t *testing.T) {
	const source = "data6-decode-regression"
	evs := &fakeEvents{evs: []events.Event{lakeEvent(101, 1), lakeEvent(102, 2)}}

	beforeOK := runsCount(t, source, "ok")
	beforeDegraded := runsCount(t, source, "decode_degraded")
	beforeDecodeErr := decodedCount(t, source, "decode_error")

	store := &fakeStore{projectorCursor: 100, haveCursor: true, tipLedger: 105}
	p, lake := newLakeEventsProjector(store, evs, func(context.Context, consumer.Event) error { return nil })
	src := Source{Name: source, Decoder: &decodeErrDecoder{}}
	window := uint32(BatchLimit)
	var tracker poisonTracker
	var wedge wedgeTracker

	p.cycleOneSource(context.Background(), src, &window, &tracker, &wedge, lake)

	// The cursor still advances past the broken class (poison-row escape —
	// do NOT re-wedge a sole-writer source on a deterministic fault).
	if got := store.cursor(); got != 105 {
		t.Fatalf("cursor = %d, want 105 (a deterministic decode failure is skipped, not held)", got)
	}
	// The dropped rows are counted for visibility.
	if got := decodedCount(t, source, "decode_error") - beforeDecodeErr; got != 2 {
		t.Fatalf("decode_error delta = %v, want 2 (both broken rows counted)", got)
	}
	// THE FIX: the cycle must NOT be reported as a clean "ok" run...
	if got := runsCount(t, source, "ok") - beforeOK; got != 0 {
		t.Errorf("runs_total{outcome=ok} delta = %v, want 0 (a decode-dropping cycle must not be reported clean)", got)
	}
	// ...it is surfaced as "decode_degraded" so the silent drop is visible.
	if got := runsCount(t, source, "decode_degraded") - beforeDegraded; got != 1 {
		t.Errorf("runs_total{outcome=decode_degraded} delta = %v, want 1 (the dropped-rows cycle must surface as degraded)", got)
	}
}

func wedgeGauge(t *testing.T, source string) float64 {
	t.Helper()
	return testutil.ToFloat64(obs.ProjectorWedged.WithLabelValues(source))
}

// TestCycle_FlooredDeadlineStallSetsAndClearsWedgeGauge pins that a source at the
// MinBatchLimit window floor that keeps blowing the per-cycle deadline without committing
// progress (a dense + compressed chunk) is a SILENT cursor wedge: it retries the identical
// range forever and runs_total{outcome="error"} is easily masked by other sources. After
// WedgeCycles consecutive floor-stalls stellarindex_projector_wedged must flip to 1, and a
// single advancing cycle must clear it to 0.
func TestCycle_FlooredDeadlineStallSetsAndClearsWedgeGauge(t *testing.T) {
	const source = "wedge-floor-stall"
	rows := []events.Event{lakeEvent(101, 1), lakeEvent(102, 2)}

	// Every write fast-fails with the deadline the projector sees once cycleCtx
	// is spent mid-batch; the parent context below is born expired so
	// cycleCtx.Err() != nil every cycle (the sink-budget wedge shape).
	h := newWedgeHarness(t, source, rows, 2000, func(consumer.Event) error {
		return context.DeadlineExceeded
	})
	// Start already AT the floor so each failing cycle is a floor-stall — this
	// isolates the wedge threshold from the (separately-tested) shrink ramp.
	h.window = MinBatchLimit
	obs.ProjectorWedged.WithLabelValues(source).Set(0) // explicit healthy baseline

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	// Below the budget: NOT yet wedged. This is the non-vacuous half — the
	// gauge must read a real 0 for the first WedgeCycles-1 floor-stalls, or the
	// alert would fire on a single slow cycle instead of a genuine wedge.
	for i := 1; i < WedgeCycles; i++ {
		h.cycleCtx(expired)
		if got := h.store.cursor(); got != 100 {
			t.Fatalf("cycle %d: cursor = %d, want 100 (a floored deadline holds the cursor, never advances)", i, got)
		}
		if got := wedgeGauge(t, source); got != 0 {
			t.Fatalf("cycle %d: wedged = %v, want 0 (must not flag before %d consecutive floor-stalls)", i, got, WedgeCycles)
		}
	}

	// The WedgeCycles-th consecutive floor-stall flips the flag.
	h.cycleCtx(expired)
	if got := wedgeGauge(t, source); got != 1 {
		t.Fatalf("wedged = %v after %d consecutive floor-stalls, want 1 (a stuck cursor must be observable)", got, WedgeCycles)
	}

	// A single healthy cycle (budget + sink both recovered) commits forward
	// progress and must clear the wedge.
	h.proj.sink = func(context.Context, consumer.Event) error { return nil }
	h.cycle()
	if got := h.store.cursor(); got == 100 {
		t.Fatal("recovery cycle did not advance the cursor (still 100) — test harness broken")
	}
	if got := wedgeGauge(t, source); got != 0 {
		t.Fatalf("wedged = %v after a healthy advancing cycle, want 0 (any advance clears the wedge)", got)
	}
}

// TestCycle_FloorStallEscalatesCycleBudget: at the floor the window cannot
// shrink, so each consecutive floor-stall must lengthen the next cycle's
// deadline (capped), and an advancing cycle must reset it.
func TestCycle_FloorStallEscalatesCycleBudget(t *testing.T) {
	const source = "wedge-budget-escalation"
	rows := []events.Event{lakeEvent(101, 1), lakeEvent(102, 2)}

	var remaining time.Duration
	h := newWedgeHarness(t, source, rows, 2000, func(consumer.Event) error { return context.DeadlineExceeded })
	h.window = MinBatchLimit
	h.proj.sink = func(ctx context.Context, _ consumer.Event) error {
		if d, ok := ctx.Deadline(); ok {
			remaining = time.Until(d)
		}
		return nil
	}

	var wt wedgeTracker
	if got := wt.budget(); got != PerSourceTimeout {
		t.Fatalf("fresh budget = %v, want %v", got, PerSourceTimeout)
	}
	for i := 0; i < 10; i++ {
		wt.floorStall(source)
	}
	if got, want := wt.budget(), PerSourceTimeout*MaxCycleBudgetMultiple; got != want {
		t.Fatalf("capped budget = %v, want %v", got, want)
	}

	h.wedge = wedgeTracker{floorStalls: 2}
	h.cycle()
	if remaining <= 2*PerSourceTimeout || remaining > 4*PerSourceTimeout {
		t.Fatalf("cycle deadline = %v, want in (%v, %v] after 2 floor-stalls", remaining, 2*PerSourceTimeout, 4*PerSourceTimeout)
	}
	if got := h.wedge.budget(); got != PerSourceTimeout {
		t.Fatalf("budget after an advancing cycle = %v, want %v", got, PerSourceTimeout)
	}
}

// Anti-false-positive half: a source that keeps blowing the deadline but is still shrinking (window above the floor) is
// adapting, not wedged, so the flag stays 0 until the window bottoms out. Avoids paging on the healthy shrink ramp.
func TestCycle_NonFlooredDeadlineDoesNotWedge(t *testing.T) {
	const source = "wedge-still-shrinking"
	rows := []events.Event{lakeEvent(101, 1), lakeEvent(102, 2)}

	h := newWedgeHarness(t, source, rows, 2000, func(consumer.Event) error {
		return context.DeadlineExceeded
	})
	// Window well above the floor; it will halve each cycle but not reach 25
	// within the loop below (100 -> 50 -> 25 takes two shrinks).
	h.window = 100
	obs.ProjectorWedged.WithLabelValues(source).Set(0)

	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()

	// One cycle: 100 -> 50, still above the floor, so no floor-stall even at
	// WedgeCycles+ repetitions of an above-floor deadline.
	h.cycleCtx(expired)
	if h.window != 50 {
		t.Fatalf("window = %d, want 50 (deadline halves the window)", h.window)
	}
	if got := wedgeGauge(t, source); got != 0 {
		t.Fatalf("wedged = %v, want 0 (a still-shrinking source is adapting, not wedged)", got)
	}
}

// TestCycle_HealthyRowsUnaffected is the no-regression baseline: with a sink
// that never fails, the cursor advances to the window end and everything
// counts as ok.
func TestCycle_HealthyRowsUnaffected(t *testing.T) {
	const source = "healthy-baseline"
	rows := []events.Event{lakeEvent(101, 1), lakeEvent(102, 2)}
	before := decodedCount(t, source, "ok")

	h := newWedgeHarness(t, source, rows, 104, func(consumer.Event) error { return nil })
	h.cycle()

	if got := h.store.cursor(); got != 104 {
		t.Fatalf("cursor = %d, want 104", got)
	}
	if got := decodedCount(t, source, "ok") - before; got != 2 {
		t.Errorf("ok delta = %v, want 2", got)
	}
}

// Entry-point duplicate guard: the lake is an append log read without FINAL, so re-ingested duplicates arrive
// consecutively. Stateless decoders and keyed sinks absorb them, but buffered decoders (phoenix) corrupt: a duplicate
// re-opens a completed group and cross-assigns fields between legs. Exact-identity re-deliveries must decode once.
func TestCycle_AdjacentDuplicateRowsDecodeOnce(t *testing.T) {
	// Three copies of one row followed by a distinct second row.
	dup := lakeEvent(101, 1)
	rows := []events.Event{dup, dup, dup, lakeEvent(102, 2)}

	var emitted int
	h := newWedgeHarness(t, "dup-test", rows, 200, func(consumer.Event) error {
		emitted++
		return nil
	})
	h.cycle()

	if emitted != 2 {
		t.Fatalf("sink saw %d events, want 2 (three duplicate copies must decode once; the distinct row once)", emitted)
	}
}

// The i128 overflow outcome must PARTITION from sink_permanent (counted once,
// never under both) so a sum across outcomes does not double-count the row, and
// an ordinary class-23502 drop must never reach the SEV-1 overflow child.
func TestCycle_I128OverflowOutcomePartitionsFromSinkPermanent(t *testing.T) {
	tests := []struct {
		name         string
		source       string
		poisonErr    error
		wantOverflow float64
		wantPerm     float64
	}{
		{
			name:   "i128 overflow gets its own outcome",
			source: "rlt131-i128-overflow",
			poisonErr: fmt.Errorf("%w: trade amount %s exceeds 128 bits", canonical.ErrI128Overflow,
				"340282366920938463463374607431768211456"),
			wantOverflow: 1,
			wantPerm:     0,
		},
		{
			name:         "ordinary poison row is not an overflow",
			source:       "rlt131-i128-negative-control",
			poisonErr:    notNullViolation(),
			wantOverflow: 0,
			wantPerm:     1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Ledger 102 commits: the sink-health proof, so the poison row is shed on cycle one.
			rows := []events.Event{lakeEvent(101, 1), lakeEvent(102, 2)}
			i128Before := decodedCount(t, tt.source, "sink_i128_overflow")
			permBefore := decodedCount(t, tt.source, "sink_permanent")
			okBefore := decodedCount(t, tt.source, "ok")

			h := newWedgeHarness(t, tt.source, rows, 105, func(ev consumer.Event) error {
				if ev.(ledgerEvent).ledger == 101 {
					return tt.poisonErr
				}
				return nil
			})
			h.cycle()

			if got := decodedCount(t, tt.source, "sink_i128_overflow") - i128Before; got != tt.wantOverflow {
				t.Errorf("outcome=sink_i128_overflow delta = %v, want %v", got, tt.wantOverflow)
			}
			if got := decodedCount(t, tt.source, "sink_permanent") - permBefore; got != tt.wantPerm {
				t.Errorf("outcome=sink_permanent delta = %v, want %v", got, tt.wantPerm)
			}
			if got := decodedCount(t, tt.source, "ok") - okBefore; got != 1 {
				t.Errorf("outcome=ok delta = %v, want 1 (only ledger 102 durably committed)", got)
			}
			if got := h.store.cursor(); got != 105 {
				t.Fatalf("cursor = %d, want 105 (a deterministic drop must not wedge the source)", got)
			}
		})
	}
}

// TestCycle_ReplayRewindMidCycleIsNotClobbered: a projector-replay rewind landing mid-cycle must not be undone. The
// commit derives from the start-of-cycle cursor, so it must not reset to tip; the next cycle re-walks the rewound range.
func TestCycle_ReplayRewindMidCycleIsNotClobbered(t *testing.T) {
	const (
		rewoundLedger  = uint32(60)  // below the cursor: already projected, to be re-driven
		inFlightLedger = uint32(110) // what cycle 1 is sinking when the rewind lands
		tip            = uint32(120)
		rewindTo       = uint32(49)
	)
	var (
		mu     sync.Mutex
		sunk   []uint32
		rewind func()
	)
	h := newWedgeHarness(t, "cas", []events.Event{lakeEvent(rewoundLedger, 1), lakeEvent(inFlightLedger, 2)}, tip,
		func(ev consumer.Event) error {
			mu.Lock()
			sunk = append(sunk, ev.(ledgerEvent).ledger)
			r := rewind
			rewind = nil // once
			mu.Unlock()
			if r != nil {
				r()
			}
			return nil
		})
	rewind = func() { h.store.rewind(t, rewindTo) }

	// Cycle 1 — reads cursor=100, sinks ledger 110, rewind lands, commit.
	h.cycle()
	if got := h.store.cursor(); got != rewindTo {
		t.Fatalf("after the in-flight cycle the cursor = %d, want the replay's rewind point %d — the cycle's stale commit clobbered the rewind (F159)", got, rewindTo)
	}
	mu.Lock()
	if len(sunk) != 1 || sunk[0] != inFlightLedger {
		t.Fatalf("cycle 1 sunk %v, want exactly [%d] — the interleave did not arm", sunk, inFlightLedger)
	}
	sunk = nil
	mu.Unlock()

	// Cycle 2 — re-reads the rewound cursor and re-walks [50, 120].
	h.cycle()
	mu.Lock()
	defer mu.Unlock()
	if len(sunk) != 2 || sunk[0] != rewoundLedger || sunk[1] != inFlightLedger {
		t.Fatalf("cycle 2 sunk %v, want [%d %d] — the rewound range was not re-projected", sunk, rewoundLedger, inFlightLedger)
	}
	if got := h.store.cursor(); got != tip {
		t.Fatalf("after the re-walk the cursor = %d, want tip %d", got, tip)
	}
}

// TestCycle_FirstCycleSeedDoesNotOverwriteARowThatAppeared covers the
// not-found arm: a cycle that read "no cursor" must not install its commit
// over a row someone else created meanwhile.
func TestCycle_FirstCycleSeedDoesNotOverwriteARowThatAppeared(t *testing.T) {
	var appear func()
	h := newWedgeHarness(t, "cas-seed", []events.Event{lakeEvent(110, 1)}, 120,
		func(consumer.Event) error {
			if appear != nil {
				appear()
				appear = nil
			}
			return nil
		})
	h.store.haveCursor, h.store.projectorCursor = false, 0
	appear = func() {
		h.store.mu.Lock()
		h.store.haveCursor, h.store.projectorCursor = true, 30
		h.store.mu.Unlock()
	}
	h.cycle()
	if got := h.store.cursor(); got != 30 {
		t.Fatalf("cursor = %d, want 30 — a not-found read must seed with DO NOTHING semantics, not overwrite", got)
	}

	// And the plain seed still works when nothing races it.
	h2 := newWedgeHarness(t, "cas-seed-plain", []events.Event{lakeEvent(110, 1)}, 120,
		func(consumer.Event) error { return nil })
	h2.store.haveCursor, h2.store.projectorCursor = false, 0
	h2.window = 200 // [0, 120] in one cycle
	h2.cycle()
	if got := h2.store.cursor(); got != 120 {
		t.Fatalf("plain first-cycle seed: cursor = %d, want 120", got)
	}
}

// TestCycle_SpentBudgetStillCommitsDurableProgress: when a sink commits ledger 101 and then exhausts PerSourceTimeout,
// the cursor must still advance to 101; the cursor write must not use the expired cycle context, or work is re-projected
// forever.
func TestCycle_SpentBudgetStillCommitsDurableProgress(t *testing.T) {
	const source = "t066-spent-budget-commit"
	rows := []events.Event{lakeEvent(101, 1), lakeEvent(102, 2)}
	h := newWedgeHarness(t, source, rows, 2000, func(ev consumer.Event) error {
		if ev.(ledgerEvent).ledger == 101 {
			return nil // landed before the budget ran out
		}
		return context.DeadlineExceeded
	})
	h.proj.store = ctxStore{h.store}

	// A parent already past its deadline gives a cycleCtx born expired; the
	// fake stream ignores ctx, so the scan completes and only the sink and
	// the cursor write can observe the spent budget.
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	h.cycleCtx(expired)

	if got := h.store.cursor(); got != 101 {
		t.Fatalf("cursor = %d, want 101 (ledger 101 committed; a spent cycle budget must not discard it)", got)
	}
	if want := uint32(BatchLimit / 2); h.window != want {
		t.Fatalf("window = %d, want %d (a budget-exhausted cycle keeps its sink-side shrink even when it commits)", h.window, want)
	}
}

// The failing output belongs to ledger 101 though it was scanned at ledger
// 103: the cursor must stay below 101, not advance to 102.
func TestCycle_CarriedOutputFaultHoldsCursorBelowOutputLedger(t *testing.T) {
	h := carryHarness(t, "carry-hold")
	h.cycle()
	if got := h.store.cursor(); got > 100 {
		t.Fatalf("cursor = %d, want <= 100 (hold below the carried output's ledger 101)", got)
	}
}

// Quarantining a carried-output row must forget the history the tracker
// recorded for that row, or a later identical row inherits a spent budget.
func TestCycle_CarriedOutputQuarantineForgetsTrackerHistory(t *testing.T) {
	const source = "carry-quarantine"
	h := carryHarness(t, source)
	before := decodedCount(t, source, "sink_quarantined")
	for i := 0; i < QuarantineAfterCycles; i++ {
		h.cycle()
	}
	if got := decodedCount(t, source, "sink_quarantined") - before; got != 1 {
		t.Fatalf("sink_quarantined delta = %v, want 1", got)
	}
	if n := len(h.tracker.fails); n != 0 {
		t.Errorf("tracker still holds %d entries after quarantine: %v", n, h.tracker.fails)
	}
	if got := h.store.cursor(); got != 105 {
		t.Errorf("cursor = %d, want 105 after quarantine", got)
	}
}

// TestCycle_HeldRowWarningIsThrottled: the per-row warning for a persistently held row fires on the first failing cycle
// and every heldRowLogEvery-th after, not every cycle, so it cannot bury the ERROR lines an operator needs.
func TestCycle_HeldRowWarningIsThrottled(t *testing.T) {
	const source = "rlt142-held-row-log"
	rows := []events.Event{lakeEvent(101, 1), lakeEvent(102, 2)}

	h := newWedgeHarness(t, source, rows, 105, func(ev consumer.Event) error {
		if ev.(ledgerEvent).ledger == 101 {
			return errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
		}
		return nil
	})
	var logs bytes.Buffer
	h.proj.logger = slog.New(slog.NewTextHandler(&logs, nil))

	const cycles = 2*heldRowLogEvery + 1
	for i := 0; i < cycles; i++ {
		h.cycle()
	}
	if got := h.store.cursor(); got != 100 {
		t.Fatalf("cursor = %d, want 100 (an infra fault is held for retry, never shed)", got)
	}
	// Cycles 1, heldRowLogEvery and 2*heldRowLogEvery.
	const want = 3
	if got := strings.Count(logs.String(), heldRowWarning); got != want {
		t.Errorf("held-row warnings over %d cycles = %d, want %d (first cycle, then every %d)", cycles, got, want, heldRowLogEvery)
	}
	if !strings.Contains(logs.String(), "consecutive_cycles=40") {
		t.Errorf("the throttled line must still carry the running consecutive_cycles count; logs:\n%s", logs.String())
	}
}

func TestCycle_WatermarkErrorPublishesLag(t *testing.T) {
	const src = "lag-on-watermark-error"
	h := watermarkHarness(t, src, 100, 100) // cursor 100, tip 100: caught up
	h.cycle()
	if got := lagOf(src); got != 0 {
		t.Fatalf("caught-up lag = %v, want 0", got)
	}

	h.store.tipLedger = 400
	fake := &fakeLake{wm: 400, wmErr: errors.New("clickhouse: connection reset")}
	h.lake = &sourceLake{open: func(context.Context) (lakeReader, error) { return fake, nil }}
	errBefore := runsCount(t, src, "error")
	h.cycle()

	if d := runsCount(t, src, "error") - errBefore; d != 1 {
		t.Fatalf("runs_total{error} delta = %v, want 1", d)
	}
	if got := lagOf(src); got != 300 {
		t.Errorf("lag after a failed watermark read = %v, want 300 (tip 400 - cursor 100)", got)
	}
}

func TestCycle_StreamErrorPublishesLag(t *testing.T) {
	const src = "lag-on-stream-error"
	h := newWedgeHarness(t, src, nil, 100, func(consumer.Event) error { return nil })
	h.cycle()
	if got := lagOf(src); got != 0 {
		t.Fatalf("caught-up lag = %v, want 0", got)
	}

	h.store.tipLedger = 250
	h.events.streamErr = errors.New("pq: connection reset")
	h.cycle()

	if h.store.cursor() != 100 {
		t.Fatalf("cursor = %d, want it held at 100 on a stream error", h.store.cursor())
	}
	if got := lagOf(src); got != 150 {
		t.Errorf("lag after a failed stream = %v, want 150 (tip 250 - cursor 100)", got)
	}
}

func TestCycle_CursorReadErrorPublishesLagFromLastRead(t *testing.T) {
	const src = "lag-on-cursor-error"
	h := newWedgeHarness(t, src, nil, 100, func(consumer.Event) error { return nil })
	h.cycle()
	if got := lagOf(src); got != 0 {
		t.Fatalf("caught-up lag = %v, want 0", got)
	}

	h.store.tipLedger = 180
	h.store.cursorErr = errors.New("pq: projector_cursors: lock timeout")
	h.cycle()

	if got := lagOf(src); got != 80 {
		t.Errorf("lag after a failed cursor read = %v, want 80 (tip 180 - last read cursor 100)", got)
	}
}

// TestCycle_LogsLastSeenLedger pins that the cycle-summary "projector cycle" log line
// carries last_seen_ledger, matching the highest ledger this cycle's stream callback saw.
func TestCycle_LogsLastSeenLedger(t *testing.T) {
	const source = "t119-last-seen-ledger"
	rows := []events.Event{lakeEvent(101, 1), lakeEvent(102, 2)}

	h := newWedgeHarness(t, source, rows, 105, func(_ consumer.Event) error { return nil })
	var logs bytes.Buffer
	h.proj.logger = slog.New(slog.NewTextHandler(&logs, nil))

	h.cycle()

	if got := h.store.cursor(); got != 105 {
		t.Fatalf("cursor = %d, want 105 (full window committed)", got)
	}
	if !strings.Contains(logs.String(), "last_seen_ledger=102") {
		t.Errorf("projector cycle log missing last_seen_ledger=102 (highest ledger scanned); logs:\n%s", logs.String())
	}
}

// sushiswap_v3's gate grows live from the factory's pool_created events, so the contract-id prefilter must follow it. A
// pool created at 101 and traded at 102 must be projected before the cursor passes 102; a boot-time snapshot would skip
// the swap.
func TestCycle_PrefilterTracksLiveFactorySeededGate(t *testing.T) {
	reg, err := BuildRegistry([]string{sushiswap_v3.SourceName}, config.OracleConfig{}, nil, nil)
	if err != nil || len(reg.Sources) != 1 {
		t.Fatalf("BuildRegistry: %v (%d sources)", err, len(reg.Sources))
	}
	src := reg.Sources[0]

	var newPoolRaw [32]byte
	newPoolRaw[0] = 0x5a
	newPool, err := strkey.Encode(strkey.VersionByteContract, newPoolRaw[:])
	if err != nil {
		t.Fatal(err)
	}
	store := &fakeStore{projectorCursor: 100, haveCursor: true, tipLedger: 105}
	lakeEvs := &fakeEvents{
		evs: []events.Event{
			sushiEvent(101, sushiswap_v3.MainnetFactory, sushiswap_v3.TopicSymbolPoolCreated, poolCreatedFor(t, newPool)),
			sushiEvent(102, newPool, sushiswap_v3.TopicSymbolSwap, mustDecodeB64(t, sushiSwapB64)),
		},
	}
	var mu sync.Mutex
	var trades []uint32
	p, lake := newLakeEventsProjector(store, lakeEvs, func(_ context.Context, ev consumer.Event) error {
		if te, ok := ev.(sushiswap_v3.TradeEvent); ok {
			mu.Lock()
			trades = append(trades, te.Trade.Ledger)
			mu.Unlock()
		}
		return nil
	})
	widenedBefore := runsCount(t, src.Name, "gate_widened")
	window := uint32(BatchLimit)
	var tracker poisonTracker
	var wedge wedgeTracker
	for i := 0; i < 3 && store.cursor() < 105; i++ {
		p.cycleOneSource(context.Background(), src, &window, &tracker, &wedge, lake)
	}

	if got := store.cursor(); got != 105 {
		t.Fatalf("cursor = %d, want 105 (the window must still complete)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(trades) == 0 {
		t.Fatalf("the swap of pool %s (created at 101, traded at 102) was never projected; prefilters used: %v",
			newPool, lakeEvs.filters)
	}
	for _, l := range trades {
		if l != 102 {
			t.Errorf("projected trade at ledger %d, want 102", l)
		}
	}
	if got := runsCount(t, src.Name, "gate_widened") - widenedBefore; got != 1 {
		t.Errorf("gate_widened cycles = %v, want 1 (one held re-read, then convergence)", got)
	}
}

// TestCycle_PrefilterTracksSpectraSecondHop: Spectra's gate widens twice in one creation tx (pt_deployed admits the PT,
// then yt_deployed the YT). yt_deployed is emitted before pt_deployed, so each widening must hold the window for a re-
// read or the YT's transfer is lost.
func TestCycle_PrefilterTracksSpectraSecondHop(t *testing.T) {
	reg, err := BuildRegistry([]string{spectra.SourceName}, config.OracleConfig{}, nil, nil)
	if err != nil || len(reg.Sources) != 1 {
		t.Fatalf("BuildRegistry: %v (%d sources)", err, len(reg.Sources))
	}
	src := reg.Sources[0]

	pt, yt := testContract(t, 0x6a), testContract(t, 0x6b)
	store := &fakeStore{projectorCursor: 100, haveCursor: true, tipLedger: 105}
	lakeEvs := &fakeEvents{
		evs: []events.Event{
			spectraEvent(101, 4, pt, []string{spectraYTDeployedTopic},
				withContractField(t, spectraYTDeployedB64, "address", yt)),
			spectraEvent(101, 6, spectra.MainnetFactory, []string{spectraPTDeployedTopic},
				withContractField(t, spectraPTDeployedB64, "pt", pt)),
			spectraEvent(102, 2, yt, spectraTransferTopics, mustDecodeB64(t, spectraTransferB64)),
		},
	}
	var mu sync.Mutex
	projected := map[string]uint32{}
	p, lake := newLakeEventsProjector(store, lakeEvs, func(_ context.Context, ev consumer.Event) error {
		if se, ok := ev.(spectra.Event); ok {
			mu.Lock()
			projected[se.Kind+"@"+se.ContractID] = se.Ledger
			mu.Unlock()
		}
		return nil
	})
	widenedBefore := runsCount(t, src.Name, "gate_widened")
	window := uint32(BatchLimit)
	var tracker poisonTracker
	var wedge wedgeTracker
	for i := 0; i < 5 && store.cursor() < 105; i++ {
		p.cycleOneSource(context.Background(), src, &window, &tracker, &wedge, lake)
	}

	if got := store.cursor(); got != 105 {
		t.Fatalf("cursor = %d, want 105 (the window must still complete)", got)
	}
	mu.Lock()
	defer mu.Unlock()
	want := map[string]uint32{
		spectra.EventPTDeployed + "@" + spectra.MainnetFactory: 101,
		spectra.EventYTDeployed + "@" + pt:                     101,
		spectra.EventTransfer + "@" + yt:                       102,
	}
	for k, l := range want {
		if got, ok := projected[k]; !ok || got != l {
			t.Errorf("%s projected at %d (present=%v), want ledger %d; prefilters used: %v",
				k, got, ok, l, lakeEvs.filters)
		}
	}
	if len(projected) != len(want) {
		t.Errorf("projected %v, want exactly %v", projected, want)
	}
	if got := runsCount(t, src.Name, "gate_widened") - widenedBefore; got != 2 {
		t.Errorf("gate_widened cycles = %v, want 2 (one held re-read per hop, then convergence)", got)
	}
}

// TestCycle_GlobalPermanentFaultShedsAtMostOneRowPerCycle: a bad migration makes the sink reject every row with a
// class-23 error. The cursor holds (visible lag, sink_retry) until the no-progress budget is spent, then sheds one row
// per cycle; uncapped, the first cycle would forget all five rows.
func TestCycle_GlobalPermanentFaultShedsAtMostOneRowPerCycle(t *testing.T) {
	const source = "rlt131-global-not-null"
	rows := []events.Event{
		lakeEvent(101, 1), lakeEvent(102, 2), lakeEvent(103, 3), lakeEvent(104, 4), lakeEvent(105, 5),
	}
	okRunsBefore := runsCount(t, source, "ok")
	retryRunsBefore := runsCount(t, source, "sink_retry")

	h := newWedgeHarness(t, source, rows, 110, func(consumer.Event) error {
		return notNullViolation()
	})

	h.cycle()

	if got := h.store.cursor(); got != 100 {
		t.Fatalf("cycle 1: cursor = %d, want 100 — a class-23502 fault rejecting EVERY row of the window has no sink-health proof, so NOTHING may be shed yet; 101 sheds a row an hour before an operator can see the fault, 110 drops the whole backlog in a single pass", got)
	}
	if got := runsCount(t, source, "sink_retry") - retryRunsBefore; got != 1 {
		t.Errorf("runs_total{outcome=sink_retry} delta = %v, want 1 — a cycle that held rows back must not be reported as a clean run", got)
	}
	if got := runsCount(t, source, "ok") - okRunsBefore; got != 0 {
		t.Errorf("runs_total{outcome=ok} delta = %v, want 0 — all five rows are still held", got)
	}

	// The stall lasts the whole no-progress budget: the operator's window to
	// fix the migration before any row leaves the served tier.
	for i := 2; i < QuarantineAfterCyclesNoProgress; i++ {
		h.cycle()
		if got := h.store.cursor(); got != 100 {
			t.Fatalf("cycle %d: cursor = %d, want 100 — the stall must last the whole no-progress budget", i, got)
		}
	}

	// Budget spent: the held rows are re-read and bled off one per cycle —
	// bounded and loud, never a permanent stall (a poison row must not
	// wedge a sole-writer domain).
	for _, want := range []uint32{101, 102, 103, 104} {
		h.cycle()
		if got := h.store.cursor(); got != want {
			t.Fatalf("cursor = %d, want %d — the backlog must drain at exactly one poison row per cycle", got, want)
		}
	}

	// Last poison row: nothing is left to hold, so the cursor catches up to
	// the tip in the same cycle.
	h.cycle()
	if got := h.store.cursor(); got != 110 {
		t.Fatalf("final cycle: cursor = %d, want 110 — once the last poison row is shed the source must catch up to the tip, not wedge", got)
	}
}

// TestCycle_PoisonOutputOnAHeldRowDoesNotResetItsRetryBudget: a multi-output row with both a dropped output and a
// cursor-holding fault is retried whole, so it must not be a shed candidate; shedding forgets the row's identity, resets
// the quarantine count and wedges a sole-writer source.
func TestCycle_PoisonOutputOnAHeldRowDoesNotResetItsRetryBudget(t *testing.T) {
	const source = "rlt131-drop-plus-held"
	quarantinedBefore := decodedCount(t, source, "sink_quarantined")

	h := newWedgeHarness(t, source, []events.Event{lakeEvent(101, 1)}, 105, nil)
	h.src.Decoder = &scriptedDecoder{build: []func(events.Event) consumer.Event{poisonTrade, echoOutput}}
	h.proj.sink = productionTradeSink(func(consumer.Event) error {
		// Unclassified: neither a positively permanent data fault nor a
		// positively transient infra one, so it is held under the budget.
		return errors.New("timescale: InsertSEP41TransferBatch: row 0 transfer negative Amount -1")
	})

	// Nothing else committed this cycle, so the long no-progress budget is the
	// one that applies.
	for i := 1; i < QuarantineAfterCyclesNoProgress; i++ {
		h.cycle()
		if got := h.store.cursor(); got != 100 {
			t.Fatalf("cycle %d: cursor = %d, want 100 — the held fault must keep the cursor below its ledger for the whole budget", i, got)
		}
	}

	h.cycle()

	if got := decodedCount(t, source, "sink_quarantined") - quarantinedBefore; got != 1 {
		t.Errorf("outcome=sink_quarantined delta = %v, want 1 — the budget must accumulate across cycles even though the row also drops a poison output", got)
	}
	if got := h.store.cursor(); got != 105 {
		t.Fatalf("cursor = %d, want 105 — the row quarantines once its budget is spent; a reset budget would wedge the source forever", got)
	}
}

// TestCycle_PoisonShedTakesTheLowestLedgerAndKeepsGoodRowsFlowing: only the lowest-ledger poison row is shed, so the
// watermark advances in ledger order. With rows 101 poison, 102 valid, 103 poison, 104 valid, the cycle sheds 101 only
// and the watermark lands at 102, not 110.
func TestCycle_PoisonShedTakesTheLowestLedgerAndKeepsGoodRowsFlowing(t *testing.T) {
	const source = "rlt131-lowest-ledger-first"
	rows := []events.Event{lakeEvent(101, 1), lakeEvent(102, 2), lakeEvent(103, 3), lakeEvent(104, 4)}
	okBefore := decodedCount(t, source, "ok")

	h := newWedgeHarness(t, source, rows, 110, func(ev consumer.Event) error {
		switch ev.(ledgerEvent).ledger {
		case 101, 103:
			return notNullViolation()
		default:
			return nil
		}
	})

	h.cycle()

	if got := h.store.cursor(); got != 102 {
		t.Fatalf("cycle 1: cursor = %d, want 102 — only the lowest poison row (101) may be shed, so the watermark stops below the one still held (103)", got)
	}
	if got := decodedCount(t, source, "ok") - okBefore; got != 2 {
		t.Errorf("outcome=ok delta = %v, want 2 — holding a poison row must not stop the window's valid rows reaching the sink", got)
	}

	// Ledger 103 is re-read next cycle, shed, and 104 is already durable, so
	// the source catches up.
	h.cycle()
	if got := h.store.cursor(); got != 110 {
		t.Fatalf("cycle 2: cursor = %d, want 110", got)
	}
}

// TestCycle_PoisonRowWithNoSinkHealthProofHoldsUntilTheNoProgressBudget: the per-cycle shed cap bounds the rate of a
// global class-22/23 fault, not the fact of it. Without madeProgress (another event committed) the cursor holds for
// QuarantineAfterCyclesNoProgress cycles so lag/sink_retry alerts fire, then sheds one row per cycle.
func TestCycle_PoisonRowWithNoSinkHealthProofHoldsUntilTheNoProgressBudget(t *testing.T) {
	const source = "rlt131-no-health-proof"
	// One row, and it is poison: nothing else commits, so this cycle has NO
	// evidence the sink is healthy. A bad migration and a genuinely bad row
	// look identical from here, and the safe reading is the pessimistic one.
	h := newWedgeHarness(t, source, []events.Event{lakeEvent(101, 1)}, 105, func(consumer.Event) error {
		return notNullViolation()
	})

	permBefore := decodedCount(t, source, "sink_permanent")

	for i := 1; i < QuarantineAfterCyclesNoProgress; i++ {
		h.cycle()
		if got := h.store.cursor(); got != 100 {
			t.Fatalf("cycle %d: cursor = %d, want 100 — with no proof the sink is healthy, a class-23502 verdict must stall visibly instead of shedding the row", i, got)
		}
	}

	// The stall is not silence: outcome="sink_permanent" counts the sink's REJECTION on every
	// cycle that re-reads the row, so it climbs through the whole hold and
	// stellarindex_projector_row_dropped_permanent can fire BEFORE anything leaves the served
	// tier. A counter that only moved on the shed would leave no warning window.
	if got := decodedCount(t, source, "sink_permanent") - permBefore; got != float64(QuarantineAfterCyclesNoProgress-1) {
		t.Errorf("sink_permanent delta over the hold = %v, want %d — the rejection must be counted every cycle, not only when the row is finally shed",
			got, QuarantineAfterCyclesNoProgress-1)
	}

	// Budget spent: the source self-heals rather than wedging, one row per
	// cycle, exactly as the unclassified arm does.
	h.cycle()
	if got := h.store.cursor(); got != 105 {
		t.Fatalf("cursor = %d, want 105 — after the no-progress budget the poison row is shed so a sparse source still self-heals", got)
	}
}

// TestCycle_PoisonRowWithSinkHealthProofShedsOnCycleOne is the other side of the same rail,
// and why the proof is a DISCRIMINATOR rather than a blanket delay: a scattered poison row
// sits beside rows that commit, so the proof is present and the row costs exactly one cycle.
func TestCycle_PoisonRowWithSinkHealthProofShedsOnCycleOne(t *testing.T) {
	const source = "rlt131-with-health-proof"
	rows := []events.Event{lakeEvent(101, 1), lakeEvent(102, 2)}

	h := newWedgeHarness(t, source, rows, 105, func(ev consumer.Event) error {
		if ev.(ledgerEvent).ledger == 101 {
			return notNullViolation()
		}
		return nil // ledger 102 commits — THE health proof
	})

	h.cycle()
	if got := h.store.cursor(); got != 105 {
		t.Fatalf("cycle 1: cursor = %d, want 105 — with another event durably committed the class-23502 verdict is row-local and sheds at once", got)
	}
}

// In soroban_events mode the ledgerstream cursor advances when raw rows are enqueued, before they commit. A cycle
// scanning to tip while rows are still buffered would commit the cursor past them forever, so the barrier must land them
// before the scan.
func TestCycle_SorobanEventsModeWaitsForBufferedRawRows(t *testing.T) {
	var sunk []uint32
	h := newWedgeHarness(t, "raw-barrier", nil, 200,
		func(ev consumer.Event) error {
			sunk = append(sunk, ev.(ledgerEvent).ledger)
			return nil
		})
	h.sorobanEventsMode(lakeRow(150, 1))
	buffered := []sorobanevents.Row{lakeRow(200, 2)}
	h.proj.SetRawEventBarrier(func(context.Context) error {
		h.store.mu.Lock()
		h.store.rows = append(h.store.rows, buffered...)
		h.store.mu.Unlock()
		buffered = nil
		return nil
	})

	h.cycle()

	if h.store.cursor() != 200 {
		t.Fatalf("cursor = %d, want 200", h.store.cursor())
	}
	if !slices.Equal(sunk, []uint32{150, 200}) {
		t.Errorf("projected ledgers = %v, want [150 200]: the tip's buffered row was skipped", sunk)
	}
}

// A barrier that cannot settle (Postgres down, a stuck flush) fails the
// cycle and holds the cursor: advancing past unsettled rows is the loss.
func TestCycle_SorobanEventsBarrierFailureHoldsCursor(t *testing.T) {
	const src = "raw-barrier-fail"
	h := newWedgeHarness(t, src, nil, 200,
		func(consumer.Event) error { return nil })
	h.sorobanEventsMode(lakeRow(150, 1))
	h.proj.SetRawEventBarrier(func(context.Context) error {
		return errors.New("sorobanevents: waiting for accepted rows to settle: context deadline exceeded")
	})
	errBefore := runsCount(t, src, "error")

	h.cycle()

	if h.store.cursor() != 100 {
		t.Errorf("cursor = %d, want it held at 100", h.store.cursor())
	}
	if d := runsCount(t, src, "error") - errBefore; d != 1 {
		t.Errorf("runs_total{error} delta = %v, want 1", d)
	}
	if got := lagOf(src); got != 100 {
		t.Errorf("lag = %v, want 100 (tip 200 - cursor 100)", got)
	}
}

// TestCycle_MalformedRowIsLoggedWithIdentity: a landing-zone row that Reconstruct rejects counts as reconstruct_error
// (not decode_error) and marks the cycle decode_degraded. It is logged with full identity and error, throttled to the
// first and every reconstructErrLogEvery-th failure.
func TestCycle_MalformedRowIsLoggedWithIdentity(t *testing.T) {
	const source = "malformed-row-log"
	const bad = 2*reconstructErrLogEvery + 1
	rows := make([]sorobanevents.Row, 0, bad+1)
	for i := 0; i < bad; i++ {
		r := lakeRow(uint32(101+i), byte(i+1))
		r.Topic0XDR = nil // Reconstruct: missing topic_0_xdr
		rows = append(rows, r)
	}
	rows = append(rows, lakeRow(uint32(101+bad), 0xff))
	beforeReconstruct := decodedCount(t, source, "reconstruct_error")
	beforeDecode := decodedCount(t, source, "decode_error")
	beforeDegraded := runsCount(t, source, "decode_degraded")

	h := newWedgeHarness(t, source, nil, uint32(101+bad+5), func(consumer.Event) error { return nil })
	h.sorobanEventsMode(rows...)
	var logs bytes.Buffer
	h.proj.logger = slog.New(slog.NewTextHandler(&logs, nil))
	h.cycle()

	if got := decodedCount(t, source, "reconstruct_error") - beforeReconstruct; got != bad {
		t.Errorf("reconstruct_error delta = %v, want %d", got, bad)
	}
	if got := decodedCount(t, source, "decode_error") - beforeDecode; got != 0 {
		t.Errorf("decode_error delta = %v, want 0 (reconstruct failures are counted separately)", got)
	}
	if got := runsCount(t, source, "decode_degraded") - beforeDegraded; got != 1 {
		t.Errorf("runs_total{outcome=decode_degraded} delta = %v, want 1", got)
	}
	out := logs.String()
	// Failures 1, reconstructErrLogEvery and 2*reconstructErrLogEvery.
	if got, want := strings.Count(out, malformedRowWarning), 3; got != want {
		t.Errorf("malformed-row warnings = %d, want %d; logs:\n%s", got, want, out)
	}
	first := rows[0]
	for _, want := range []string{
		"source=" + source,
		"ledger=101",
		"tx=0165",
		"op_index=0",
		"event_index=1",
		"contract=" + first.ContractID,
		"missing topic_0_xdr",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("malformed-row warning lacks %q; logs:\n%s", want, out)
		}
	}
}

// TestCycle_DroppedOutputDoesNotAbortTheRowsOtherOutputs: a permanently dropped output must be counted and the loop must
// continue, else the cursor skips the row and loses its valid outputs. [poison trade, valid output] sinks the valid one
// (ok=1), counts the drop (sink_permanent=1) and advances the cursor.
func TestCycle_DroppedOutputDoesNotAbortTheRowsOtherOutputs(t *testing.T) {
	const source = "rlt132-sibling-after-drop"
	okBefore := decodedCount(t, source, "ok")
	permBefore := decodedCount(t, source, "sink_permanent")
	retryBefore := decodedCount(t, source, "sink_retry")

	h := newWedgeHarness(t, source, []events.Event{lakeEvent(101, 1)}, 105, nil)
	h.src.Decoder = &scriptedDecoder{build: []func(events.Event) consumer.Event{poisonTrade, echoOutput}}
	siblingSunk := 0
	h.proj.sink = productionTradeSink(func(consumer.Event) error { siblingSunk++; return nil })

	h.cycle()

	if siblingSunk != 1 {
		t.Errorf("valid sibling output sunk %d times, want 1 — a permanently dropped output aborted the row and its remaining valid output was never offered to the sink", siblingSunk)
	}
	if got := decodedCount(t, source, "ok") - okBefore; got != 1 {
		t.Errorf("outcome=ok delta = %v, want 1 (the valid sibling output, and only it)", got)
	}
	if got := decodedCount(t, source, "sink_permanent") - permBefore; got != 1 {
		t.Errorf("outcome=sink_permanent delta = %v, want 1 (the dropped trade)", got)
	}
	if got := decodedCount(t, source, "sink_retry") - retryBefore; got != 0 {
		t.Errorf("outcome=sink_retry delta = %v, want 0 — nothing here is retryable", got)
	}
	if got := h.store.cursor(); got != 105 {
		t.Fatalf("cursor = %d, want 105 — a deterministic drop must not hold the source", got)
	}
}

// TestCycle_PermanentDropsAreCountedPerOutput — sink_permanent is a per-OUTPUT
// count, like outcome=ok. One row decoding to two poison trades around a valid
// output is two drops, not "one bad row".
func TestCycle_PermanentDropsAreCountedPerOutput(t *testing.T) {
	const source = "rlt132-per-output-drop-count"
	okBefore := decodedCount(t, source, "ok")
	permBefore := decodedCount(t, source, "sink_permanent")

	h := newWedgeHarness(t, source, []events.Event{lakeEvent(101, 1)}, 105, nil)
	h.src.Decoder = &scriptedDecoder{build: []func(events.Event) consumer.Event{poisonTrade, echoOutput, poisonTrade}}
	h.proj.sink = productionTradeSink(func(consumer.Event) error { return nil })

	h.cycle()

	if got := decodedCount(t, source, "sink_permanent") - permBefore; got != 2 {
		t.Errorf("outcome=sink_permanent delta = %v, want 2 — one per dropped output, not one per row", got)
	}
	if got := decodedCount(t, source, "ok") - okBefore; got != 1 {
		t.Errorf("outcome=ok delta = %v, want 1", got)
	}
	if got := h.store.cursor(); got != 105 {
		t.Fatalf("cursor = %d, want 105", got)
	}
}

// TestCycle_RetryableFaultAfterADropStillHoldsTheRow: continuing past a permanent drop must not weaken no-silent-loss. A
// transient fault on a later output still stops the row and holds the cursor below its ledger; later outputs are not
// offered, and idempotent sinks absorb the re-read.
func TestCycle_RetryableFaultAfterADropStillHoldsTheRow(t *testing.T) {
	const source = "rlt132-drop-then-transient"
	permBefore := decodedCount(t, source, "sink_permanent")
	retryBefore := decodedCount(t, source, "sink_retry")

	h := newWedgeHarness(t, source, []events.Event{lakeEvent(101, 1)}, 105, nil)
	h.src.Decoder = &scriptedDecoder{build: []func(events.Event) consumer.Event{poisonTrade, echoOutput, echoOutput}}
	offered := 0
	h.proj.sink = productionTradeSink(func(consumer.Event) error {
		offered++
		return context.DeadlineExceeded // positively transient
	})

	h.cycle()

	if offered != 1 {
		t.Errorf("outputs offered after the drop = %d, want 1 — a retryable fault must still stop the row at the failing output", offered)
	}
	if got := decodedCount(t, source, "sink_retry") - retryBefore; got != 1 {
		t.Errorf("outcome=sink_retry delta = %v, want 1", got)
	}
	if got := decodedCount(t, source, "sink_permanent") - permBefore; got != 1 {
		t.Errorf("outcome=sink_permanent delta = %v, want 1 — the drop happened and is counted even though the row is then held", got)
	}
	if got := h.store.cursor(); got != 100 {
		t.Fatalf("cursor = %d, want 100 (held) — a transient fault at the window's first ledger must not advance it", got)
	}
}

// A never-run source starts at its first event, not ledger 0: one cycle
// projects the events and commits to the tip, instead of crawling the empty
// pre-history BatchLimit ledgers per Interval (~89 h on mainnet).
func TestCycle_NoCursorSeedsAtFirstEvent(t *testing.T) {
	const first, second, tip = 50_000_000, 50_000_010, 50_000_020
	h, seen := newSeedHarness(t, "seed-first-event", []events.Event{lakeEvent(first, 1), lakeEvent(second, 2)}, tip)

	h.cycle()

	if got := h.store.cursor(); got != tip {
		t.Fatalf("cursor after first cycle = %d, want %d (scan from the first event through the tip)", got, tip)
	}
	if got, want := seen(), []uint32{first, second}; !slices.Equal(got, want) {
		t.Fatalf("sink saw ledgers %v, want %v", got, want)
	}
}

// With no matching event at or below the tip there is nothing to project:
// the cursor lands on the tip in one cycle.
func TestCycle_NoCursorNoEventsSeedsAtTip(t *testing.T) {
	const tip = 50_000_000
	h, seen := newSeedHarness(t, "seed-no-events", nil, tip)

	h.cycle()

	if got := h.store.cursor(); got != tip {
		t.Fatalf("cursor after first cycle = %d, want %d", got, tip)
	}
	if got := seen(); len(got) != 0 {
		t.Fatalf("sink saw ledgers %v, want none", got)
	}
}

// An event beyond the durable tip is not a seed: the seek is bounded by the
// same tip the scan is, so the source waits at the tip rather than jumping
// past ledgers that are not yet durable.
func TestCycle_NoCursorSeekBoundedByTip(t *testing.T) {
	const tip = 50_000_000
	h, seen := newSeedHarness(t, "seed-beyond-tip", []events.Event{lakeEvent(tip+5, 1)}, tip)

	h.cycle()

	if got := h.store.cursor(); got != tip {
		t.Fatalf("cursor after first cycle = %d, want %d", got, tip)
	}
	if got := seen(); len(got) != 0 {
		t.Fatalf("sink saw ledgers %v, want none (event is beyond the tip)", got)
	}
}

// A failed seek degrades to the lossless crawl from ledger 0, and is not
// retried every cycle.
func TestCycle_NoCursorSeekFailureFallsBackToCrawl(t *testing.T) {
	h, _ := newSeedHarness(t, "seed-seek-fails", []events.Event{lakeEvent(50_000_000, 1)}, 50_000_010)
	h.events.seekErr = errors.New("seek unavailable")

	h.cycle()

	if got := h.store.cursor(); got != BatchLimit {
		t.Fatalf("cursor after failed seek = %d, want %d (crawl from 0)", got, BatchLimit)
	}
	h.store.haveCursor = false
	h.cycle()
	if h.events.seeks != 1 {
		t.Fatalf("seeks = %d, want 1 (fallback is remembered)", h.events.seeks)
	}
}

// TestCycle_SinkPanicIsShedLikeAPermanentFault: pipeline.HandleEvent wraps a recovered sink panic in
// pipeline.ErrSinkPanic, which the sink classifier drops. The projector must read it the same way (shed on cycle one
// given a health proof, counted sink_permanent), not re-run the panicking decode for the whole quarantine budget.
func TestCycle_SinkPanicIsShedLikeAPermanentFault(t *testing.T) {
	const source = "q053-sink-panic"
	rows := []events.Event{lakeEvent(101, 1), lakeEvent(102, 2)}
	beforePermanent := decodedCount(t, source, "sink_permanent")
	beforeRetry := decodedCount(t, source, "sink_retry")

	h := newWedgeHarness(t, source, rows, 105, func(ev consumer.Event) error {
		if ev.(ledgerEvent).ledger == 101 {
			return fmt.Errorf("%w for %s/echo: runtime error: nil map write", pipeline.ErrSinkPanic, source)
		}
		return nil // ledger 102 commits — the health proof
	})

	h.cycle()
	if got := h.store.cursor(); got != 105 {
		t.Fatalf("cycle 1: cursor = %d, want 105 — a recovered sink panic is deterministic for the event and must be shed like a class-22/23 verdict, not held", got)
	}
	if got := decodedCount(t, source, "sink_permanent") - beforePermanent; got != 1 {
		t.Errorf("sink_permanent delta = %v, want 1 (the panicking row, counted as a permanent drop)", got)
	}
	if got := decodedCount(t, source, "sink_retry") - beforeRetry; got != 0 {
		t.Errorf("sink_retry delta = %v, want 0 (nothing was held for retry)", got)
	}
}

// TestCycle_DroppedTradeIsNotReportedOK: persistTrade must not return nil for a permanently dropped trade, or it counts
// as outcome="ok" (durably committed) while in no tier or loss counter. It is labelled sink_permanent; a lone row cannot
// tell a bad trade from a global sink fault, so the cursor holds the no-progress budget, then sheds.
func TestCycle_DroppedTradeIsNotReportedOK(t *testing.T) {
	const source = "rlt132-dropped-trade"
	rows := []events.Event{lakeEvent(101, 1)}
	okBefore := decodedCount(t, source, "ok")
	permBefore := decodedCount(t, source, "sink_permanent")
	retryBefore := decodedCount(t, source, "sink_retry")

	h := newWedgeHarness(t, source, rows, 105, nil)
	h.src.Decoder = &invalidTradeDecoder{}
	h.proj.sink = func(ctx context.Context, ev consumer.Event) error {
		return pipeline.HandleEvent(ctx, discardLog(), nil, ev)
	}

	h.cycle()

	if got := decodedCount(t, source, "ok") - okBefore; got != 0 {
		t.Errorf("outcome=ok delta = %v, want 0 — a trade the store permanently rejected was reported as durably committed", got)
	}
	if got := decodedCount(t, source, "sink_permanent") - permBefore; got != 1 {
		t.Errorf("outcome=sink_permanent delta = %v, want 1 — the dropped trade must be counted as a permanent sink fault", got)
	}
	if got := decodedCount(t, source, "sink_retry") - retryBefore; got != 0 {
		t.Errorf("outcome=sink_retry delta = %v, want 0 — a permanent verdict is never counted as a transient hold", got)
	}
	if got := h.store.cursor(); got != 100 {
		t.Fatalf("cycle 1: cursor = %d, want 100 — with nothing else committed this cycle, a permanent verdict must stall visibly before anything is shed (RLT-131)", got)
	}

	// …and the source still self-heals: the anti-wedge property is
	// preserved, just deferred behind the no-progress budget.
	for i := 2; i < QuarantineAfterCyclesNoProgress; i++ {
		h.cycle()
		if got := h.store.cursor(); got != 100 {
			t.Fatalf("cycle %d: cursor = %d, want 100 (the stall lasts the whole no-progress budget)", i, got)
		}
	}
	h.cycle()
	if got := h.store.cursor(); got != 105 {
		t.Fatalf("cursor = %d after %d cycles, want 105 — a trade that can never land must not wedge a sole-writer source forever", got, QuarantineAfterCyclesNoProgress)
	}
}

// A lake hole at ledger 101 stalls the watermark at 100 while ledgerstream holds through 200. The source must count as
// watermark_held and report the real 100-ledger lag, or a stalled watermark looks like a healthy idle source and no lag
// alert can fire.
func TestCycle_WatermarkClampedIdleReportsHeldWithRealLag(t *testing.T) {
	const src = "wm-held"
	h := watermarkHarness(t, src, 200, 100)
	idleBefore := runsCount(t, src, "idle")
	heldBefore := runsCount(t, src, "watermark_held")

	h.cycle()

	if got := testutil.ToFloat64(obs.ProjectorLagLedgers.WithLabelValues(src)); got != 100 {
		t.Errorf("lag = %v, want 100 (ledgerstream tip 200 - cursor 100)", got)
	}
	if d := runsCount(t, src, "watermark_held") - heldBefore; d != 1 {
		t.Errorf("runs_total{watermark_held} delta = %v, want 1", d)
	}
	if d := runsCount(t, src, "idle") - idleBefore; d != 0 {
		t.Errorf("runs_total{idle} delta = %v, want 0 for a watermark-held cycle", d)
	}
	if h.store.projectorCursor != 100 {
		t.Errorf("cursor = %d, want it held at 100", h.store.projectorCursor)
	}
}

// When ledgerstream itself is at the cursor, the empty scan range is a real
// catch-up: idle with lag 0.
func TestCycle_WatermarkAtDurableTipIsIdle(t *testing.T) {
	const src = "wm-caught-up"
	h := watermarkHarness(t, src, 100, 100)
	idleBefore := runsCount(t, src, "idle")
	heldBefore := runsCount(t, src, "watermark_held")

	h.cycle()

	if got := testutil.ToFloat64(obs.ProjectorLagLedgers.WithLabelValues(src)); got != 0 {
		t.Errorf("lag = %v, want 0", got)
	}
	if d := runsCount(t, src, "idle") - idleBefore; d != 1 {
		t.Errorf("runs_total{idle} delta = %v, want 1", d)
	}
	if d := runsCount(t, src, "watermark_held") - heldBefore; d != 0 {
		t.Errorf("runs_total{watermark_held} delta = %v, want 0", d)
	}
}

func lagOf(source string) float64 {
	return testutil.ToFloat64(obs.ProjectorLagLedgers.WithLabelValues(source))
}

// heldRowWarning is the per-row line under test; the per-cycle
// "no fully-committed progress" warning also says "holding cursor" and is
// deliberately NOT throttled, so the count keys on the row-level text.
const heldRowWarning = "sink failure — holding cursor for retry (NOT advancing past this ledger)"

// poolCreatedFor rewrites the real pool_created body to announce pool.
func poolCreatedFor(t *testing.T, pool string) []byte {
	t.Helper()
	return withContractField(t, sushiPoolCreatedB64, "pool_address", pool)
}

// Real sushiswap_v3 lake bodies (internal/sources/sushiswap_v3/fixtures_test.go):
// the factory's pool_created for the XLM/USDC 0.30% pool (ledger
// 61,487,379) and a swap from the original pool WASM.
const (
	sushiPoolCreatedB64 = "AAAAEQAAAAEAAAAGAAAADwAAAANmZWUAAAAAAwAAC7gAAAAPAAAADHBvb2xfYWRkcmVzcwAAABIAAAABo6EfhoVFk5viOWrGgaJXOip0dVXyhCJjzAFNiInP4wMAAAAPAAAABnNlbmRlcgAAAAAAEgAAAAH2qKjDjWz71Dut10ZBTL+vn0NH3viK5Hy92gYqUawX0wAAAA8AAAAMdGlja19zcGFjaW5nAAAABAAAADwAAAAPAAAABnRva2VuMAAAAAAAEgAAAAEltPzYWa7C+mNIQ4xImzw8EMmLbSG+T9PLMMtolT75dwAAAA8AAAAGdG9rZW4xAAAAAAASAAAAAa3vzlmu5Slo92Bh1JTCUlt1ZZ+kKWpl9JnvKeVkd+SW"
	sushiSwapB64        = "AAAAEQAAAAEAAAAHAAAADwAAAAdhbW91bnQwAAAAAAoAAAAAAAAAAAAAAAAFuAFpAAAADwAAAAdhbW91bnQxAAAAAAr/////////////////CT5hAAAADwAAAAlsaXF1aWRpdHkAAAAAAAAJAAAAAAAAAAAAAAAAHZIo3QAAAA8AAAAJcmVjaXBpZW50AAAAAAAAEgAAAAAAAAAAxRy/OA51yJ4u3YL0mKNf2jKqkAy3kYfYMFIdzMphcBgAAAAPAAAABnNlbmRlcgAAAAAAEgAAAAAAAAAAxRy/OA51yJ4u3YL0mKNf2jKqkAy3kYfYMFIdzMphcBgAAAAPAAAADnNxcnRfcHJpY2VfeDk2AAAAAAALAAAAAAAAAAAAAAAAAAAAAAAAAABlKxxd8TMIn9sIRdQAAAAPAAAABHRpY2sAAAAE//+3dw=="
)

// withContractField rewrites the contract-address field of a real map body.
func withContractField(t *testing.T, bodyB64, field, contract string) []byte {
	t.Helper()
	var sv xdr.ScVal
	if err := sv.UnmarshalBinary(mustDecodeB64(t, bodyB64)); err != nil {
		t.Fatal(err)
	}
	raw, err := strkey.Decode(strkey.VersionByteContract, contract)
	if err != nil {
		t.Fatal(err)
	}
	var cid xdr.ContractId
	copy(cid[:], raw)
	for i := range **sv.Map {
		e := &(**sv.Map)[i]
		if string(*e.Key.Sym) == field {
			e.Val.Address.ContractId = &cid
		}
	}
	b, err := sv.MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const malformedRowWarning = "malformed landing-zone row — skipped"

func mustDecodeB64(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Real Spectra mainnet bodies (test/fixtures/spectra): the factory's
// pt_deployed and the PT's yt_deployed from the USDC market's creation
// transaction (ledger 63,782,624), and a YT transfer.
const (
	spectraPTDeployedTopic = "AAAADwAAAAtwdF9kZXBsb3llZAA="
	spectraPTDeployedB64   = "AAAAEQAAAAEAAAAEAAAADwAAAAhkZXBsb3llcgAAABIAAAAAAAAAAJovmvVGl4o2X6sTeIxd+eulvvp97zlWCE+aU/YQs1qKAAAADwAAAAhkdXJhdGlvbgAAAAUAAAAAAHanAAAAAA8AAAADaWJ0AAAAABIAAAABYz4ToD62ZkYI+fx4W7w7BsVwWtudoRG9cexHSjgMMiYAAAAPAAAAAnB0AAAAAAASAAAAAQDo9LzZpQzhJJHRVZshYdh90qlkgkOVQqBU6kJZZAXa"
	spectraYTDeployedTopic = "AAAADwAAAAt5dF9kZXBsb3llZAA="
	spectraYTDeployedB64   = "AAAAEQAAAAEAAAABAAAADwAAAAdhZGRyZXNzAAAAABIAAAABRwyW2P77E+3wW1XAoc745pxzMLK7gZRZ+RC+jA7Xffk="
	spectraTransferB64     = "AAAACgAAAAAAAAAAAAAAAAAAnEA="
)

// watermarkHarness is a CH feed-switch projector (projector cursor 100) whose
// ledgerstream tip and lake contiguous watermark are fixed per test.
func watermarkHarness(t *testing.T, name string, ledgerstreamTip, watermark uint32) *wedgeHarness {
	t.Helper()
	h := newWedgeHarness(t, name, nil, ledgerstreamTip, func(consumer.Event) error { return nil })
	h.events.lake.wm = watermark
	return h
}

var spectraTransferTopics = []string{
	"AAAADwAAAAh0cmFuc2Zlcg==",
	"AAAAEgAAAAAAAAAAmi+a9UaXijZfqxN4jF3566W++n3vOVYIT5pT9hCzWoo=",
	"AAAAEgAAAAGU1wln9BCzS8bGbMh5Ij2Fof+uEjsVheKhflTNzWyFnA==",
}

func spectraEvent(ledger uint32, eventIndex int, contract string, topicsB64 []string, body []byte) events.Event {
	ev := sushiEvent(ledger, contract, topicsB64[0], body)
	ev.EventIndex = eventIndex
	ev.Topic = slices.Clone(topicsB64)
	return ev
}

// notNullViolation is what a migration adding a NOT NULL column puts on every insert: SQLSTATE 23502, class 23, a
// permanent data fault per timescale.IsPermanentDataError. The trap: the verdict says these values are bad, but a global
// fault yields the same SQLSTATE, making every row of the window look like poison.
func notNullViolation() error {
	return &pgconn.PgError{
		Code:    "23502",
		Message: `null value in column "quote_asset_id" of relation "trades" violates not-null constraint`,
	}
}

func sushiEvent(ledger uint32, contract, topic0B64 string, body []byte) events.Event {
	txHash := make([]byte, 32)
	txHash[0] = byte(ledger)
	return events.Event{
		Type:           "contract",
		Ledger:         ledger,
		LedgerClosedAt: time.Unix(1_750_000_000+int64(ledger), 0).UTC().Format(time.RFC3339),
		ContractID:     contract,
		TxHash:         hex.EncodeToString(txHash),
		Topic:          []string{topic0B64},
		Value:          base64.StdEncoding.EncodeToString(body),
	}
}

// ctxStore refuses a cursor write on a done context, as pgx's ExecContext
// does; the base fake ignores ctx and so cannot see which one the write used.
type ctxStore struct{ *fakeStore }

func (s ctxStore) AdvanceCursorFrom(ctx context.Context, source, sub string, expected timescale.CursorRead, newLast uint32) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if _, ok := ctx.Deadline(); !ok {
		return false, fmt.Errorf("fake: cursor write carries no deadline")
	}
	return s.fakeStore.AdvanceCursorFrom(ctx, source, sub, expected, newLast)
}

func testContract(t *testing.T, seed byte) string {
	t.Helper()
	var raw [32]byte
	raw[0] = seed
	c, err := strkey.Encode(strkey.VersionByteContract, raw[:])
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// invalidTradeDecoder matches every row and emits one soroswap trade the store can never hold: a zero-value
// canonical.Trade fails Trade.Validate before any SQL runs. It is the pre-SQL twin of a SQLSTATE 22/23 rejection and is
// deterministic for the row.
type invalidTradeDecoder struct{}

func (*invalidTradeDecoder) Name() string { return "invalid-trade" }

func (*invalidTradeDecoder) Matches(events.Event) bool { return true }

func (*invalidTradeDecoder) Decode(ev events.Event) ([]consumer.Event, error) {
	return []consumer.Event{soroswap.TradeEvent{
		Trade: canonical.Trade{Source: "soroswap", Ledger: ev.Ledger, TxHash: ev.TxHash},
	}}, nil
}
