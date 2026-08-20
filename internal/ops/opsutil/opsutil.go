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
	"flag"
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

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ledgerstream"
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

// ExitCodeError lets a subcommand report a specific positive exit
// code — not just the generic 1 — while still returning through the
// normal realMain flow, so the flush() defer in
// cmd/stellarindex-ops/main.go's realMain (SilenceSDKChecksumWarnings)
// still runs. NEVER call os.Exit directly from a subcommand handler
// for this — see realMain's doc comment for why (rc.77 regression).
//
// reconcile-balances is the first user: "exit code = number of
// MISMATCHes" mirrors scripts/dev/r1-smoke.sh's "exit code = number
// of failed checks" convention so cron/Healthchecks.io can consume
// either the same way. Err is optional context for the rare case the
// subcommand wants realMain to also print a message; leave nil when
// the subcommand has already printed its own report.
type ExitCodeError struct {
	Code int
	Err  error
}

func (e *ExitCodeError) Error() string {
	if e.Err != nil {
		return e.Err.Error()
	}
	return fmt.Sprintf("exit code %d", e.Code)
}

func (e *ExitCodeError) Unwrap() error { return e.Err }

// WriteGate is the shared fail-closed write toggle every mutating
// stellarindex-ops subcommand registers, so the whole CLI shares ONE
// convention: a command previews by DEFAULT and applies changes only
// when the operator passes -write. It replaces the old split where some
// commands wrote UNLESS you passed -dry-run — a default-WRITE shape that
// silently mutates a money surface the moment a flag is forgotten (the
// INV-3 DO-NOTHING/DO-DAMAGE trap). Build with [RegisterWriteGate], gate
// the write with [WriteGate.Enabled], and announce the mode once with
// [WriteGate.Banner].
type WriteGate struct {
	write  *bool
	dryRun *bool
}

// RegisterWriteGate registers the shared -write / -dry-run flag pair on
// fs and returns the gate.
//
//   - -write   applies changes. WITHOUT it the command is a fail-closed
//     DRY RUN that reports what WOULD change and writes nothing.
//   - -dry-run is retained as an explicit no-op alias so existing callers
//     (scripts, runbooks, systemd units) that already pass it keep
//     working. Dry run is the default now, so -dry-run only documents
//     intent; -write wins if both are passed.
func RegisterWriteGate(fs *flag.FlagSet) *WriteGate {
	return &WriteGate{
		write: fs.Bool("write", false,
			"apply changes to the datastore. Without it this command is a fail-closed DRY RUN: it reports what would change and writes nothing."),
		dryRun: fs.Bool("dry-run", false,
			"preview only, writing nothing — the DEFAULT. Retained as an explicit no-op alias for existing callers; pass -write to actually apply."),
	}
}

// Enabled reports whether the operator opted into writing (passed -write).
func (g *WriteGate) Enabled() bool { return *g.write }

// DryRun reports whether this run writes nothing — the default unless
// -write was passed. It is the exact negation of [WriteGate.Enabled],
// provided so a subcommand's existing `dryRun` control flow reads
// unchanged after the flip.
func (g *WriteGate) DryRun() bool { return !*g.write }

// Banner prints the loud fail-closed mode banner to stderr. Call it once,
// after flags are parsed and the command's own required-flag checks pass,
// so the operator sees the mode before any slow or mutating work begins.
// Returns [WriteGate.Enabled] for convenient `if gate.Banner() { … }` use.
func (g *WriteGate) Banner() bool { return PrintWriteBanner(*g.write) }

// PrintWriteBanner prints the loud fail-closed mode banner to stderr for
// the given write mode and returns write. Subcommands that hold a
// [WriteGate] call [WriteGate.Banner]; those that carry the gate decision
// as a plain bool (e.g. inside an options struct) call this directly, so
// the banner text has exactly one definition.
func PrintWriteBanner(write bool) bool {
	if write {
		fmt.Fprintln(os.Stderr, "═══ WRITING — applying changes ═══")
		return true
	}
	fmt.Fprintln(os.Stderr, "═══ DRY RUN — no writes; pass -write to apply ═══")
	return false
}

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

