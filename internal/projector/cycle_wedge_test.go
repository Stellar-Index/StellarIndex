package projector

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"slices"
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
)

// ---------------------------------------------------------------------------
// Harness: an in-memory eventStore so the cursor-durability state machine can
// be driven cycle-by-cycle without Postgres. These tests are entirely
// about WHEN the cursor is allowed to move, so the cursor is the assertion
// surface here — not a mock's call log.
// ---------------------------------------------------------------------------

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

	// dirtyBlock, when non-nil, makes ProjectionDirtyWindows block until
	// EITHER the channel is closed or the caller's ctx is done —
	// standing in for a query stuck behind a lock wait. dirtyDeadline
	// records whether the ctx it was handed carried one, and when
	// (the read must not run on the un-timeout'd root ctx).
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

// fakeEvents is an in-memory [eventSource]. StreamEvents and FirstEventLedger
// serve the events in [from, to] that pass the contract-id prefilter, as the
// lake reads do (topic filters are ignored); OpenLake hands back lake, or
// fails when it is nil.
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

// decodeErrDecoder matches every row and returns a decode error for all of
// them — the shape of a shipped decoder REGRESSION that breaks a whole class
// of valid events at once (the phoenix 5,161-orphaned-swap / I-L4 class), as
// opposed to the odd scattered poison row.
type decodeErrDecoder struct{}

func (*decodeErrDecoder) Name() string              { return "decode-err" }
func (*decodeErrDecoder) Matches(events.Event) bool { return true }
func (*decodeErrDecoder) Decode(events.Event) ([]consumer.Event, error) {
	return nil, errors.New("field-mapping regression: cannot decode 7-field swap")
}

// TestCycle_ValidationErrorDoesNotWedge pins that a Validate-failing row (an
// OracleUpdate rejected by canonical validation, which carries no
// *pgconn.PgError) is a deterministic data fault: it is skipped and counted on
// the first cycle, the cursor advances, and a later cycle over the still-poisoned
// range keeps making progress.
//
// Ledger 102 commits alongside the poison row: that is the sink-health proof the
// skip arm needs. A lone poison row with nothing else committing is the
// global-fault shape instead, pinned (holding) in poison_shed_health_proof_test.go.
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

// ---------------------------------------------------------------------------
// a negative SEP-41 amount rejected by the store's own pre-SQL
// validation (a plain fmt.Errorf, no sentinel, no *pgconn.PgError) must not wedge
// the transfers projection forever.
// ---------------------------------------------------------------------------

// TestCycle_NegativeSEP41AmountQuarantinesAfterBudget pins that the store
// rejects a hostile/malformed negative transfer amount BEFORE the statement
// runs, so the error is un-classifiable from the projector's side. It is
// retried under a budget (transient faults get their chance) and then
// quarantined, so the sole-writer transfers projection resumes instead of
// stalling indefinitely.
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

// ---------------------------------------------------------------------------
// The other half of the fix: do NOT over-correct into dropping recoverable
// work.
// ---------------------------------------------------------------------------

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

// ---------------------------------------------------------------------------
// Sink-side adaptive shrink: the cycle-level
// half of the shrinkWindow unit test. A window whose CH scan FINISHES but
// whose per-event sink writes exhaust PerSourceTimeout ends the cycle with a
// dead cycleCtx and held transient rows; without the fix the window pointer never
// moved, so the identical dense range was retried forever (aquarius reserves
// wedged 3.5h at ledger 63,488,687).
// ---------------------------------------------------------------------------

// TestCycle_SinkBudgetExhaustionShrinksWindowAndHoldsCursor drives
// cycleOneSource under an already-expired cycle budget with a sink that
// fast-fails every write with context.DeadlineExceeded — exactly what the
// wedge looked like from inside the projector. Each cycle must halve the
// adaptive window pointer, flooring at MinBatchLimit over repeated cycles,
// while the cursor holds (a deadline is dispositionRetry — never a skip, never
// a quarantine, never an advance-past-loss).
func TestCycle_SinkBudgetExhaustionShrinksWindowAndHoldsCursor(t *testing.T) {
	const source = "sink-budget-shrink"
	rows := []events.Event{lakeEvent(101, 1), lakeEvent(102, 2)}
	beforeQuarantined := decodedCount(t, source, "sink_quarantined")

	h := newWedgeHarness(t, source, rows, 2000, func(consumer.Event) error {
		// Every write fast-fails on the spent cycle budget, the shape the
		// sink sees once cycleCtx is dead mid-batch.
		return context.DeadlineExceeded
	})

	// The parent context is already past its deadline, so cycleCtx (derived
	// via WithTimeout in cycleOneSource) is born expired — the fake store
	// ignores ctx, so the scan still completes and only the sink "spends"
	// the budget, isolating the sink-side shrink arm from the stream-side
	// one (which needs the stream itself to return DeadlineExceeded).
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

// TestCycle_DecoderRegressionMarksRunDegradedNotOK pins that
// when a decoder regression makes a whole class of valid
// events fail to decode, the projector still (correctly) advances the cursor
// past them — holding would re-wedge the sole-writer source on a deterministic
// failure. What must NOT happen is the cycle reporting a clean "ok"
// run over those dropped rows. The cycle is marked runs_total{outcome=
// "decode_degraded"} instead, so runs_total no longer counts it clean and the
// per-source decode_error rate alert can distinguish a regression from
// scattered poison rows. An "ok" outcome would hide the loss at the run level.
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

// TestCycle_FlooredDeadlineStallSetsAndClearsWedgeGauge pins 9b (task #33 / W8
// recon): a source pinned at the MinBatchLimit window floor that keeps blowing
// the per-cycle deadline WITHOUT committing forward progress — a floor-sized
// range that stays over PerSourceTimeout (a dense + compressed chunk) — is a
// SILENT cursor wedge. It retries the identical range every cycle forever and
// the only prior signal was a flat stellarindex_projector_runs_total{outcome=
// "error"} rate (easily masked by an error storm from other sources). After
// WedgeCycles consecutive floor-stalls the projector must flip
// stellarindex_projector_wedged to 1; a single advancing cycle must clear it
// back to 0. The adaptive shrink logic itself is deliberately unchanged — this
// only makes the terminal stall observable/alertable.
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

// TestCycle_NonFlooredDeadlineDoesNotWedge is the anti-false-positive half: a
// source that keeps blowing the deadline but is STILL SHRINKING (window above
// the floor) is adapting, not wedged — the flag must stay 0 until the window
// bottoms out. Guards against paging on the healthy shrink ramp.
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

// TestCycle_AdjacentDuplicateRowsDecodeOnce pins the entry-point
// duplicate guard: the lake is
// an append log read without FINAL, so re-ingested duplicate rows reach
// the cycle consecutively. Stateless decoders + keyed sinks absorb
// that, but BUFFERED decoders (phoenix's multi-event correlation)
// corrupt: a duplicate re-opens a completed group and cross-assigns
// fields between legs. Exact-identity re-deliveries must be decoded
// exactly once.
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
