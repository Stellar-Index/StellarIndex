// Copyright (c) 2026 Stellar Index contributors.
// SPDX-License-Identifier: Apache-2.0

package v1

import (
	"context"
	"io"
	"log/slog"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/api/streaming"
	"github.com/Stellar-Index/StellarIndex/internal/canonical"
)

// blockingTickHistory holds every TradesInRange call until release is
// closed, recording the peak number of calls in flight at once. A
// compute reads its pairs sequentially, so that peak is the number of
// concurrent computeTip calls.
type blockingTickHistory struct {
	HistoryReader // nil — only TradesInRange is exercised
	release       chan struct{}
	inFlight      atomic.Int32
	peak          atomic.Int32
}

func (h *blockingTickHistory) TradesInRange(ctx context.Context, pair canonical.Pair, _, to time.Time, _ int) ([]canonical.Trade, error) {
	n := h.inFlight.Add(1)
	defer h.inFlight.Add(-1)
	for {
		p := h.peak.Load()
		if n <= p || h.peak.CompareAndSwap(p, n) {
			break
		}
	}
	select {
	case <-h.release:
		return []canonical.Trade{{
			Source:      "kraken",
			Pair:        pair,
			BaseAmount:  canonical.NewAmount(big.NewInt(1_000_0000)),
			QuoteAmount: canonical.NewAmount(big.NewInt(200_0000)),
			Timestamp:   to.Add(-time.Second),
		}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// TestTipTicks_ConcurrencyIsBoundedOnTheRealTickPath drives both
// producer shapes' real tick loops with more producers than tick slots
// against a reader that never answers, and requires that no more
// computes than slots are ever in flight. The count and rate budgets do
// not bound this: every producer can fire in the same instant.
func TestTipTicks_ConcurrencyIsBoundedOnTheRealTickPath(t *testing.T) {
	const slots, producers = 2, 6
	btc, _ := canonical.ParseAsset("crypto:BTC")
	usd, _ := canonical.ParseAsset("fiat:USD")

	shapes := map[string]func(ctx context.Context, s *Server, i int){
		"shared": func(ctx context.Context, s *Server, i int) {
			key := tipProducerKey{asset: btc.String(), quote: usd.String(), window: 60 - i}
			s.runSharedTipProducer(ctx, key, btc, usd, 60-i)
		},
		"per_connection": func(ctx context.Context, s *Server, _ int) {
			var gen streaming.Generator
			ch := make(chan streaming.Event, 4)
			s.runTipStreamProducer(ctx, ch, &gen, btc, usd, 1, streaming.Event{})
		},
	}
	for name, run := range shapes {
		t.Run(name, func(t *testing.T) {
			h := &blockingTickHistory{release: make(chan struct{})}
			s := New(Options{
				History: h,
				Prices:  noPriceReader{},
				Hub:     streaming.NewHub(0),
				Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			s.tipProducers.tickConcurrency = slots

			ctx, cancel := context.WithCancel(context.Background())
			var wg sync.WaitGroup
			for i := range producers {
				wg.Add(1)
				go func() { defer wg.Done(); run(ctx, s, i) }()
			}
			t.Cleanup(func() { cancel(); wg.Wait() })

			deadline := time.Now().Add(5 * time.Second)
			for h.inFlight.Load() < slots && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if got := h.inFlight.Load(); got < slots {
				t.Fatalf("only %d computes started; the tick path never reached the reader", got)
			}
			// Every producer has fired by now; give the unbounded shape the
			// time to pile past the slots.
			time.Sleep(300 * time.Millisecond)
			close(h.release)
			if peak := h.peak.Load(); peak > slots {
				t.Fatalf("peak concurrent tick computes = %d across %d producers, want <= %d (tick slots)",
					peak, producers, slots)
			}
		})
	}
}

// panicOnceHistory panics on its first TradesInRange call and answers
// with no trades after.
type panicOnceHistory struct {
	HistoryReader
	calls atomic.Int32
}

func (h *panicOnceHistory) TradesInRange(context.Context, canonical.Pair, time.Time, time.Time, int) ([]canonical.Trade, error) {
	if h.calls.Add(1) == 1 {
		panic("tip tick compute panicked")
	}
	return nil, nil
}

// TestTipTicks_SlotIsFreedWhenTheComputePanics pins that a tick slot is
// released when the compute panics and the producer's recover absorbs
// it. With one slot, a leaked slot would starve every later tick.
func TestTipTicks_SlotIsFreedWhenTheComputePanics(t *testing.T) {
	btc, _ := canonical.ParseAsset("crypto:BTC")
	usd, _ := canonical.ParseAsset("fiat:USD")
	h := &panicOnceHistory{}
	s := New(Options{
		History: h,
		Prices:  noPriceReader{},
		Hub:     streaming.NewHub(0),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	s.tipProducers.tickConcurrency = 1

	key := tipProducerKey{asset: btc.String(), quote: usd.String(), window: 60}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.runSharedTipProducer(context.Background(), key, btc, usd, 60)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("producer did not exit on its panicking compute")
	}
	if got := h.calls.Load(); got != 1 {
		t.Fatalf("TradesInRange calls = %d, want the 1 that panicked", got)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, _, err := s.computeTipGated(ctx, btc, usd, 5)
	if ctx.Err() != nil {
		t.Fatalf("next tick could not get the only slot (err = %v): the panicked compute leaked it", err)
	}
}

// noPriceReader answers every read with ErrPriceNotFound.
type noPriceReader struct{}

func (noPriceReader) LatestPrice(context.Context, canonical.Asset, canonical.Asset) (PriceSnapshot, []string, bool, error) {
	return PriceSnapshot{}, nil, false, ErrPriceNotFound
}

func (noPriceReader) RecentClosedSnapshots(context.Context, canonical.Asset, canonical.Asset, int) ([]PriceSnapshot, error) {
	return nil, nil
}