// ResolveStreamBucket resolves which galexie bucket a BOUNDED backfill
// walk reads, given the operator's -bucket override and the requested
// ledger range. Every ops subcommand that walks a historic range
// (ch-backfill, census-backfill) must go through this rather than
// defaulting to a bucket of its own choosing.
//
// The defect this exists to close (found live 2026-07-25 in ch-backfill,
// 2026-07-25 in census-backfill): the walkers defaulted to
// cfg.Storage.S3BucketLive unconditionally, which is silently wrong for
// exactly the ranges these commands exist to serve. The live bucket holds
// only what galexie has exported since this node started (on r1 it is also
// the TRIMMED one — see
// docs/operations/runbooks/consolidated-deploy-plan-2026-07-18.md §4), so a
// historic range resolves to zero objects there. Because every ops walker
// opts into TolerateTrailingMissing (NewBoundedLedgerStreamConfig), that
// walk ends WITHOUT an error, and the caller records the window as DONE on
// exit 0 — a permanent hole, recorded as success. The per-command coverage
// checks close the second half of that trap; this closes the first.
//
// It lives here rather than in one subcommand package because the two
// callers had INDEPENDENT copies of the broken default: fixing one and
// leaving the other is how this class survives a remediation. One seam
// policy, one place.
//
// Resolution order:
//  1. An explicit -bucket always wins: the operator knows their layout, and
//     every runbook that matters already passes it.
//  2. With a live seam configured (ingestion.live_seam_ledger), the RANGE
//     decides — entirely below the seam reads the archive, entirely at or
//     above it reads live. A range that STRADDLES the seam is an error, not
//     a guess: one walk reads one bucket, so either choice silently drops
//     the ledgers on the other side, which is the same vacuous-success
//     failure in a subtler shape.
//  3. With NO seam configured (r1 today), the live bucket — unchanged.
//     Without a seam the live bucket's floor is not knowable from config,
//     and guessing it is what produced this whole class of bug in the
//     first place. Silently switching the default to the archive would
//     also break scripts/ops/ch-live-catchup.sh, which heals live-era
//     holes and extends the tip with no -bucket at all: the archive is an
//     hourly MIRROR of live, so it lags the tip by up to an hour and that
//     timer would fail on every run until the mirror caught up. So the
//     default stays, and the caller's coverage check is what makes a wrong
//     bucket unmissable instead of silent — a historic range now fails
//     naming galexie-live and telling the operator to pass the archive
//     bucket. Setting ingestion.live_seam_ledger promotes this host to
//     case 2 and makes the choice automatic, but it also changes the
//     INDEXER's read path (StreamArchiveThenLive), so it is an operator
//     decision, not a default this function should assume.
func ResolveStreamBucket(cfg config.Config, override string, from, to uint32) (string, error) {
	if override != "" {
		return override, nil
	}
	archive, live := cfg.Storage.S3BucketArchive, cfg.Storage.S3BucketLive
	seam := cfg.Ingestion.LiveSeamLedger
	switch {
	case seam > 0 && to < seam:
		if archive == "" {
			return "", fmt.Errorf("ledgers %d..%d are below the live seam %d but storage.s3_bucket_archive is unset — pass -bucket", from, to, seam)
		}
		return archive, nil
	case seam > 0 && from >= seam:
		if live == "" {
			return "", fmt.Errorf("ledgers %d..%d are at/above the live seam %d but storage.s3_bucket_live is unset — pass -bucket", from, to, seam)
		}
		return live, nil
	case seam > 0:
		return "", fmt.Errorf("ledgers %d..%d straddle the live seam %d — %q holds [%d,tip] and %q holds the history below it, and one walk reads exactly one bucket; split the range at %d or pass -bucket explicitly",
			from, to, seam, live, seam, archive, seam)
	case live != "":
		return live, nil
	case archive != "":
		return archive, nil
	default:
		return "", fmt.Errorf("no galexie bucket configured — set storage.s3_bucket_archive / s3_bucket_live or pass -bucket")
	}
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
