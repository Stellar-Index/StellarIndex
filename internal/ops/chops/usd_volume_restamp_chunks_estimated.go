// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"fmt"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ─── the ESTIMATED tiers' half of the chunk walk ────────────────────────
//
// The chunk driver (usd_volume_restamp_chunks.go) owns the chunks, the
// compression policy, the run lock and the free-space guard. This file is
// the other half for every tier whose write set is a ROW LIST — the #372
// XLM-base re-derive, its XLM-quote mirror, and the CEX fiat-quote
// re-derive. All three plan a window through the live valuation function,
// apply the plan in `-chunk-batch` transactions and report the same
// dispositions, so they share ONE [chunkRestampTier] implementation: what
// differs is the run's planner, and a guard cannot drift between them
// because there is only one copy of it.
//
// The exact tier has its own (usd_volume_restamp_chunks_exact.go): its
// write set is a set-based UPDATE per `-slice` window, not a row list.

// estimatedChunkApply is the guarded in-chunk write: the store's apply
// with the is-the-chunk-still-decompressed check ahead of every batch.
type estimatedChunkApply func(ctx context.Context, c timescale.TradeChunk, plan *timescale.RestampPlan, generation int64, batch int) (int64, error)

// estimatedChunkTier drives one estimated tier's re-derive through
// [runChunkRestamp].
type estimatedChunkTier struct {
	run     *xlmBaseRestampRun
	opts    xlmBaseRestampOptions
	inChunk estimatedChunkApply
}

func (t *estimatedChunkTier) write() bool { return t.run.write }

func (t *estimatedChunkTier) header(from, to time.Time, copts chunkRestampOptions) string {
	return fmt.Sprintf("usd-volume-restamp: tier=%s mode=chunks window [%s, %s] slice=%s chunk_batch=%d generation=%d max-generation=%d fill_null=%v min_rel_delta=%s%s\n",
		t.run.tier, from.Format(time.DateOnly), to.Format(time.DateOnly), t.opts.Slice, copts.Batch,
		t.opts.Generation, t.opts.MaxGeneration, t.opts.FillNull, ratPercent(t.opts.MinRelDelta), t.run.headerFlags)
}

// probe plans [lo, hi) slice by slice, folding NOTHING into the report,
// and stops at the first slice that would change a row.
func (t *estimatedChunkTier) probe(ctx context.Context, lo, hi time.Time) (bool, error) {
	w, err := t.run.walk(ctx, lo, hi, xlmBaseWalkProbe)
	if err != nil {
		return false, err
	}
	return w.stats.Changed > 0, nil
}

// preview is the dry run's pass over the chunk as it is.
func (t *estimatedChunkTier) preview(ctx context.Context, lo, hi time.Time) (string, error) {
	w, err := t.run.walk(ctx, lo, hi, xlmBaseWalkFull)
	if err != nil {
		return "", err
	}
	t.run.totals.Merge(w.stats)
	return fmt.Sprintf("would change %d row(s) (scanned %d, null-fill %d, already correct %d)",
		w.stats.Changed, w.stats.Scanned, w.stats.NullFilled, w.stats.Unchanged), nil
}

// restamp re-derives the chunk's slice inside the decompressed chunk. The
// outcome carries the rows written even when the walk failed part-way.
func (t *estimatedChunkTier) restamp(ctx context.Context, c timescale.TradeChunk, lo, hi time.Time) (chunkRestampOutcome, error) {
	inChunk := func(ctx context.Context, plan *timescale.RestampPlan, generation int64, batch int) (int64, error) {
		return t.inChunk(ctx, c, plan, generation, batch)
	}
	w, err := t.run.walkWith(ctx, lo, hi, xlmBaseWalkFull, inChunk)
	out := chunkRestampOutcome{
		Written: w.written,
		Note:    fmt.Sprintf("changed %d row(s) (planned %d)", w.written, w.stats.Changed),
	}
	if err != nil {
		return out, err
	}
	t.run.totals.Merge(w.stats)
	return out, nil
}

func (t *estimatedChunkTier) tick(watermark time.Time) {
	t.run.hb.Progress(t.run.progress, uint64(watermark.Unix())) //nolint:gosec // post-1970 timestamp
}

func (t *estimatedChunkTier) finish(cfgPath string, from, to time.Time) string {
	t.run.printReport(from, to)
	return t.run.summary(cfgPath, from, to)
}

func (t *estimatedChunkTier) resume(cfgPath string, from, to time.Time, copts chunkRestampOptions) string {
	return chunkRestampResumeHint(cfgPath, from, to, t.run, t.opts, copts)
}

// ─── the two mirror tiers' chunk entry points ───────────────────────────

// xlmQuoteChunkStore is the xlm-quote tier's chunk seam: the driver's
// chunk/policy/lock primitives, the tier's plan/apply pair, and the
// in-chunk apply that re-reads the chunk's is_compressed ahead of every
// batch. *timescale.Store satisfies it.
type xlmQuoteChunkStore interface {
	chunkRestampStore
	xlmQuoteRestampStore
	ApplyUSDVolumeRestampPlanInChunk(ctx context.Context, c timescale.TradeChunk, plan *timescale.RestampPlan, generation int64, batch int) (int64, error)
}

// runXLMQuoteChunkRestamp is the `-tier xlm-quote -chunks` entry point,
// called by usdVolumeRestamp once the shared window/flag/live-tail
// validation has passed.
func runXLMQuoteChunkRestamp(ctx context.Context, store xlmQuoteChunkStore, cfgPath string, from, to time.Time, opts xlmBaseRestampOptions, copts chunkRestampOptions) error {
	run := newEstimatedRestampRun(opts, xlmQuoteTierProfile(store))
	run.batch = copts.Batch
	return runChunkRestamp(ctx, store, cfgPath, from, to, copts,
		&estimatedChunkTier{run: run, opts: opts, inChunk: store.ApplyUSDVolumeRestampPlanInChunk})
}

// cexFiatChunkStore is the cex-fx tier's chunk seam.
type cexFiatChunkStore interface {
	chunkRestampStore
	cexFiatRestampStore
	ApplyUSDVolumeRestampPlanInChunk(ctx context.Context, c timescale.TradeChunk, plan *timescale.RestampPlan, generation int64, batch int) (int64, error)
}

// runCEXFiatChunkRestamp is the `-tier cex-fx -chunks` entry point.
func runCEXFiatChunkRestamp(ctx context.Context, store cexFiatChunkStore, cfgPath string, from, to time.Time, opts xlmBaseRestampOptions, copts chunkRestampOptions, maxStaleness time.Duration) error {
	run := newEstimatedRestampRun(opts, cexFiatTierProfile(store, maxStaleness))
	run.batch = copts.Batch
	return runChunkRestamp(ctx, store, cfgPath, from, to, copts,
		&estimatedChunkTier{run: run, opts: opts, inChunk: store.ApplyUSDVolumeRestampPlanInChunk})
}
