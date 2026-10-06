// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package ingest

import (
	"context"
	"testing"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// TestPreviewPathsNeverReachTheStore is the behavioural half: the gate is
// only worth its flag if the preview branch actually writes nothing. Each
// of these is handed a NIL *timescale.Store, so any call through to the
// datastore panics on the nil dereference instead of quietly passing.
func TestPreviewPathsNeverReachTheStore(t *testing.T) {
	ctx := context.Background()
	var store *timescale.Store // deliberately nil — see the doc above

	t.Run("tag-signer", func(t *testing.T) {
		tags := []timescale.SignerTag{
			{Ledger: 1, TxHash: "a", Signer: "GA"},
			{Ledger: 2, TxHash: "b", Signer: "GB"},
		}
		got, err := tagSignerWindow(ctx, store, false, time.Unix(0, 0), time.Unix(1, 0), tags)
		if err != nil {
			t.Fatalf("preview returned %v, want nil", err)
		}
		if got != int64(len(tags)) {
			t.Errorf("preview reported %d trades, want the %d candidate tags it would apply", got, len(tags))
		}
	})

	t.Run("census-backfill", func(t *testing.T) {
		if err := upsertCensusRow(ctx, store, false, timescale.LedgerIngestRow{LedgerSeq: 42}); err != nil {
			t.Errorf("preview returned %v, want nil", err)
		}
	})

	t.Run("backfill-router", func(t *testing.T) {
		if err := insertRouterSwap(ctx, store, false, timescale.SoroswapRouterSwap{Ledger: 42}); err != nil {
			t.Errorf("preview returned %v, want nil", err)
		}
	})

	t.Run("seed-protocol-contracts", func(t *testing.T) {
		// A curated-set source: its whole write is the in-code set, so
		// the preview must report the set size and upsert none of it.
		var curated string
		var want int
		for _, name := range pipeline.GatedSourceNames() {
			meta, ok := pipeline.GatedMetaFor(name)
			if ok && len(meta.Factories) == 0 && len(meta.CuratedSet) > 0 {
				curated, want = name, len(meta.CuratedSet)
				break
			}
		}
		if curated == "" {
			t.Skip("no curated-only gated source in the registry")
		}
		got, err := seedOneGatedSource(ctx, store, false, curated, 100)
		if err != nil {
			t.Fatalf("preview of %s returned %v, want nil", curated, err)
		}
		if got != want {
			t.Errorf("preview of %s reported %d contracts, want the %d it would upsert", curated, got, want)
		}
	})
}
