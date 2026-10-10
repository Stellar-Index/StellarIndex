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
// global-fault shape instead, pinned (holding) elsewhere in this file.
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

// TestCycle_ReplayRewindMidCycleIsNotClobbered is the unit-level
// check: projector-replay's rewind lands while a cycle is in flight (here:
// from inside that cycle's own sink call, which is as mid-cycle as it
// gets). The cycle's commit is derived from the cursor it read at its
// start; it must NOT put the cursor back at tip, and the next cycle must
// re-walk the rewound range.
//
// RED on the unfixed code (commit = never-regress upsert of commitTo): the
// cursor reads 120 after cycle 1 and ledger 60 is never sunk again.
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

// TestCycle_SpentBudgetStillCommitsDurableProgress pins that a cycle
// whose sink writes commit ledger 101 and then exhaust PerSourceTimeout (102
// fails with DeadlineExceeded) must still advance the cursor to 101. The
// write must not run on the expired cycle context, or it fails and the
// committed work is re-projected, identically, on every later cycle.
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

// TestCycle_HeldRowWarningIsThrottled pins the log budget for a row the
// projector keeps retrying: an infra fault holds the cursor every cycle for
// as long as it lasts, and the per-row warning must fire on the first
// failing cycle and every heldRowLogEvery-th after — not once per cycle,
// which turned a sustained outage into one identical line per row per
// Interval and buried the ERROR lines an operator needs.
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

// TestCycle_LogsLastSeenLedger pins: the cycle-summary "projector
// cycle" log line must actually carry last_seen_ledger, matching the
// highest ledger this cycle's stream callback observed — the comment above
// the commit-watermark logic claims "lastSeenLedger is only logged", but
// until this the variable was tracked and never referenced by any log call.
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

// TestCycle_PrefilterTracksLiveFactorySeededGate pins: sushiswap_v3's
// gate grows LIVE from the factory's pool_created events, so the contract-id
// prefilter must follow it. A pool created at ledger 101 and traded at 102
// — both inside one projector window — must have its swap projected before
// the cursor passes 102. A boot-time snapshot filtered the swap out ahead of
// Matches and advanced the cursor over it.
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

// TestCycle_PrefilterTracksSpectraSecondHop is the two-hop form of the test
// above. Spectra's gate grows twice in a market's creation transaction: the
// factory's pt_deployed admits the PT, then the PT's yt_deployed admits the
// YT. The PT's yt_deployed is emitted BEFORE the factory's pt_deployed, so
// the first read cannot see it; each widening must hold the window for a
// re-read, or the YT and its transfer one ledger later are lost.
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

// TestCycle_GlobalPermanentFaultShedsAtMostOneRowPerCycle is the
// regression. A bad migration makes the sink reject EVERY row of the window
// with a class-23 error. The projector must not answer that by dropping the
// whole backlog: it holds every row below the cursor (a visible stall — rising
// lag, runs_total{outcome="sink_retry"}) for as long as the cycle cannot prove
// the sink is otherwise healthy, then bleeds at most one row per cycle, and
// still drains rather than wedging.
//
// Without a cap the skip arm runs inline per row: the first cycle
// would count all five, forget them and let the cursor advance to the window's end
// (110), so an operator's next look showed a caught-up source with five rows
// missing from the served tier and nothing but an ERROR log to say so.
//
// The cap alone (rail 1) still shed the first row on cycle ONE, ~1 hour before
// anything could page. This case now pins BOTH rails: no row leaves the served
// tier until the no-progress budget is spent, because a window in which
// nothing at all committed is the global-fault shape by construction. See
// TestCycle_PoisonRowWithSinkHealthProofShedsOnCycleOne for the single-row form.
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

