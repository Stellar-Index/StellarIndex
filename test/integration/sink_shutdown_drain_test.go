//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

	c "github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/consumer"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/sdex"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestSinkShutdownDrain_PersistsAllInFlight is the C2-17 proof: a shutdown
// (parent ctx cancelled) with N events already in flight on the sink channel
// must persist ALL N — none dropped. The pre-fix persistWorker select could
// pick the shutdown arm over draining a buffered event, and the drain-timeout
// path counted-and-dropped the remainder instead of persisting it.
//
// This buffers N distinct trades, cancels the parent ctx, then runs
// PersistEvents against a REAL store and asserts every trade landed.
func TestSinkShutdownDrain_PersistsAllInFlight(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	usdc, err := c.NewClassicAsset("USDC", "GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN")
	if err != nil {
		t.Fatalf("NewClassicAsset: %v", err)
	}
	pair, err := c.NewPair(c.NativeAsset(), usdc)
	if err != nil {
		t.Fatalf("NewPair: %v", err)
	}

	const n = 200
	ts := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)

	// Buffer all N trades on the channel BEFORE starting the sink, then cancel
	// the parent ctx, so every worker's first select sees ctx.Done ready with
	// the channel full — the exact race the fix must survive.
	in := make(chan consumer.Event, n)
	for i := 0; i < n; i++ {
		txHash := fmt.Sprintf("%064x", i) // 64 hex chars, distinct per trade
		in <- sdex.TradeEvent{Trade: c.Trade{
			Source:      "test-shutdown-drain",
			Ledger:      uint32(70_000_000 + i),
			TxHash:      txHash,
			OpIndex:     0,
			Timestamp:   ts,
			Pair:        pair,
			BaseAmount:  c.NewAmount(big.NewInt(1_000_000_000)),
			QuoteAmount: c.NewAmount(big.NewInt(12_000_000)),
		}}
	}

	sinkCtx, sinkCancel := context.WithCancel(ctx)
	sinkCancel() // shutdown requested with N events already buffered
	close(in)    // producer done — lets the blocking drain exit cleanly

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	done := make(chan struct{})
	go func() {
		defer close(done)
		pipeline.PersistEvents(sinkCtx, logger, store, in, pipeline.SinkModeAll)
	}()

	select {
	case <-done:
	case <-time.After(90 * time.Second):
		t.Fatal("PersistEvents did not return within 90s after ctx cancel + channel close")
	}

	got := countTrades(t, store, "test-shutdown-drain")
	if got != n {
		t.Fatalf("persisted trades = %d, want %d — shutdown dropped %d in-flight events (C2-17 regression)", got, n, n-got)
	}
}
