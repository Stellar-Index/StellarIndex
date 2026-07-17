//go:build integration

package integration_test

import (
	"context"
	"encoding/hex"
	"io"
	"log/slog"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/lib/pq"
	"github.com/stellar/go-stellar-sdk/strkey"

	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/events"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/projector"
	sep41_supply "github.com/Stellar-Index/StellarIndex/internal/sources/sep41_supply"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sorobanevents"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestProjectorSinkDurability_TransientFailureDoesNotAdvanceCursor is the
// proven-red test for audit-2026-07-16 C2-1 / D1 — the projector advancing
// its cursor past a silently-swallowed SINK write failure, which permanently
// drops the row for the sole-writer sep41 domain.
//
// It drives the REAL projector ([projector.New] + [projector.Run]) reading
// one seeded soroban_events row, with a sink that:
//   - FAILS the first cycle's write with a transient Postgres fault (a
//     deadlock, SQLSTATE 40P01 — exactly the class C2-1 describes), then
//   - SUCCEEDS on the retry, delegating to the production
//     [pipeline.HandleEvent] so the row lands for real.
//
// It asserts the two properties the fix must guarantee:
//
//	(a) after the transient failure the projector cursor did NOT advance past
//	    the failing ledger (so the event is not lost); and
//	(b) the next cycle re-reads that ledger and the row LANDS.
//
// RED on the unfixed code: revert cycleOneSource to advance the cursor to
// `toLedger` UNCONDITIONALLY (ignoring the sink error) — keeping the
// SinkFunc/HandleEvent error signatures — and assertion (a) fails (the cursor
// jumps past the failing ledger) and (b) fails (the idle next cycle never
// re-reads it, so the row is permanently lost). This mirrors the loss C2-1
// documents: `-resume` skips it, reconcile re-sums the equally-short table,
// and obs reports it as `ok`.
func TestProjectorSinkDurability_TransientFailureDoesNotAdvanceCursor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const (
		srcName = "sep41_supply"
		ledger  = uint32(50_000_010)
	)

	// Seed one soroban_events row at `ledger`. The fake decoder below ignores
	// its contents (it always emits one sep41 mint), so the row only needs to
	// Reconstruct cleanly — hence OpArgsXDR is nil (random op-args would fail
	// scval decode and soft-fail as a decode error instead of reaching the
	// sink).
	row := mkReconstructableRow(t, ledger)
	if err := store.InsertSorobanEventsBatch(ctx, []sorobanevents.Row{row}); err != nil {
		t.Fatalf("seed soroban_events: %v", err)
	}

	// Tip: the projector never scans past the live ledgerstream cursor. Set it
	// AT `ledger` so [ledger, ledger] is the exact scan window.
	if err := store.UpsertCursor(ctx, "ledgerstream", "", ledger); err != nil {
		t.Fatalf("seed ledgerstream cursor: %v", err)
	}
	// Projector cursor starts one BELOW `ledger`, so fromLedger = ledger and
	// the single row is in-window on the first cycle.
	if err := store.UpsertCursor(ctx, "projector", srcName, ledger-1); err != nil {
		t.Fatalf("seed projector cursor: %v", err)
	}

	// Decoder emits exactly one sep41 mint per matched row, keyed to the row's
	// ledger/tx so the write is a real, valid sep41_supply_events row.
	contractID := row.ContractID
	dec := &fakeSupplyDecoder{contractID: contractID}

	sink := &durabilitySink{
		store:    store,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		failNext: true, // fail the first write transiently
		called:   make(chan struct{}, 8),
	}

	reg := projector.Registry{Sources: []projector.Source{{Name: srcName, Decoder: dec}}}
	p := projector.New(store, reg, sink.handle, slog.New(slog.NewTextHandler(io.Discard, nil)))

	runCtx, runCancel := context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() {
		defer close(runDone)
		_ = p.Run(runCtx)
	}()
	t.Cleanup(func() {
		runCancel()
		select {
		case <-runDone:
		case <-time.After(30 * time.Second):
			t.Error("projector Run did not exit within 30s of cancel")
		}
	})

	readCursor := func() uint32 {
		c, err := store.GetCursor(ctx, "projector", srcName)
		if err != nil {
			t.Fatalf("read projector cursor: %v", err)
		}
		return c.LastLedger
	}
	countRows := func() int {
		var n int
		if err := store.DB().QueryRowContext(ctx,
			`SELECT count(*) FROM sep41_supply_events WHERE contract_id = $1`, contractID).Scan(&n); err != nil {
			t.Fatalf("count sep41_supply_events: %v", err)
		}
		return n
	}

	// ── Cycle 1: the transient sink failure ──────────────────────────────
	select {
	case <-sink.called:
	case <-time.After(30 * time.Second):
		t.Fatal("projector never invoked the sink on the first cycle")
	}
	// Let the cycle finish its post-sink cursor decision. On the UNFIXED code
	// the unconditional UpsertCursor(toLedger) runs synchronously right after
	// the sink returns, so a 500ms settle reliably catches the bad advance.
	time.Sleep(500 * time.Millisecond)

	if got := readCursor(); got != ledger-1 {
		t.Fatalf("after a TRANSIENT sink failure the cursor advanced to %d, want %d "+
			"(C2-1: the projector must NOT advance past a ledger whose write failed transiently — "+
			"the unfixed code jumps to toLedger and permanently drops the sep41 row)", got, ledger-1)
	}
	if n := countRows(); n != 0 {
		t.Fatalf("sep41_supply_events has %d rows after the failed write, want 0", n)
	}

	// ── Cycle 2+: the retry lands the row ────────────────────────────────
	sink.mu.Lock()
	sink.failNext = false // let the next write succeed
	sink.mu.Unlock()

	// The next cycle (Interval later) re-reads `ledger` and commits it.
	deadline := time.Now().Add(2*projectorSettle + 30*time.Second)
	for {
		if readCursor() == ledger {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("cursor never reached %d after the fault cleared — the row was NOT retried "+
				"(C2-1: a held cursor must re-read the failing ledger next cycle)", ledger)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if n := countRows(); n != 1 {
		t.Fatalf("sep41_supply_events has %d rows after the retry, want 1 (the dropped mint must land)", n)
	}
}

// projectorSettle pads the retry deadline by more than one projector Interval
// so the ticker-driven second cycle has time to run.
const projectorSettle = 5 * time.Second

// durabilitySink is the projector [projector.SinkFunc]: it fails the first
// write transiently (a deadlock), then delegates real writes to the
// production [pipeline.HandleEvent].
type durabilitySink struct {
	mu       sync.Mutex
	failNext bool
	store    *timescale.Store
	logger   *slog.Logger
	called   chan struct{}
}

func (s *durabilitySink) handle(ctx context.Context, ev consumer.Event) error {
	s.mu.Lock()
	fail := s.failNext
	s.mu.Unlock()
	select {
	case s.called <- struct{}{}:
	default:
	}
	if fail {
		// A transient Postgres fault mid-cycle (deadlock_detected, SQLSTATE
		// 40P01) — precisely the class C2-1 says the old sink swallowed.
		// timescale.IsPermanentDataError classifies it as transient, so the
		// projector must HOLD its cursor and retry rather than skip.
		return &pq.Error{Code: "40P01", Message: "deadlock detected (injected)"}
	}
	// Real production write path — exercises HandleEvent's new error return.
	return pipeline.HandleEvent(ctx, s.logger, s.store, ev)
}

// fakeSupplyDecoder matches every reconstructed row and emits exactly one
// sep41 mint keyed to the row, so the projector's sink writes a valid
// sep41_supply_events row through the real HandleEvent path.
type fakeSupplyDecoder struct{ contractID string }

func (fakeSupplyDecoder) Name() string              { return "sep41_supply" }
func (fakeSupplyDecoder) Matches(events.Event) bool { return true }
func (d *fakeSupplyDecoder) Decode(ev events.Event) ([]consumer.Event, error) {
	return []consumer.Event{sep41_supply.Event{
		ContractID:   d.contractID,
		Ledger:       ev.Ledger,
		TxHash:       ev.TxHash,
		OpIndex:      uint32(ev.OperationIndex), //nolint:gosec // test data, small
		EventIndex:   uint32(ev.EventIndex),     //nolint:gosec // test data, small
		ObservedAt:   time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
		Kind:         sep41_supply.SymbolMint,
		Amount:       big.NewInt(1000),
		Counterparty: "GCOUNTERPARTY00000000000000000000000000000000000000000000",
	}}, nil
}

// mkReconstructableRow builds one soroban_events row that
// sorobanevents.Reconstruct accepts (valid contract strkey, 32-byte tx hash,
// non-empty topic-0 + body) and that carries NO op-args (so Reconstruct does
// not attempt to scval-decode random bytes).
func mkReconstructableRow(t *testing.T, ledger uint32) sorobanevents.Row {
	t.Helper()
	var cid [32]byte
	cid[0] = 0x11
	cid[1] = 0xAA
	cstrk, err := strkey.Encode(strkey.VersionByteContract, cid[:])
	if err != nil {
		t.Fatalf("strkey.Encode: %v", err)
	}
	txh := make([]byte, 32)
	for i := range txh {
		txh[i] = 0x22
	}
	_ = hex.EncodeToString(txh) // Reconstruct hex-encodes TxHash into ev.TxHash
	return sorobanevents.Row{
		Ledger:          ledger,
		LedgerCloseTime: time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC),
		TxHash:          txh,
		OpIndex:         0,
		EventIndex:      0,
		ContractID:      cstrk,
		ContractIDHex:   cid[:],
		TopicCount:      1,
		Topic0Sym:       "mint",
		Topic0XDR:       []byte{0x00, 0x01, 0x02, 0x03},
		Topic1XDR:       nil,
		Topic2XDR:       nil,
		Topic3XDR:       nil,
		BodyXDR:         []byte{0x04, 0x05, 0x06, 0x07},
		OpArgsXDR:       nil,
	}
}
