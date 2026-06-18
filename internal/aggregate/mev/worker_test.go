package mev

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/StellarIndex/stellar-index/internal/canonical"
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

// A scan error surfaces and writes nothing.
func TestWorker_RunOnce_ScanError(t *testing.T) {
	w := NewWorker(&fakeScanner{err: errors.New("boom")}, &fakeSink{}, WorkerConfig{})
	if _, _, err := w.RunOnce(context.Background(), time.Now()); err == nil {
		t.Fatal("want scan error, got nil")
	}
}
