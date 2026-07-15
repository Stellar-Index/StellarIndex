// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

// Package opsutil holds the small set of helpers shared across the
// stellarindex-ops subcommand packages (internal/ops/{ingest,archive,
// discovery,supply,diagnostics,chops}) — extracted alongside the
// cmd/stellarindex-ops → internal/ops/* package split (maintainability
// audit 2026-07-01, D1 finding M1-5). Each of these previously lived
// in one subcommand's file (backfill.go, cross_region_check.go,
// backfill_router.go, wasm_history.go, ledgerstream_config.go) but was
// called directly by subcommands that now live in a different
// package, so it moved here rather than being duplicated or forcing
// an odd cross-bucket import.
package opsutil

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/stellar/go-stellar-sdk/ingest/ledgerbackend"
	"github.com/stellar/go-stellar-sdk/support/datastore"

	"github.com/StellarIndex/stellar-index/internal/config"
	"github.com/StellarIndex/stellar-index/internal/ledgerstream"
)

// ErrExitSilently is a sentinel error subcommand handlers return when
// they want stellarindex-ops to exit 1 *without* the dispatch table
// printing an extra "subcommand: <err>" prefix line — they already
// printed a more specific message themselves. Used in place of a bare
// os.Exit(1) so subcommand handlers drain the fd 2 filter via
// realMain's defer before exit (rc.77 regression: short-lived
// subcommands printed only their first line then ate the rest because
// the consumer goroutine behind fd 2's filter was killed mid-buffer).
var ErrExitSilently = errors.New("exit silently")

// SignalContext returns a context that cancels on SIGINT / SIGTERM so
// long-running passes (backfill-router, tag-routed-via, the ch-*
// ClickHouse walkers) can flush a final checkpoint and exit cleanly.
// Pulled out so callers can defer cancel() right after the call site.
func SignalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		fmt.Fprintln(os.Stderr, "stellarindex-ops: signal received, flushing checkpoint + exiting...")
		cancel()
	}()
	return ctx, cancel
}

// SplitCSV splits a comma-separated flag value into trimmed,
// non-empty parts.
func SplitCSV(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Truncate shortens s to at most n bytes on a UTF-8 rune boundary,
// appending "...(truncated)" when it does. Used to keep long values
// (subscription refs, cursor blobs) out of one-line log/report output.
func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	end := n
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + "...(truncated)"
}

// MkBackfillLogger returns the plain stderr text logger stellarindex-ops
// subcommands use for progress output (originated in the `backfill`
// subcommand; hubble-check and resume-stalled re-use it for identical
// formatting).
func MkBackfillLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}

// RangeChunk is one worker's slice of an overall [From,To] ledger range.
type RangeChunk struct{ From, To uint32 }

// SplitRange divides [from,to] into n contiguous chunks. The last
// chunk absorbs any remainder so the union exactly covers [from,to].
//
// Degrades to a single chunk when n ≤ 1, the range is single-ledger,
// or n exceeds the range span (would otherwise produce zero-width
// chunks that the downstream walkers can't process).
func SplitRange(from, to uint32, n int) []RangeChunk {
	if n <= 1 || to <= from {
		return []RangeChunk{{from, to}}
	}
	span := to - from + 1
	if uint32(n) > span {
		return []RangeChunk{{from, to}}
	}
	width := span / uint32(n)
	out := make([]RangeChunk, n)
	for i := 0; i < n; i++ {
		chunkFrom := from + uint32(i)*width
		chunkTo := chunkFrom + width - 1
		if i == n-1 {
			chunkTo = to // last chunk absorbs remainder
		}
		out[i] = RangeChunk{chunkFrom, chunkTo}
	}
	return out
}

