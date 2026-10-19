// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

package chops

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/pipeline"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// `tag-tx-index -chunks` tags compressed history through the trades chunk walk
// (usd_volume_restamp_chunks.go): an UPDATE into a compressed chunk decompresses it inside the
// transaction, so each chunk is decompressed once, tagged window by window, and re-compressed.

// txIndexChunkStore is the tier's seam: the walk's chunk/policy/lock primitives, the tagger, the
// read-only probe and the per-page compression check. *timescale.Store satisfies it.
type txIndexChunkStore interface {
	chunkRestampStore
	pipeline.TxIndexTradeTagger
	UntaggedTxIndexTradeExists(ctx context.Context, from, to time.Time) (bool, error)
	TradesChunkCompressed(ctx context.Context, c timescale.TradeChunk) (bool, error)
}

type txIndexChunkTier struct {
	lake    pipeline.TxIndexLakeReader
	store   txIndexChunkStore
	isWrite bool
	window  time.Duration
	hb      *opsutil.JobHeartbeat

	tagged       int64
	dirty, clean int
}

// runTagTxIndexChunks walks [from, to), both at UTC midnight, chunk by chunk. The walk takes a
// date-inclusive end, hence the day off `to`; the tier adds it back for what it prints.
func runTagTxIndexChunks(ctx context.Context, lake pipeline.TxIndexLakeReader, store txIndexChunkStore, cfgPath string,
	write bool, from, to time.Time, window time.Duration, hb *opsutil.JobHeartbeat, copts chunkRestampOptions,
) error {
	copts.Command = "tag-tx-index"
	tier := &txIndexChunkTier{lake: lake, store: store, isWrite: write, window: window, hb: hb}
	return runChunkRestamp(ctx, store, cfgPath, from, to.AddDate(0, 0, -1), copts, tier)
}

func (t *txIndexChunkTier) write() bool { return t.isWrite }

func (t *txIndexChunkTier) header(from, to time.Time, _ chunkRestampOptions) string {
	return fmt.Sprintf("tag-tx-index: mode=chunks [%s, %s) window %s\n",
		from.Format(time.RFC3339), to.AddDate(0, 0, 1).Format(time.RFC3339), t.window)
}

func (t *txIndexChunkTier) probe(ctx context.Context, lo, hi time.Time) (bool, error) {
	return t.store.UntaggedTxIndexTradeExists(ctx, lo, hi)
}

func (t *txIndexChunkTier) preview(ctx context.Context, lo, hi time.Time) (string, error) {
	dirty, err := t.probe(ctx, lo, hi)
	if err != nil {
		return "", err
	}
	if !dirty {
		t.clean++
		return "no untagged on-chain trades", nil
	}
	t.dirty++
	return "holds untagged on-chain trades; -write would decompress it", nil
}

func (t *txIndexChunkTier) restamp(ctx context.Context, c timescale.TradeChunk, lo, hi time.Time) (chunkRestampOutcome, error) {
	guarded := chunkGuardedTagger{txIndexChunkStore: t.store, c: c}
	var written int64
	for wlo := lo; wlo.Before(hi); wlo = wlo.Add(t.window) {
		whi := wlo.Add(t.window)
		if whi.After(hi) {
			whi = hi
		}
		n, err := pipeline.TagTxIndexWindow(ctx, t.lake, guarded, wlo, whi, pipeline.TxIndexPageSize)
		written += n
		t.tagged += n
		t.tick(whi)
		if err != nil {
			return chunkRestampOutcome{Written: written}, fmt.Errorf("window [%s, %s): %w", wlo.Format(time.RFC3339), whi.Format(time.RFC3339), err)
		}
	}
	note := fmt.Sprintf("tagged %d trade(s)", written)
	if written == 0 {
		// The probe saw untagged rows the lake could not resolve; a rerun will decompress this chunk again.
		note += " (its untagged trades have no lake transaction)"
	}
	return chunkRestampOutcome{Written: written, Note: note}, nil
}

func (t *txIndexChunkTier) tick(watermark time.Time) {
	t.hb.Progress(uint64(t.tagged), uint64(watermark.Unix())) //nolint:gosec // non-negative count; post-1970 timestamp
}

func (t *txIndexChunkTier) tickBytes(moved uint64) { t.hb.ProgressBytes(moved) }

func (t *txIndexChunkTier) finish(cfgPath string, from, to time.Time) string {
	if !t.isWrite {
		return fmt.Sprintf("DRY RUN: %d chunk(s) hold untagged on-chain trades, %d hold none. To apply:\n    %s\n",
			t.dirty, t.clean, t.command(cfgPath, from, to, chunkRestampOptions{}, true))
	}
	return fmt.Sprintf("tag-tx-index: done. tagged trades: %d\n", t.tagged)
}

func (t *txIndexChunkTier) resume(cfgPath string, from, to time.Time, copts chunkRestampOptions) string {
	return fmt.Sprintf("RESUME (tagging is first-wins; finished chunks are skipped undecompressed):\n    %s\n",
		t.command(cfgPath, from, to, copts, t.isWrite))
}

func (t *txIndexChunkTier) command(cfgPath string, from, to time.Time, copts chunkRestampOptions, write bool) string {
	args := []string{
		"stellarindex-ops tag-tx-index -config " + cfgPath, "-chunks",
		"-from " + from.Format(time.RFC3339), "-to " + to.AddDate(0, 0, 1).Format(time.RFC3339), "-window " + t.window.String(),
	}
	if copts.MinFreeBytes > 0 {
		args = append(args, fmt.Sprintf("-min-free-bytes %d", copts.MinFreeBytes))
	}
	if copts.AllowLiveAdjacent {
		args = append(args, "-allow-live-adjacent")
	}
	if write {
		args = append(args, "-write")
	}
	return strings.Join(args, " ")
}

// chunkGuardedTagger re-reads the chunk's compression state before every page it tags: a page
// written into a re-compressed chunk would decompress it inside the UPDATE.
type chunkGuardedTagger struct {
	txIndexChunkStore
	c timescale.TradeChunk
}

func (g chunkGuardedTagger) TagTradesTxIndex(ctx context.Context, from, to time.Time, tags []timescale.TxIndexTag) (int64, error) {
	compressed, err := g.TradesChunkCompressed(ctx, g.c)
	if err != nil {
		return 0, err
	}
	if compressed {
		return 0, fmt.Errorf("%w: %s reads is_compressed = true ahead of the next page; the page is NOT written", timescale.ErrTradesChunkRecompressed, g.c)
	}
	return g.txIndexChunkStore.TagTradesTxIndex(ctx, from, to, tags)
}
