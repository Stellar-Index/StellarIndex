// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ─── `-tier xlm-base -chunks` — the anchor re-derive's half of the walk ──
//
// The chunk driver (usd_volume_restamp_chunks.go) owns chunks, compression policy, the run lock and
// the free-space guard. This file runs the day walk's plan/apply pair (usd_volume_restamp_xlmbase.go),
// generation guard included, on one chunk through the guarded apply.

// xlmBaseChunkStore is the xlm-base tier's seam: the driver's chunk and
// policy primitives, the day walk's plan/apply pair, and the in-chunk
// apply that re-reads the chunk's is_compressed ahead of every batch.
// *timescale.Store satisfies it.
type xlmBaseChunkStore interface {
	chunkRestampStore
	xlmBaseRestampStore
	ApplyXLMBaseUSDVolumeRestampInChunk(ctx context.Context, c timescale.TradeChunk, plan *timescale.XLMBaseRestampPlan, generation int64, batch int) (int64, error)
}

// runXLMBaseChunkRestamp is the `-tier xlm-base -chunks` entry point, called after shared
// validation. The per-chunk half is the estimated tier's; this tier supplies its planner and apply.
func runXLMBaseChunkRestamp(ctx context.Context, store xlmBaseChunkStore, cfgPath string, from, to time.Time, opts xlmBaseRestampOptions, copts chunkRestampOptions) error {
	run := newXLMBaseRestampRun(store, opts)
	run.batch = copts.Batch
	inChunk := func(ctx context.Context, c timescale.TradeChunk, plan *timescale.RestampPlan, generation int64, batch int) (int64, error) {
		return store.ApplyXLMBaseUSDVolumeRestampInChunk(ctx, c, plan, generation, batch)
	}
	return runChunkRestamp(ctx, store, cfgPath, from, to, copts,
		&estimatedChunkTier{run: run, opts: opts, inChunk: inChunk})
}

// chunkRestampResumeHint builds the RESUME command carrying the run's generation and every
// population-shaping flag. Finished chunks are skipped; the failed chunk was re-compressed
// unless the error says LEFT DECOMPRESSED.
func chunkRestampResumeHint(cfgPath string, from, to time.Time, run *xlmBaseRestampRun, opts xlmBaseRestampOptions, copts chunkRestampOptions) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nRESUME: stellarindex-ops usd-volume-restamp -config %s -tier %s -chunks -from %s -to %s -generation %d",
		cfgPath, run.tier, from.Format(time.DateOnly), to.Format(time.DateOnly), opts.Generation)
	b.WriteString(xlmBaseResumeFlags(opts))
	b.WriteString(run.resumeFlags)
	if copts.Batch != defaultChunkBatch {
		fmt.Fprintf(&b, " -chunk-batch %d", copts.Batch)
	}
	if copts.MinFreeBytes > 0 {
		fmt.Fprintf(&b, " -min-free-bytes %d", copts.MinFreeBytes)
	}
	if copts.AllowLiveAdjacent {
		b.WriteString(" -allow-live-adjacent")
	}
	if opts.Write {
		b.WriteString(" -write")
	}
	b.WriteString("\n  (finished chunks are probed read-only and skipped; the failed chunk was re-compressed unless the error says LEFT DECOMPRESSED)\n")
	return b.String()
}

// xlmBaseResumeFlags renders the population-shaping flags and slice both walks' RESUME lines
// carry. Dropping -sources would restamp every DEX source; dropping -max-generation would admit
// rows the first run excluded.
func xlmBaseResumeFlags(opts xlmBaseRestampOptions) string {
	var b strings.Builder
	if opts.FillNull {
		b.WriteString(" -fill-null")
	}
	if opts.Slice != time.Hour {
		fmt.Fprintf(&b, " -slice %s", opts.Slice)
	}
	b.WriteString(restampSourcesFlag(opts.Allow))
	if opts.MaxGeneration != opts.Generation {
		fmt.Fprintf(&b, " -max-generation %d", opts.MaxGeneration)
	}
	if opts.MinRelDelta != nil {
		fmt.Fprintf(&b, " -min-rel-delta %s", exactRatFlag(opts.MinRelDelta))
	}
	return b.String()
}

// exactRatFlag renders r so [parseMinRelDelta] reads back exactly r: every
// decimal digit when r has a finite expansion, else the a/b fraction. A
// rounded threshold resumes over a different write set, and one rounded
// to zero resumes with no threshold at all.
func exactRatFlag(r *big.Rat) string {
	if n, exact := r.FloatPrec(); exact {
		return r.FloatString(n)
	}
	return r.RatString()
}

// defaultChunkBatch is -chunk-batch's default. Ten times the day walk's
// -batch: inside a decompressed chunk an UPDATE is a plain heap write,
// so the per-transaction footprint the smaller batch bounded is gone.
const defaultChunkBatch = 20_000

// restampSourcesFlag renders `-sources` for a RESUME line, deterministically
// ordered. Empty when the run had no allow-list (every source the tier
// owns), which is the default and must not be spelled out. A resume that
// dropped it would restamp every source where one was asked for.
func restampSourcesFlag(allow map[string]bool) string {
	if len(allow) == 0 {
		return ""
	}
	sources := make([]string, 0, len(allow))
	for s := range allow {
		sources = append(sources, s)
	}
	sort.Strings(sources)
	return " -sources " + strings.Join(sources, ",")
}