// NewBoundedLedgerStreamConfig returns the ledgerstream.Config that ops
// subcommands should ALWAYS use when their `-to` may equal the live
// galexie-archive tip. Always opts into TolerateTrailingMissing per
// rc.81 (#62 diagnosis); never override that downstream.
//
// Background: the trailing-edge missing-file failure surfaced in the
// 2026-05-25 verify-archive bootstrap (project_62_diagnosis_2026_05_25)
// was patched site-by-site in verify-archive and wasm-history. The
// other ops subcommands that stream LCM (verify-decoders,
// scan-soroban-events) used to construct ledgerstream.Config inline
// without the flag and could hit the same trap when called with
// `-to 0` (live tip). This helper centralises the construction so the
// flag can't be forgotten.
//
// parallel is the number of concurrent ledgerstream.Stream walkers
// the CALLER is about to run against copies of the returned Config
// (ch-backfill's -parallel, wasm-history's -parallel, verify-archive's
// -workers all split a bounded range into N contiguous chunks and walk
// each on its own goroutine — see boundedWalkerBufferConfig below).
// Single-walker callers pass 1.
//
// # Why this sets an explicit Buffered override (2026-07-15 -parallel OOM)
//
// Left nil, [ledgerstream.Stream] falls back to the SDK's
// ingest.DefaultBufferedStorageBackendConfig(lpf). Because this helper
// never populates DataStore.Schema, lpf resolves to 1 inside Stream,
// which selects the SDK's "small files" branch: BufferSize=10000,
// NumWorkers=10 (github.com/stellar/go-stellar-sdk v0.6.0
// ingest/producer.go — sized "so a bounded range fits entirely" for a
// SINGLE walker). Each ledgerstream.Stream call constructs its own
// BufferedStorageBackend with an independent priority-queue buffer, so
// N parallel walkers sharing one process multiply that queue depth by
// N. On r1, `stellarindex-ops ch-backfill -parallel 2` and `-parallel
// 4` both OOM-killed the 20G ops job cap (run-heavy-job.sh) within
// ~1000 ledgers; `-parallel 1` was stable at ~12GB using the same
// 10000-deep default. The single walker is IO-latency-bound (it sleeps
// on serial MinIO fetches; CPU is idle) — parallelism is the right
// throughput lever for a backfill, but only once per-walker buffer
// memory is bounded so it doesn't scale unboundedly with N.
//
// boundedWalkerBufferBudget is a TOTAL ledger-count budget shared
// across all N walkers a caller intends to run concurrently: each
// walker's BufferSize is boundedWalkerBufferBudget/parallel, floored
// at boundedWalkerBufferMin so a high -parallel still keeps enough
// read-ahead to hide MinIO fetch latency (that's the buffer's only
// job here — it doesn't need to hold a large window). At parallel=1
// this alone shrinks the queue depth from the SDK's 10000 to 200 — a
// single walker never needed a range's-worth of prefetch either.
// NumWorkers is fixed at a modest per-backend fetch concurrency
// (well under boundedWalkerBufferMin, satisfying the SDK's NumWorkers
// <= BufferSize invariant at any parallel). RetryLimit/RetryWait match
// the SDK's own defaults — only BufferSize/NumWorkers needed bounding.
//
// The indexer's live-tail path (internal/pipeline.LedgerstreamConfig)
// is deliberately NOT touched by this — it runs exactly one walker
// and legitimately wants the SDK's larger default for throughput.
func NewBoundedLedgerStreamConfig(cfg config.Config, bucket string, parallel int) ledgerstream.Config {
	return ledgerstream.Config{
		DataStore: datastore.DataStoreConfig{
			Type: "S3",
			Params: map[string]string{
				"destination_bucket_path": bucket,
				"region":                  cfg.Storage.S3Region,
				"endpoint_url":            cfg.Storage.S3Endpoint,
			},
			NetworkPassphrase: cfg.Stellar.Passphrase(),
			Compression:       "zstd",
		},
		TolerateTrailingMissing: true,
		Buffered:                boundedWalkerBufferConfig(parallel),
	}
}

const (
	// boundedWalkerBufferBudget is the total per-process ledger
	// read-ahead depth shared across every concurrent bounded-backfill
	// walker (ch-backfill -parallel, wasm-history -parallel,
	// verify-archive -workers). See NewBoundedLedgerStreamConfig's doc
	// for the 2026-07-15 OOM this replaces.
	boundedWalkerBufferBudget = 200

	// boundedWalkerBufferMin is the floor each walker's BufferSize is
	// clamped to, so a high -parallel doesn't starve any one walker's
	// read-ahead below what's needed to hide MinIO fetch latency.
	boundedWalkerBufferMin = 32

	// boundedWalkerNumWorkers is the fixed per-backend fetch
	// concurrency (independent of parallel — this is in-flight S3 GETs
	// per walker, not queue depth). Always <= boundedWalkerBufferMin,
	// satisfying the SDK's NewBufferedStorageBackend NumWorkers <=
	// BufferSize invariant regardless of parallel.
	boundedWalkerNumWorkers = 4

	// boundedWalkerRetryLimit / boundedWalkerRetryWait mirror the SDK's
	// own ingest.DefaultBufferedStorageBackendConfig defaults — only
	// BufferSize/NumWorkers needed bounding for the OOM fix.
	boundedWalkerRetryLimit = 5
	boundedWalkerRetryWait  = 30 * time.Second
)

// boundedWalkerBufferConfig returns the bounded, parallelism-scaled
// BufferedStorageBackendConfig override for the ops bounded-backfill
// read path. parallel <= 1 is treated as a single walker.
func boundedWalkerBufferConfig(parallel int) *ledgerbackend.BufferedStorageBackendConfig {
	if parallel < 1 {
		parallel = 1
	}
	bufSize := boundedWalkerBufferBudget / parallel
	if bufSize < boundedWalkerBufferMin {
		bufSize = boundedWalkerBufferMin
	}
	return &ledgerbackend.BufferedStorageBackendConfig{
		BufferSize: uint32(bufSize),
		NumWorkers: boundedWalkerNumWorkers,
		RetryLimit: boundedWalkerRetryLimit,
		RetryWait:  boundedWalkerRetryWait,
	}
}
