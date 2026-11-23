package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/Stellar-Index/StellarIndex/internal/obs"
)

// fakeCursorWriter keeps the highest value each forward-only upsert would leave, as the store's WHERE guards do.
type fakeCursorWriter struct {
	cursor, account, classic uint32
	cursorSrc                string
}

func (f *fakeCursorWriter) UpsertCursor(_ context.Context, source, _ string, lastLedger uint32) error {
	f.cursorSrc = source
	f.cursor = max(f.cursor, lastLedger)
	return nil
}

func (f *fakeCursorWriter) UpsertAccountObserverWatermark(_ context.Context, l uint32) error {
	f.account = max(f.account, l)
	return nil
}

func (f *fakeCursorWriter) UpsertClassicObserverWatermark(_ context.Context, l uint32) error {
	f.classic = max(f.classic, l)
	return nil
}

type fixedMarks struct{ processed, durable uint32 }

func (m *fixedMarks) Processed() uint32      { return m.processed }
func (m *fixedMarks) DurableThrough() uint32 { return m.durable }

// Ledgers 101..105 are processed but a row of 103 has not landed: the cursor and both observer watermarks must stop at
// 102, or a crash now resumes at 106 and 103's row is never written.
func TestDurableCursor_CommitsDurableLedgerNotProcessed(t *testing.T) {
	store := &fakeCursorWriter{}
	marks := &fixedMarks{processed: 105, durable: 102}
	c := &durableCursor{
		store: store, marks: marks, logger: slog.New(slog.NewTextHandler(io.Discard, nil)), written: 100,
		accountObserverActive: true, classicObserverActive: true,
	}

	c.checkpoint(context.Background())

	if store.cursor != 102 || store.cursorSrc != cursorSource {
		t.Fatalf("cursor = (%q, %d), want (%q, 102): it passed a ledger whose rows have not landed", store.cursorSrc, store.cursor, cursorSource)
	}
	if store.account != 102 || store.classic != 102 {
		t.Fatalf("watermarks account=%d classic=%d, want 102 each", store.account, store.classic)
	}
	if got := testutil.ToFloat64(obs.CursorDurableLagLedgers); got != 3 {
		t.Fatalf("durable lag gauge = %v, want 3", got)
	}
	if got := testutil.ToFloat64(obs.CursorLastLedger.WithLabelValues(cursorSource)); got != 102 {
		t.Fatalf("cursor gauge = %v, want 102", got)
	}

	// The stuck row lands: the next checkpoint catches the cursor up to the processed ledger.
	marks.durable = 105
	c.checkpoint(context.Background())
	if store.cursor != 105 || store.account != 105 || store.classic != 105 {
		t.Fatalf("after landing: cursor=%d account=%d classic=%d, want 105", store.cursor, store.account, store.classic)
	}
	if got := testutil.ToFloat64(obs.CursorDurableLagLedgers); got != 0 {
		t.Fatalf("durable lag gauge = %v, want 0", got)
	}
}

// An inactive observer's watermark must not advance, whatever the cursor does.
func TestDurableCursor_InactiveObserversStayPut(t *testing.T) {
	store := &fakeCursorWriter{}
	c := &durableCursor{
		store: store, marks: &fixedMarks{processed: 7, durable: 7},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)), written: 5,
	}
	c.checkpoint(context.Background())
	if store.cursor != 7 || store.account != 0 || store.classic != 0 {
		t.Fatalf("cursor=%d account=%d classic=%d, want 7/0/0", store.cursor, store.account, store.classic)
	}
}
