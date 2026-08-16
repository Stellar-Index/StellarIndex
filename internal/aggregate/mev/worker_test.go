package mev

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

type fakeScanner struct {
	trades []canonical.Trade
	usd    []string
	err    error
}

func (f *fakeScanner) TradesForArbScan(_ context.Context, _ time.Time, _ int) ([]canonical.Trade, []string, error) {
	return f.trades, f.usd, f.err
}

type fakeSink struct {
	seen   map[string]bool
	events []StoredEvent
	err    error
}

func (s *fakeSink) InsertMEVEvent(_ context.Context, e StoredEvent) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	if s.seen[e.DedupKey] {
		return false, nil
	}
	s.seen[e.DedupKey] = true
	s.events = append(s.events, e)
	return true, nil
}

func arbTrades(t *testing.T) []canonical.Trade {
	return []canonical.Trade{
		trade(t, "soroswap", 1, "GARB", "native", usdc),
		trade(t, "phoenix", 2, "GARB", usdc, "native"),
	}
}

// RunOnce detects + persists once, and a re-run over the same window
// inserts nothing (dedup via the key).
func TestWorker_RunOnce_DetectsThenDedups(t *testing.T) {
	scanner := &fakeScanner{trades: arbTrades(t), usd: []string{"5.00", "5.00"}}
	sink := &fakeSink{}
	w := NewWorker(scanner, sink, WorkerConfig{})

	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC)
	detected, inserted, err := w.RunOnce(context.Background(), now)
	if err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if detected != 1 || inserted != 1 {
		t.Fatalf("first run: detected=%d inserted=%d, want 1/1", detected, inserted)
	}
	if len(sink.events) != 1 {
		t.Fatalf("want 1 stored event, got %d", len(sink.events))
	}

	// Evidence: detail carries the legs + notional.
	var d arbDetail
	if err := json.Unmarshal(sink.events[0].DetailJSON, &d); err != nil {
		t.Fatalf("detail unmarshal: %v", err)
	}
	if len(d.Legs) != 2 || d.NotionalUSD != "10.00" {
		t.Errorf("detail = %+v (want 2 legs, notional 10.00)", d)
	}
	if sink.events[0].NotionalUSD != "10.00" {
		t.Errorf("event notional = %q, want 10.00", sink.events[0].NotionalUSD)
	}

	// Re-run: same window → dedup → nothing new.
	detected2, inserted2, err := w.RunOnce(context.Background(), now)
	if err != nil {
		t.Fatalf("RunOnce 2: %v", err)
	}
	if detected2 != 1 || inserted2 != 0 {
		t.Fatalf("second run: detected=%d inserted=%d, want 1/0 (dedup)", detected2, inserted2)
	}
}

// tableSink models mev_events as a real table: inserts dedup on the key
// and prune deletes rows whose detected_at is before the cutoff. Used to
// prove the worker enforces a bounded retention window.
type tableSink struct {
	rows []storedRow
}

type storedRow struct {
	e  StoredEvent
	at time.Time
}

func (s *tableSink) InsertMEVEvent(_ context.Context, e StoredEvent) (bool, error) {
	for _, r := range s.rows {
		if r.e.DedupKey == e.DedupKey {
			return false, nil
		}
	}
	s.rows = append(s.rows, storedRow{e: e, at: e.Timestamp})
	return true, nil
}

func (s *tableSink) PruneMEVEvents(_ context.Context, before time.Time) (int64, error) {
	kept := make([]storedRow, 0, len(s.rows))
	var removed int64
	for _, r := range s.rows {
		if r.at.Before(before) {
			removed++
			continue
		}
		kept = append(kept, r)
	}
	s.rows = kept
	return removed, nil
}

func (s *tableSink) has(dedup string) bool {
	for _, r := range s.rows {
		if r.e.DedupKey == dedup {
			return true
		}
	}
	return false
}

// RunOnce prunes mev_events past the retention window so the table stays
// bounded: a row older than the window is deleted, the fresh detection
// from this tick survives.
func TestWorker_RunOnce_PrunesPastRetention(t *testing.T) {
	sink := &tableSink{}
	// Seed a stale event ~5.5 months old — well past the 90-day window.
	staleKey := "sandwich:stale:GATK"
	sink.rows = append(sink.rows, storedRow{
		e:  StoredEvent{DedupKey: staleKey},
		at: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	})

	scanner := &fakeScanner{trades: arbTrades(t), usd: []string{"5.00", "5.00"}}
	w := NewWorker(scanner, sink, WorkerConfig{Pruner: sink, Retention: 90 * 24 * time.Hour})

	now := time.Date(2026, 6, 18, 12, 0, 0, 0, time.UTC) // cutoff = 2026-03-20
	if _, _, err := w.RunOnce(context.Background(), now); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	if sink.has(staleKey) {
		t.Fatalf("stale event past retention was not pruned: %+v", sink.rows)
	}
	if len(sink.rows) == 0 {
		t.Fatal("this tick's detection should survive the prune, but the table is empty")
	}
}

// A scan error surfaces and writes nothing.
func TestWorker_RunOnce_ScanError(t *testing.T) {
	w := NewWorker(&fakeScanner{err: errors.New("boom")}, &fakeSink{}, WorkerConfig{})
	if _, _, err := w.RunOnce(context.Background(), time.Now()); err == nil {
		t.Fatal("want scan error, got nil")
	}
}