// TestCycle_PoisonOutputOnAHeldRowDoesNotResetItsRetryBudget guards the
// interaction the shed cap introduces, rather than the defect it fixes: a
// multi-output row can carry BOTH a permanently dropped output and a
// fault that holds the cursor. Such a row is retried whole, poison outputs
// included, so it must not also be a shed candidate — shedding it would forget
// the row identity, and the consecutive-cycle count the quarantine budget is
// made of would restart every cycle, so the row could never be quarantined and
// the sole-writer source would wedge forever.
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

// TestCycle_PoisonShedTakesTheLowestLedgerAndKeepsGoodRowsFlowing pins the
// other half of the cap: it is the LOWEST-ledger poison row that is shed, so
// the cursor watermark still advances in ledger order, and the valid rows
// between two poison rows commit on the way past.
//
// Rows: 101 poison, 102 valid, 103 poison, 104 valid. The cycle may shed 101
// only, which puts the watermark at 102 (the last fully-committed ledger below
// the row still held) — not at 110, which is what shedding both poison rows in
// one pass produced.
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

// TestCycle_PoisonRowWithNoSinkHealthProofHoldsUntilTheNoProgressBudget is the
// second rail for the shed cap.
//
// The rail: the per-cycle cap ([PermanentSkipPerCycle]) bounds the RATE at
// which a global class-22/23 fault sheds rows, but not the FACT of it. The
// discriminator between "this row's values are bad" and "the sink rejects
// everything right now" is the same proof the quarantine arm already takes —
// `madeProgress` (some OTHER event of this cycle durably committed). Without
// it, [QuarantineAfterCyclesNoProgress] is the budget: hold ~1 hour of cycles,
// far longer than the lag / sink_retry alerts take to fire, and only then give
// up one row per cycle.
//
// Two existing cases pinned the superseded contract — a lone poison row sheds
// on cycle one with no health proof at all — and both were moved rather than
// exempted:
//
//   - cycle_wedge_test.go, TestCycle_ValidationErrorStillAdvancesAcrossCycles
//     now supplies the health proof it always meant to (a second row that
//     commits) and keeps asserting the anti-wedge property;
//   - TestCycle_DroppedTradeIsNotReportedOK now
//     asserts the hold on cycle one AND the eventual self-heal, so
//     "a drop must not wedge a sole-writer source" survives intact.
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

	// The stall is not silence. outcome="sink_permanent" counts the sink's
	// REJECTION, on every cycle that re-reads the row, so it climbs through
	// the whole hold — which is what gives
	// stellarindex_projector_row_dropped_permanent something to fire on an
	// hour BEFORE the budget lets anything leave the served tier. A counter
	// that only moved on the shed would make the operator's warning window
	// and the alert's first sample the same instant.
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

// TestCycle_PoisonRowWithSinkHealthProofShedsOnCycleOne is the other side of
// the same rail, and the reason the proof is a DISCRIMINATOR rather than a
// blanket delay: a scattered poison row sits beside rows that commit, so the
// proof is present and the row costs exactly one cycle — the behaviour
// this arm exists for is unchanged.
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

// In soroban_events mode the ledgerstream cursor (the scan bound) advances
// when a ledger's raw rows are enqueued, before they commit. A cycle that
// scans to the tip while the tip's rows are still buffered finds nothing
// there, commits the cursor past them, and never reads them again. The
// barrier must land those rows before the scan.
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

// TestCycle_MalformedRowIsLoggedWithIdentity pins that a landing-zone row
// Reconstruct rejects is counted as reconstruct_error (not decode_error), marks
// the cycle decode_degraded, and is logged with its full identity and error,
// throttled to the first and every reconstructErrLogEvery-th failure.
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

// TestCycle_DroppedOutputDoesNotAbortTheRowsOtherOutputs pins the regression
// an early attempt at the drop-reporting fix introduced. Once a dropped trade is REPORTED
// (non-nil) instead of folded into nil, a loop that stops at the first sink
// error never offers the row's remaining outputs to the sink — and because a
// permanent fault is SKIPPED, the cursor advances past the row, so those valid
// outputs are lost from the served tier under one sink_permanent count.
//
// Corrected: a permanently dropped output is counted and the loop goes on, so
// [poison trade, valid output] sinks the valid output (ok=1), counts the drop
// (sink_permanent=1) and advances the cursor.
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

