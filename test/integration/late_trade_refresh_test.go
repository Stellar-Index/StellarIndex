//go:build integration

package integration_test

import (
	"context"
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

// TestLateTradeRefresh_MaterialisesTradeOlderThanPolicyLookback proves the
// live write path (PersistEvents + LateTradeRefresher, as the indexer wires
// them) puts a trade written ~2h late into prices_1m with no manual
// refresh_continuous_aggregate. prices_1m's policy only reaches back 15
// minutes, so before the refresher such a row was never materialised.
func TestLateTradeRefresh_MaterialisesTradeOlderThanPolicyLookback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn) // policy jobs stay defined but unscheduled

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	quote := c.NativeAsset()
	mkPair := func(code string) c.Pair {
		a, err := c.NewClassicAsset(code, priceableIssuer)
		if err != nil {
			t.Fatalf("NewClassicAsset: %v", err)
		}
		p, err := c.NewPair(a, quote)
		if err != nil {
			t.Fatalf("NewPair: %v", err)
		}
		return p
	}
	latePair, onTimePair := mkPair("LATE2492"), mkPair("ONTIME2492")

	lateTS := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	bucket := lateTS.Truncate(time.Minute)
	mk := func(p c.Pair, ts time.Time, hash string) sdex.TradeEvent {
		return sdex.TradeEvent{Trade: c.Trade{
			Source:      "test-late-refresh",
			Ledger:      71_000_000,
			TxHash:      hash,
			Timestamp:   ts,
			Pair:        p,
			BaseAmount:  c.NewAmount(big.NewInt(1_000_000_000)),
			QuoteAmount: c.NewAmount(big.NewInt(12_000_000)),
		}}
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	late := pipeline.NewLateTradeRefresher(store, pipeline.LateTradeRefresherOptions{Logger: logger})
	runCtx, stop := context.WithCancel(ctx)
	runDone := make(chan struct{})
	go func() { defer close(runDone); late.Run(runCtx) }()

	in := make(chan consumer.Event, 2)
	in <- mk(onTimePair, time.Now().UTC().Truncate(time.Second), "00000000000000000000000000000000000000000000000000000000000a0001")
	in <- mk(latePair, lateTS, "00000000000000000000000000000000000000000000000000000000000a0002")
	sinkDone := make(chan struct{})
	go func() {
		defer close(sinkDone)
		pipeline.PersistEvents(ctx, logger, store, in, pipeline.SinkModeAll, late)
	}()
	t.Cleanup(func() {
		close(in)
		<-sinkDone
		stop()
		<-runDone
	})

	// The refresher debounces for a minute before its first flush.
	const q = `SELECT vwap::text, volume::text, trade_count FROM prices_1m
	            WHERE base_asset = $1 AND quote_asset = $2 AND bucket = $3`
	var vwap, volume string
	var count int
	deadline := time.Now().Add(90 * time.Second)
	for {
		err := store.DB().QueryRowContext(ctx, q, latePair.Base.String(), latePair.Quote.String(), bucket).Scan(&vwap, &volume, &count)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no prices_1m row for the late trade after 90s (last err: %v)", err)
		}
		time.Sleep(2 * time.Second)
	}

	wantVWAP := new(big.Rat).SetFrac64(12_000_000, 1_000_000_000)
	gotVWAP, ok := new(big.Rat).SetString(vwap)
	if !ok || gotVWAP.Cmp(wantVWAP) != 0 {
		t.Errorf("vwap = %s, want exactly 0.012", vwap)
	}
	if volume != "1000000000" {
		t.Errorf("volume = %s, want 1000000000", volume)
	}
	if count != 1 {
		t.Errorf("trade_count = %d, want 1", count)
	}

	// The on-time trade is inside the policy's reach, so the refresher
	// must leave it to the policy (unscheduled here): nothing materialised.
	var onTimeRows int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT count(*) FROM prices_1m WHERE base_asset = $1`, onTimePair.Base.String()).Scan(&onTimeRows); err != nil {
		t.Fatalf("count on-time rows: %v", err)
	}
	if onTimeRows != 0 {
		t.Errorf("on-time trade has %d prices_1m rows; the refresher must not refresh it", onTimeRows)
	}
}
