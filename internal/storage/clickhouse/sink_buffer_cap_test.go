package clickhouse

import (
	"context"
	"errors"
	"testing"
)

// TestSinkBufferCapBoundedDrop verifies the G12-01 bounded-drop: once the
// in-memory buffer reaches maxBufferLedgers (a sustained-CH-outage proxy, since
// the cap is only reached when flushes are failing), Add DROPS the incoming
// extract with ErrBufferFull instead of growing the buffer unbounded.
//
// This exercises the cap purely in-memory: with maxBufferLedgers set, Add
// returns ErrBufferFull BEFORE any auto-flush could be triggered (cap < the
// flushEvery threshold here), so no ClickHouse connection is needed.
func TestSinkBufferCapBoundedDrop(t *testing.T) {
	s := &Sink{flushEvery: 1_000_000} // never auto-flush during the test
	s.SetMaxBufferLedgers(3)

	ctx := context.Background()
	ext := func(seq uint32) LedgerExtract {
		return LedgerExtract{Ledger: LedgerRow{LedgerSeq: seq}}
	}

	// First 3 fit under the cap.
	for i := uint32(1); i <= 3; i++ {
		if err := s.Add(ctx, ext(i)); err != nil {
			t.Fatalf("Add(%d) under cap: unexpected error %v", i, err)
		}
	}
	if got := s.BufferedLedgers(); got != 3 {
		t.Fatalf("BufferedLedgers after 3 adds = %d, want 3", got)
	}

	// The 4th and 5th exceed the cap → bounded-drop, buffer stays at 3.
	for i := uint32(4); i <= 5; i++ {
		err := s.Add(ctx, ext(i))
		if !errors.Is(err, ErrBufferFull) {
			t.Fatalf("Add(%d) over cap = %v, want ErrBufferFull", i, err)
		}
	}
	if got := s.BufferedLedgers(); got != 3 {
		t.Fatalf("BufferedLedgers stayed bounded? = %d, want 3 (no unbounded growth)", got)
	}
}

// TestSinkBufferCapUnboundedByDefault verifies the cap is opt-in: a Sink with no
// SetMaxBufferLedgers (the backfill default) never returns ErrBufferFull, because
// backfill callers retry the SAME range on flush failure rather than streaming
// new ledgers on top.
func TestSinkBufferCapUnboundedByDefault(t *testing.T) {
	s := &Sink{flushEvery: 1_000_000}
	ctx := context.Background()
	for i := uint32(1); i <= 100; i++ {
		if err := s.Add(ctx, LedgerExtract{Ledger: LedgerRow{LedgerSeq: i}}); err != nil {
			t.Fatalf("Add(%d) with no cap: unexpected error %v", i, err)
		}
	}
	if got := s.BufferedLedgers(); got != 100 {
		t.Fatalf("BufferedLedgers = %d, want 100 (unbounded by default)", got)
	}
}