// TestCycle_RetryableFaultAfterADropStillHoldsTheRow — continuing past a
// permanent drop must not weaken the no-silent-loss rule: a transient fault on a LATER output of
// the same row still stops the row and holds the cursor below its ledger, and
// the output after the transient fault is NOT offered (the whole row is
// re-read next cycle; the idempotent sinks absorb the repeats).
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

// TestCycle_SinkPanicIsShedLikeAPermanentFault pins the projector side of
// the panic contract: pipeline.HandleEvent recovers a sink panic and returns
// it wrapped in pipeline.ErrSinkPanic, which the sink's own classifier drops.
// The projector must read it the same way — with a sink-health proof the row
// is shed on cycle one under the shed cap and counted as sink_permanent —
// rather than as an unclassified fault that re-runs the panicking decode
// every cycle for the whole quarantine budget.
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

// TestCycle_DroppedTradeIsNotReportedOK pins the outcome end to end, through the
// PRODUCTION sink: cmd/stellarindex-indexer binds the projector's SinkFunc to
// pipeline.HandleEvent, and so does this test (a nil store is safe — Validate
// rejects the trade before the store is touched).
//
// persistTrade must not return nil for a permanently dropped trade, which is
// also what a landed trade returns; processEventSafely would count it emitted and
// the cycle would publish it under outcome="ok" — the label whose own comment
// promises "only events that DURABLY committed". The row is in neither the
// served tier nor any loss counter the projector owns.
//
// Corrected: the drop is labelled sink_permanent, never ok — and the source
// STILL self-heals, because a deterministic fault must not wedge a sole-writer
// source.
//
// The cursor advance is deferred, not dropped. This row is the only
// one in its window, so its cycle commits nothing else and cannot tell "this
// trade is malformed" from "the sink rejects every trade right now" — the
// shape a bad migration has. Advancing on cycle one is exactly the unbounded
// shed that finding exists to stop, so the cursor now HOLDS for
// QuarantineAfterCyclesNoProgress cycles first (a visible stall: rising lag,
// runs_total{outcome="sink_retry"}) and only then sheds. Both halves are
// asserted below; the anti-wedge property is the second one.
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

// A lake hole at ledger 101 stalls the watermark at 100 while ledgerstream
// holds through 200. The source is held, not caught up: it must be counted as
// watermark_held and report the real 100-ledger lag, or a stalled watermark
// is indistinguishable from a healthy idle source and no lag alert can fire.
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

// notNullViolation is the verbatim shape a migration that adds a NOT NULL
// column (or a CHECK the live rows violate) puts on EVERY insert into the
// affected hypertable: SQLSTATE 23502, class 23, which
// timescale.IsPermanentDataError reports as a POSITIVE permanent data fault.
//
// That is the trap: the classifier's verdict is "these VALUES
// are bad", but the same SQLSTATE arrives from a fault that is GLOBAL rather
// than row-local, in which case every row of the window is "poison" at once.
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

// invalidTradeDecoder matches every row and emits ONE soroswap trade the
// store can never hold: a zero-value canonical.Trade fails Trade.Validate
// inside Store.InsertTrade before any SQL runs — the pre-SQL twin of a
// SQLSTATE 22/23 rejection, and deterministic for the row.
type invalidTradeDecoder struct{}

func (*invalidTradeDecoder) Name() string { return "invalid-trade" }

func (*invalidTradeDecoder) Matches(events.Event) bool { return true }

func (*invalidTradeDecoder) Decode(ev events.Event) ([]consumer.Event, error) {
	return []consumer.Event{soroswap.TradeEvent{
		Trade: canonical.Trade{Source: "soroswap", Ledger: ev.Ledger, TxHash: ev.TxHash},
	}}, nil
}
