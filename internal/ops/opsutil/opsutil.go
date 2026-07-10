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
	"unicode/utf8"

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
func NewBoundedLedgerStreamConfig(cfg config.Config, bucket string) ledgerstream.Config {
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
	}
}
