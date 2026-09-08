// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ─── `-tier xlm-base -chunks` — the anchor re-derive's half of the walk ──
//
// The chunk driver (usd_volume_restamp_chunks.go) owns the chunks, the
// compression policy, the run lock and the free-space guard. This file is
// the other half: what the #372 XLM-base re-derive does INSIDE one chunk,
// and it is the same plan/apply pair the day walk uses
// (usd_volume_restamp_xlmbase.go) — same functions, same generation
// guard, same report — restricted to the chunk's slice of the window and
// routed through the guarded in-chunk apply.

// xlmBaseChunkStore is the xlm-base tier's seam: the driver's chunk and
// policy primitives, the day walk's plan/apply pair, and the in-chunk
// apply that re-reads the chunk's is_compressed ahead of every batch.
// *timescale.Store satisfies it.
type xlmBaseChunkStore interface {
	chunkRestampStore
	xlmBaseRestampStore
	ApplyXLMBaseUSDVolumeRestampInChunk(ctx context.Context, c timescale.TradeChunk, plan *timescale.XLMBaseRestampPlan, generation int64, batch int) (int64, error)
}

// runXLMBaseChunkRestamp is the `-tier xlm-base -chunks` entry point,
// called by usdVolumeRestamp once the shared window/flag/live-tail
// validation has passed. The per-chunk half is the shared estimated-tier
// one (usd_volume_restamp_chunks_estimated.go); what this tier supplies
// is its planner and its own guarded in-chunk apply.
func runXLMBaseChunkRestamp(ctx context.Context, store xlmBaseChunkStore, cfgPath string, from, to time.Time, opts xlmBaseRestampOptions, copts chunkRestampOptions) error {
	run := newXLMBaseRestampRun(store, opts)
	run.batch = copts.Batch
	inChunk := func(ctx context.Context, c timescale.TradeChunk, plan *timescale.RestampPlan, generation int64, batch int) (int64, error) {
		return store.ApplyXLMBaseUSDVolumeRestampInChunk(ctx, c, plan, generation, batch)
	}
	return runChunkRestamp(ctx, store, cfgPath, from, to, copts,
		&estimatedChunkTier{run: run, opts: opts, inChunk: inChunk})
}

// chunkRestampResumeHint is the chunk walk's RESUME line: the same
// command, carrying the run's generation and every flag that shaped its
// population. Finished chunks are probed read-only and skipped; the
// failed chunk was re-compressed unless the error above says LEFT
// DECOMPRESSED.
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

// xlmBaseResumeFlags renders the flags BOTH walks' RESUME lines carry:
// everything that shapes the population (-fill-null, -sources,
// -max-generation, -min-rel-delta) and the slice. A resume that dropped
// -sources would restamp every DEX source where one was asked for; one
// that dropped -max-generation would admit rows the first run excluded.
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
		fmt.Fprintf(&b, " -min-rel-delta %s", opts.MinRelDelta.FloatString(6))
	}
	return b.String()
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
