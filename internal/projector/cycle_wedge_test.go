package projector

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"
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
// be driven cycle-by-cycle without Postgres. COR-11 / COR-01 are entirely
// about WHEN the cursor is allowed to move, so the cursor is the assertion
// surface here — not a mock's call log.
// ---------------------------------------------------------------------------

type fakeStore struct {
	mu sync.Mutex

	projectorCursor uint32 // last_ledger for ("projector", src)
	haveCursor      bool
	tipLedger       uint32 // last_ledger for ("ledgerstream", "")
	rows            []sorobanevents.Row
	upserts         int
}

func (f *fakeStore) GetCursor(_ context.Context, source, _ string) (timescale.Cursor, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch source {
	case "ledgerstream":
		return timescale.Cursor{Source: source, LastLedger: f.tipLedger}, nil
	case "projector":
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

func (f *fakeStore) cursor() uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.projectorCursor
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

// wedgeHarness wires a projector over the fake store with a sink whose error
// is chosen per event ledger.
type wedgeHarness struct {
	store   *fakeStore
	proj    *Projector
	src     Source
	window  uint32
	tracker poisonTracker
}

func newWedgeHarness(t *testing.T, name string, rows []sorobanevents.Row, tip uint32, sink func(ev consumer.Event) error) *wedgeHarness {
	t.Helper()
	store := &fakeStore{
		projectorCursor: 100,
		haveCursor:      true,
		tipLedger:       tip,
		rows:            rows,
	}
	p := &Projector{
		store:  store,
		logger: discardLog(),
		sink:   func(_ context.Context, ev consumer.Event) error { return sink(ev) },
	}
	return &wedgeHarness{
		store:  store,
		proj:   p,
		src:    Source{Name: name, Decoder: &ledgerEchoDecoder{}},
		window: BatchLimit,
	}
}

func (h *wedgeHarness) cycle() {
	h.cycleCtx(context.Background())
}

func (h *wedgeHarness) cycleCtx(ctx context.Context) {
	h.proj.cycleOneSource(ctx, h.src, &h.window, &h.tracker)
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

// ---------------------------------------------------------------------------
// COR-11: a DETERMINISTIC store validation error must not wedge the source.
// ---------------------------------------------------------------------------

// TestCycle_ValidationErrorDoesNotWedge pins COR-11 (audit-2026-07-23): a
// Validate-failing row — here an OracleUpdate rejected by canonical
// validation, exactly what Store.InsertOracleUpdate returns verbatim — is a
// DETERMINISTIC data fault. It carries no *pq.Error, so the old
// permanent/transient boolean classified it transient and held the cursor
// below its ledger FOREVER; under INV-4 there is no second writer, so the
// whole per-source projection stopped advancing from one bad row.
//
// The corrected behaviour: skip the row on the FIRST cycle, count it, and
// advance the cursor to the window's end.
func TestCycle_ValidationErrorDoesNotWedge(t *testing.T) {
	const source = "cor11-validation"
	rows := []sorobanevents.Row{lakeRow(101, 1), lakeRow(102, 2)}
	before := decodedCount(t, source, "sink_permanent")

	h := newWedgeHarness(t, source, rows, 105, func(ev consumer.Event) error {
		if ev.(ledgerEvent).ledger == 101 {
			// Verbatim shape of timescale.Store.InsertOracleUpdate's
			// `u.Validate()` return for a self-priced / non-positive-price
			// update published by a live oracle source.
			return fmt.Errorf("%w: price must be positive, got 0", canonical.ErrInvalidOracle)
		}
		return nil
	})

	h.cycle()

	if got := h.store.cursor(); got != 105 {
		t.Fatalf("cursor = %d, want 105 (the projector must advance past a deterministic validation failure, not wedge on it)", got)
	}
	if got := decodedCount(t, source, "sink_permanent") - before; got != 1 {
		t.Errorf("sink_permanent counter delta = %v, want 1 (the skipped row must be counted for investigation)", got)
	}
}

// TestCycle_ValidationErrorStillAdvancesAcrossCycles is the "stays unwedged"
// half: a second cycle over a still-poisoned range keeps making progress
// rather than re-stalling.
func TestCycle_ValidationErrorStillAdvancesAcrossCycles(t *testing.T) {
	const source = "cor11-validation-repeat"
	rows := []sorobanevents.Row{lakeRow(101, 1)}
	h := newWedgeHarness(t, source, rows, 105, func(consumer.Event) error {
		return fmt.Errorf("%w: tx_hash %q is not 64 hex chars", canonical.ErrInvalidOracle, "deadbeef")
	})

	h.cycle()
	if got := h.store.cursor(); got != 105 {
		t.Fatalf("cycle 1: cursor = %d, want 105", got)
	}

	// Tip moves on; the source must keep tracking it.
	h.store.mu.Lock()
	h.store.tipLedger = 205
	h.store.rows = append(h.store.rows, lakeRow(150, 3))
	h.store.mu.Unlock()

	h.cycle()
	if got := h.store.cursor(); got != 205 {
		t.Fatalf("cycle 2: cursor = %d, want 205 (a poisoned range must not re-wedge the source)", got)
	}
}

// ---------------------------------------------------------------------------
// COR-01: a negative SEP-41 amount rejected by the store's own pre-SQL
// validation (a plain fmt.Errorf, no sentinel, no *pq.Error) must not wedge
// the transfers projection forever.
// ---------------------------------------------------------------------------

// TestCycle_NegativeSEP41AmountQuarantinesAfterBudget pins COR-01: the store
// rejects a hostile/malformed negative transfer amount BEFORE the statement
// runs, so the error is un-classifiable from the projector's side. It is
// retried under a budget (transient faults get their chance) and then
// quarantined, so the sole-writer transfers projection resumes instead of
// stalling indefinitely.
func TestCycle_NegativeSEP41AmountQuarantinesAfterBudget(t *testing.T) {
	const source = "cor01-negative-amount"
	// Ledger 101 is poison; 102 commits fine — that success is the sink-health
	// proof that lets the short budget apply.
	rows := []sorobanevents.Row{lakeRow(101, 1), lakeRow(102, 2)}
	beforeQuarantined := decodedCount(t, source, "sink_quarantined")

	h := newWedgeHarness(t, source, rows, 105, func(ev consumer.Event) error {
		if ev.(ledgerEvent).ledger == 101 {
			return errors.New("timescale: InsertSEP41TransferBatch: row 0 transfer negative Amount -1")
		}
		return nil
	})

	// While the budget is unspent the cursor MUST hold below the failing
	// ledger — a genuinely transient fault deserves its retries, and dropping
	// the row immediately would be the C2-1 silent-loss bug.
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

// TestCycle_InfraErrorRetriesForever pins the anti-over-correction property: a
// positively-identified infrastructure fault (Postgres unreachable) is NEVER
// quarantined, however many cycles it lasts. Shedding live rows because the
// database is down is the C2-1 loss this projector exists to prevent.
func TestCycle_InfraErrorRetriesForever(t *testing.T) {
	const source = "infra-retry-forever"
	rows := []sorobanevents.Row{lakeRow(101, 1), lakeRow(102, 2)}
	beforeQuarantined := decodedCount(t, source, "sink_quarantined")

	h := newWedgeHarness(t, source, rows, 105, func(consumer.Event) error {
		return errors.New("dial tcp 127.0.0.1:5432: connect: connection refused")
	})

	for i := 0; i < QuarantineAfterCycles*3; i++ {
		h.cycle()
		if got := h.store.cursor(); got != 100 {
			t.Fatalf("cycle %d: cursor = %d, want 100 (an infra outage must hold the cursor, never skip rows)", i, got)
		}
	}
	if got := decodedCount(t, source, "sink_quarantined") - beforeQuarantined; got != 0 {
		t.Errorf("sink_quarantined delta = %v, want 0 (infra faults are never quarantined)", got)
	}
	if got := decodedCount(t, source, "sink_retry"); got == 0 {
		t.Error("sink_retry counter did not move; a held row must be visible as a retry")
	}
}

// TestCycle_DeadlockRetriesBeforeQuarantine pins that a classic retryable
// Postgres fault still gets its retries: a deadlock resolves on a later cycle
// and the row commits — nothing is skipped.
func TestCycle_DeadlockRetriesBeforeQuarantine(t *testing.T) {
	const source = "deadlock-retry"
	rows := []sorobanevents.Row{lakeRow(101, 1), lakeRow(102, 2)}
	beforeQuarantined := decodedCount(t, source, "sink_quarantined")

	beforeOK := decodedCount(t, source, "ok")
	failing := true
	h := newWedgeHarness(t, source, rows, 105, func(ev consumer.Event) error {
		if failing && ev.(ledgerEvent).ledger == 101 {
			return &pq.Error{Code: "40P01", Message: "deadlock detected"}
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
	// (C2-1), so the held cycles contribute nothing and the recovering cycle
	// projects both rows exactly once.
	if got := decodedCount(t, source, "ok") - beforeOK; got != 2 {
		t.Errorf("ok delta = %v, want 2 (both rows project once the deadlock clears)", got)
	}
}

// TestCycle_GlobalFailureDoesNotShedRows pins the guard against the obvious
// over-correction: when NOTHING commits (a schema drift / wedged sink — every
// row fails), the short budget must not apply, so the projector stalls
// visibly instead of quarantining the whole window.
func TestCycle_GlobalFailureDoesNotShedRows(t *testing.T) {
	const source = "global-failure-stalls"
	rows := []sorobanevents.Row{lakeRow(101, 1), lakeRow(102, 2), lakeRow(103, 3)}
	beforeQuarantined := decodedCount(t, source, "sink_quarantined")

	h := newWedgeHarness(t, source, rows, 105, func(consumer.Event) error {
		return &pq.Error{Code: "42703", Message: `column "amount" does not exist`}
	})

	for i := 0; i < QuarantineAfterCycles*2; i++ {
		h.cycle()
	}
	if got := h.store.cursor(); got != 100 {
		t.Fatalf("cursor = %d, want 100 (a global sink failure must stall visibly, never shed rows)", got)
	}
	if got := decodedCount(t, source, "sink_quarantined") - beforeQuarantined; got != 0 {
		t.Errorf("sink_quarantined delta = %v, want 0 (no health proof ⇒ no quarantine on the short budget)", got)
	}
}

// ---------------------------------------------------------------------------
// Sink-side adaptive shrink (v0.21.12, 2026-08-01 incident): the cycle-level
// half of the shrinkWindow unit test. A window whose CH scan FINISHES but
// whose per-event sink writes exhaust PerSourceTimeout ends the cycle with a
// dead cycleCtx and held transient rows; pre-fix the window pointer never
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
	rows := []sorobanevents.Row{lakeRow(101, 1), lakeRow(102, 2)}
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

// TestCycle_DecoderRegressionMarksRunDegradedNotOK pins DATA-6 / NS-2
// (audit-2026-08-14): when a decoder regression makes a whole class of valid
// events fail to decode, the projector still (correctly) advances the cursor
// past them — holding would re-wedge the sole-writer source on a deterministic
// failure (COR-11). What must NOT happen is the cycle reporting a clean "ok"
// run over those dropped rows. The cycle is marked runs_total{outcome=
// "decode_degraded"} instead, so runs_total no longer counts it clean and the
// per-source decode_error rate alert can distinguish a regression from
// scattered poison rows. Pre-fix the outcome was "ok" and the loss was
// invisible at the run level.
func TestCycle_DecoderRegressionMarksRunDegradedNotOK(t *testing.T) {
	const source = "data6-decode-regression"
	rows := []sorobanevents.Row{lakeRow(101, 1), lakeRow(102, 2)}

	beforeOK := runsCount(t, source, "ok")
	beforeDegraded := runsCount(t, source, "decode_degraded")
	beforeDecodeErr := decodedCount(t, source, "decode_error")

	store := &fakeStore{projectorCursor: 100, haveCursor: true, tipLedger: 105, rows: rows}
	p := &Projector{
		store:  store,
		logger: discardLog(),
		sink:   func(context.Context, consumer.Event) error { return nil },
	}
	src := Source{Name: source, Decoder: &decodeErrDecoder{}}
	window := uint32(BatchLimit)
	var tracker poisonTracker

	p.cycleOneSource(context.Background(), src, &window, &tracker)

	// The cursor still advances past the broken class (poison-row escape /
	// COR-11 — do NOT re-wedge a sole-writer source on a deterministic fault).
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

// TestCycle_HealthyRowsUnaffected is the no-regression baseline: with a sink
// that never fails, the cursor advances to the window end and everything
// counts as ok.
func TestCycle_HealthyRowsUnaffected(t *testing.T) {
	const source = "healthy-baseline"
	rows := []sorobanevents.Row{lakeRow(101, 1), lakeRow(102, 2)}
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
// duplicate guard (2026-08-11 stake-buffer investigation): the lake is
// an append log read without FINAL, so re-ingested duplicate rows reach
// the cycle consecutively. Stateless decoders + keyed sinks absorb
// that, but BUFFERED decoders (phoenix's multi-event correlation)
// corrupt: a duplicate re-opens a completed group and cross-assigns
// fields between legs. Exact-identity re-deliveries must be decoded
// exactly once.
func TestCycle_AdjacentDuplicateRowsDecodeOnce(t *testing.T) {
	// Three copies of one row followed by a distinct second row.
	dup := lakeRow(101, 1)
	rows := []sorobanevents.Row{dup, dup, dup, lakeRow(102, 2)}

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
