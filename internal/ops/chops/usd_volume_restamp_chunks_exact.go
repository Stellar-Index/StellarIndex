// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// ─── `-tier exact -chunks` — the peg identity's half of the walk ────────
//
// The chunk driver (usd_volume_restamp_chunks.go) owns the chunks, the
// compression policy, the run lock and the free-space guard. This file runs
// the exact tier's `round(pegged_leg / 10^decimals, 8)` UPDATE (the day
// walk's, usd_volume_restamp.go) on one chunk's slice through the guarded
// in-chunk apply. Any DML into a compressed chunk decompresses it inside the
// transaction, so an in-place exact repair over millions of rows takes days;
// the per-chunk bracket does not.
//
// Not shared with the xlm-base tier: `-chunk-batch` (one `-slice` window is one
// UPDATE, so `-slice` is the bound and the flag is refused rather than ignored)
// and `-max-generation` / `-min-rel-delta` / `-report` / `-sample` (an identity
// has no relative-move distribution).
//
// Fail-closed dry run: without `-write` it counts through
// CountUSDVolumeRestampCandidates and decompresses nothing.

// exactChunkStore is the exact tier's seam: the driver's chunk and policy
// primitives, the day walk's classify/count/apply trio, and the in-chunk
// apply that re-reads the chunk's is_compressed ahead of the UPDATE.
// *timescale.Store satisfies it.
type exactChunkStore interface {
	chunkRestampStore
	exactRestampStore
	RestampExactTierUSDVolumeInChunk(ctx context.Context, c timescale.TradeChunk, p timescale.USDVolumeRestampParams) (int64, error)
}

// exactChunkTier drives the exact-tier repair through [runChunkRestamp].
type exactChunkTier struct {
	run   *restampRun
	store exactChunkStore
	// rows is what the run counted (dry run) or wrote (-write) across
	// every chunk so far — the figure its closing summary reports.
	rows int64
}

// runExactChunkRestamp is the `-tier exact -chunks` entry point, called by
// usdVolumeRestamp once the shared window/flag/live-tail validation has
// passed.
func runExactChunkRestamp(ctx context.Context, store exactChunkStore, run *restampRun, cfgPath string, from, to time.Time, copts chunkRestampOptions) error {
	return runChunkRestamp(ctx, store, cfgPath, from, to, copts, &exactChunkTier{run: run, store: store})
}

func (t *exactChunkTier) write() bool { return t.run.write }

func (t *exactChunkTier) header(from, to time.Time, _ chunkRestampOptions) string {
	return fmt.Sprintf("usd-volume-restamp: tier=exact mode=chunks window [%s, %s] slice=%s generation=%d fill_null=%v\n",
		from.Format(time.DateOnly), to.Format(time.DateOnly), t.run.slice, t.run.generation, t.run.fillNull)
}

// walk covers [lo, hi) day by day: classify the day (once — the cache
// survives a chunk boundary that splits it), then walk the intersection
// of that day with the chunk's slice in -slice windows.
func (t *exactChunkTier) walk(ctx context.Context, lo, hi time.Time, apply exactTierApply, probe bool) (int64, error) {
	var rows int64
	for day := lo.UTC().Truncate(24 * time.Hour); day.Before(hi); day = day.AddDate(0, 0, 1) {
		d, err := t.run.dayTargets(ctx, day)
		if err != nil {
			return rows, err
		}
		if len(d.targets) == 0 {
			continue
		}
		dlo, dhi := day, day.AddDate(0, 0, 1)
		if lo.After(dlo) {
			dlo = lo
		}
		if hi.Before(dhi) {
			dhi = hi
		}
		n, err := t.run.sliceWalk(ctx, exactSpan{
			targets: d.targets, lo: dlo, hi: dhi, apply: apply, probe: probe, dated: true,
		})
		rows += n
		if err != nil {
			return rows, err
		}
		if probe && n > 0 {
			return rows, nil
		}
	}
	return rows, nil
}

// probe counts candidates read-only and stops at the first slice that
// would change a row. A chunk that probes clean is skipped by the driver
// without ever being decompressed, which is what makes a rerun walk past
// the finished prefix at dry-run cost.
func (t *exactChunkTier) probe(ctx context.Context, lo, hi time.Time) (bool, error) {
	n, err := t.walk(ctx, lo, hi, t.store.CountUSDVolumeRestampCandidates, true)
	return n > 0, err
}

// preview is the dry run's pass over the chunk as it is: the same scope
// predicate the UPDATE evaluates, counted rather than applied.
func (t *exactChunkTier) preview(ctx context.Context, lo, hi time.Time) (string, error) {
	n, err := t.walk(ctx, lo, hi, t.store.CountUSDVolumeRestampCandidates, false)
	if err != nil {
		return "", err
	}
	t.rows += n
	return fmt.Sprintf("would change %d row(s)", n), nil
}

// restamp applies the identity inside the decompressed chunk. Every
// slice's UPDATE goes through the store's guarded in-chunk apply, which
// refuses to write when the chunk reads compressed again.
func (t *exactChunkTier) restamp(ctx context.Context, c timescale.TradeChunk, lo, hi time.Time) (chunkRestampOutcome, error) {
	inChunk := func(ctx context.Context, p timescale.USDVolumeRestampParams) (int64, error) {
		return t.store.RestampExactTierUSDVolumeInChunk(ctx, c, p)
	}
	n, err := t.walk(ctx, lo, hi, inChunk, false)
	t.rows += n
	return chunkRestampOutcome{Written: n, Note: fmt.Sprintf("changed %d row(s)", n)}, err
}

func (t *exactChunkTier) tick(watermark time.Time) {
	t.run.hb.Progress(t.run.progress, uint64(watermark.Unix())) //nolint:gosec // post-1970 timestamp
}

func (t *exactChunkTier) tickBytes(moved uint64) { t.run.hb.ProgressBytes(moved) }

func (t *exactChunkTier) finish(cfgPath string, from, to time.Time) string {
	return exactRestampSummary(cfgPath, from, to, t.rows, t.run.groupDays(), t.run.write)
}

func (t *exactChunkTier) resume(cfgPath string, from, to time.Time, copts chunkRestampOptions) string {
	return exactChunkResumeHint(cfgPath, from, to, t.run, copts)
}

// exactChunkResumeHint is the exact tier's RESUME line: the same command,
// carrying the run's generation and every flag that shaped its
// population. Finished chunks are probed read-only and skipped; the
// failed chunk was re-compressed unless the error above says LEFT
// DECOMPRESSED.
func exactChunkResumeHint(cfgPath string, from, to time.Time, run *restampRun, copts chunkRestampOptions) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\nRESUME: stellarindex-ops usd-volume-restamp -config %s -tier %s -chunks -from %s -to %s -generation %d",
		cfgPath, restampTierExact, from.Format(time.DateOnly), to.Format(time.DateOnly), run.generation)
	if run.fillNull {
		b.WriteString(" -fill-null")
	}
	if run.slice != time.Hour {
		fmt.Fprintf(&b, " -slice %s", run.slice)
	}
	b.WriteString(restampSourcesFlag(run.allow))
	if copts.MinFreeBytes > 0 {
		fmt.Fprintf(&b, " -min-free-bytes %d", copts.MinFreeBytes)
	}
	if copts.AllowLiveAdjacent {
		b.WriteString(" -allow-live-adjacent")
	}
	if run.write {
		b.WriteString(" -write")
	}
	b.WriteString("\n  (finished chunks are probed read-only and skipped; the failed chunk was re-compressed unless the error says LEFT DECOMPRESSED)\n")
	return b.String()
}
