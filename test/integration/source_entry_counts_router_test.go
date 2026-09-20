//go:build integration

package integration_test

import (
	"context"
	"io"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/canonical"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/sources/soroswap_router"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestSourceEntryCounts_RouterBumpFollowsTheLandedInsert is the DB-backed
// twin of pipeline.TestHandleEvent_EntryCountFollowsTheLandedInsert: the
// soroswap-router `entries` tally moves only when a row LANDS. A row the
// store rejects contributes nothing, and a landed row contributes exactly
// one — the bump used to run ahead of the insert, so a rejected row (or
// every REL-08 infra-retry attempt of one event) inflated the tally (Q062).
func TestSourceEntryCounts_RouterBumpFollowsTheLandedInsert(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := startTimescale(t, ctx)
	applyMigrations(t, dsn)

	store, err := timescale.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("store open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	count := func() int64 {
		m, err := store.SourceEntryCounts(ctx)
		if err != nil {
			t.Fatalf("SourceEntryCounts: %v", err)
		}
		return m[soroswap_router.SourceName]
	}

	const (
		pathHopA  = "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA1"
		pathHopB  = "CAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA2"
		recipient = "GAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA3"
	)
	swap := func(txHash string, path []string) soroswap_router.Event {
		return soroswap_router.Event{Swap: soroswap_router.RouterSwap{
			Source:     soroswap_router.SourceName,
			Ledger:     1000,
			ClosedAt:   time.Now().UTC().Truncate(time.Second),
			TxHash:     txHash,
			OpIndex:    0,
			OpSource:   recipient,
			TxSource:   recipient,
			ContractID: soroswap_router.MainnetRouter,
			Function:   soroswap_router.FnSwapExactTokensForTokens,
			Recipient:  recipient,
			Path:       path,
			AmountIn:   canonical.NewAmount(big.NewInt(1_000_000)),
			AmountOut:  canonical.NewAmount(big.NewInt(990_000)),
			CallPath:   []string{soroswap_router.MainnetRouter},
			CallDepth:  0,
			CallKind:   "top_level",
		}}
	}

	// A single-hop path is rejected by the writer before any SQL runs
	// (Path must have >= 2 hops): no row, so no entry.
	if err := pipeline.HandleEvent(ctx, logger, store, swap("aa01", []string{pathHopA})); err == nil {
		t.Fatal("HandleEvent landed a single-hop router swap; the store must reject it")
	}
	if got := count(); got != 0 {
		t.Fatalf("after a rejected row: soroswap-router entries = %d, want 0 (the bump must follow the landed insert)", got)
	}

	// A landed row counts exactly once.
	if err := pipeline.HandleEvent(ctx, logger, store, swap("aa02", []string{pathHopA, pathHopB})); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if got := count(); got != 1 {
		t.Fatalf("after a landed row: soroswap-router entries = %d, want 1", got)
	}
}
