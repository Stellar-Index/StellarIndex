// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

// Package opsutil holds the small set of helpers shared across the
// stellarindex-ops subcommand packages (internal/ops/{ingest,archive,
// discovery,supply,diagnostics,chops}). Most are called directly by
// subcommands in more than one package, so they live here rather than
// being duplicated or forcing an odd cross-bucket import.
package opsutil

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"os/user"
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
// realMain's defer before exit: a bare os.Exit kills the consumer
// goroutine behind fd 2's filter mid-buffer, so a short-lived
// subcommand prints only its first line and loses the rest.
var ErrExitSilently = errors.New("exit silently")

// ExitCodeError lets a subcommand report a specific positive exit
// code — not just the generic 1 — while still returning through the
// normal realMain flow, so the flush() defer in
// cmd/stellarindex-ops/main.go's realMain (SilenceSDKChecksumWarnings)
// still runs. NEVER call os.Exit directly from a subcommand handler
// for this — see realMain's doc comment for why.
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

// WriteGate is the shared fail-closed write toggle mutating
// stellarindex-ops subcommands register, so the CLI shares ONE
// convention: a command previews by DEFAULT and applies changes only
// when the operator passes -write. The opposite, default-WRITE shape —
// writing UNLESS the operator passes -dry-run — silently mutates a money
// surface the moment a flag is forgotten. Build with [RegisterWriteGate],
// gate the write with [WriteGate.Enabled], and announce the mode once
// with [WriteGate.Banner].
type WriteGate struct {
	write  *bool
	dryRun *bool
}

// WriteFlagUsage is the -write flag's help text. Every write-gated
// subcommand's `stellarindex-ops --help` synopsis must name -write: dry run is
// the default, so a synopsis offering only -dry-run documents a command that
// never writes.
const WriteFlagUsage = "apply changes to the datastore. Without it this command is a fail-closed DRY RUN: it reports what would change and writes nothing."

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
		write: fs.Bool("write", false, WriteFlagUsage),
		dryRun: fs.Bool("dry-run", false,
			"preview only, writing nothing — the DEFAULT. Retained as an explicit no-op alias for existing callers; pass -write to actually apply."),
	}
}

// NewMutatingFlagSet builds the flag.FlagSet a MUTATING stellarindex-ops
// subcommand parses its arguments with, and arms the shared [WriteGate]
// on it in the same call. Read-only subcommands keep using
// flag.NewFlagSet directly.
//
// It exists because [RegisterWriteGate] is a convention a subcommand may
// simply decline, and a convention that can be declined is not a safety
// property. A mutating subcommand that declares neither -write nor -dry-run
// writes unconditionally, with no preview to catch a mistyped range before
// the UPDATE runs. Getting the FlagSet and getting the gate is one call, so a
// mutating subcommand cannot acquire one without the other.
//
// flag.ContinueOnError matches every ops subcommand: the handler returns
// the parse error rather than calling os.Exit, so realMain's fd-2 filter
// still drains (see [ErrExitSilently]).
func NewMutatingFlagSet(name string) (*flag.FlagSet, *WriteGate) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	return fs, RegisterWriteGate(fs)
}

// IsHelpArg reports whether arg is one of the help spellings realMain
// accepts. Namespace and positional verbs read their first argument before
// any FlagSet exists, so fs.Parse never sees a help request there.
func IsHelpArg(arg string) bool {
	switch arg {
	case "help", "-h", "-help", "--help":
		return true
	}
	return false
}

// Usage writes usage to stderr and returns flag.ErrHelp, the sentinel the
// dispatcher maps to exit 0 — the same contract fs.Parse gives a leaf.
func Usage(usage string) error {
	fmt.Fprintln(os.Stderr, usage)
	return flag.ErrHelp
}

// Enabled reports whether the operator opted into writing (passed -write).
func (g *WriteGate) Enabled() bool { return *g.write }

// ErrWriteModeUnstated is returned by [WriteGate.RequireStatedMode] when
// a run passed neither -write nor -dry-run.
var ErrWriteModeUnstated = errors.New("state the mode: pass -write to apply, or -dry-run to preview")

// RequireStatedMode refuses a run that passed neither -write nor -dry-run.
// Range walkers whose scripted callers treat exit 0 as "window done" use it,
// so a caller written for the old default-WRITE contract fails loudly
// instead of silently previewing and reporting success.
func (g *WriteGate) RequireStatedMode() error {
	if *g.write || *g.dryRun {
		return nil
	}
	return ErrWriteModeUnstated
}

// DryRun reports whether this run writes nothing — the default unless
// -write was passed. It is the exact negation of [WriteGate.Enabled],
// provided so a subcommand's existing `dryRun` control flow reads
// unchanged after the flip.
func (g *WriteGate) DryRun() bool { return !*g.write }

// DryRunStated reports whether the operator passed -dry-run explicitly, for a
// command whose stated preview does more than its bare default.
func (g *WriteGate) DryRunStated() bool { return *g.dryRun }

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

// RequireExplicitRange refuses an implicit full-history run: a windowed CH
// backfill left with every bound at its default silently starts the
// entire history (ledger 2..tip). fs must already be parsed. full is the
// subcommand's own "-full" flag value (true opts into the whole history on
// purpose); jobName and rowEstimate name the command and its approximate
// row count in the error message. Shared so every windowed CH backfill
// enforces the same footgun guard.
func RequireExplicitRange(fs *flag.FlagSet, full bool, jobName, rowEstimate string) error {
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if !full && !set["from"] && !set["to"] {
		return fmt.Errorf(
			"refusing an implicit full-history backfill (ledger 2..tip, ~%s rows on r1): pass -from (a resume point / lower bound), -to (an upper bound), or -full to run the entire history from scratch (%s)",
			rowEstimate, jobName)
	}
	return nil
}

// SignalContext returns a context that cancels on SIGINT / SIGTERM so
// long-running passes (backfill-router, tag-routed-via, the ch-*
// ClickHouse walkers) can flush a final checkpoint and exit cleanly.
// Pulled out so callers can defer cancel() right after the call site.
//
// The handler is unregistered on the first signal (and by cancel), so a
// second SIGINT / SIGTERM gets the default action and kills a wedged flush.
func SignalContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	stop := func() {
		signal.Stop(sig)
		cancel()
	}
	go func() {
		select {
		case <-sig:
			signal.Stop(sig)
			fmt.Fprintln(os.Stderr, "stellarindex-ops: signal received, flushing checkpoint + exiting (signal again to force)...")
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, stop
}

// ResolveActor names who ran a privileged ops command: the -actor flag,
// else the OS login. A recorded action with no actor is refused.
func ResolveActor(flagActor string) (string, error) {
	if a := strings.TrimSpace(flagActor); a != "" {
		return a, nil
	}
	u, err := user.Current()
	if err != nil || strings.TrimSpace(u.Username) == "" {
		return "", errors.New("-actor is required (the OS user could not be resolved)")
	}
	return u.Username, nil
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

// AssertNonVacuous refuses to certify a verification that observed
// nothing: of total things in scope, observed produced evidence. Nothing
// in scope, or nothing observed, is an unexamined run, not a clean pass.
func AssertNonVacuous(observed, total int, what string) error {
	if total <= 0 {
		return fmt.Errorf("0 %s in scope — nothing to verify; refusing to certify a pass vacuously", what)
	}
	if observed <= 0 {
		return fmt.Errorf("0 of %d %s produced any observation — nothing was verified; refusing to certify a pass vacuously", total, what)
	}
	return nil
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

// RangeCursorKey is the ingestion_cursors sub_source for a range-scoped
// resume checkpoint. UpsertCursor is monotone-forward, so a key without its
// range lets a finished later range turn an earlier one into "nothing to do".
func RangeCursorKey(from, to uint32) string {
	return fmt.Sprintf("%d-%d", from, to)
}

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

// HistoricReadBucket resolves the bucket for a read over a backfilled range:
// -bucket, else the archive (live is trimmed), else live.
func HistoricReadBucket(cfg config.Config, override string) (string, error) {
	for _, b := range []string{override, cfg.Storage.S3BucketArchive, cfg.Storage.S3BucketLive} {
		if b != "" {
			return b, nil
		}
	}
	return "", fmt.Errorf("no bucket: set -bucket or storage.s3_bucket_archive / s3_bucket_live")
}

// ResolveStreamBucket resolves which galexie bucket a BOUNDED backfill
// walk reads, given the operator's -bucket override and the requested
// ledger range. Every ops subcommand that walks a historic range
// (ch-backfill, census-backfill) must go through this rather than
// defaulting to a bucket of its own choosing.
//
// The live bucket holds only what galexie exported since this node started
// (on r1 it is also trimmed), so a historic range there resolves to zero
// objects. With TolerateTrailingMissing that walk exits 0 and the caller
// records a permanent hole as done. One shared resolver keeps callers from
// holding independent copies of the default.
//
// Resolution order:
//  1. An explicit -bucket always wins.
//  2. With a live seam configured (ingestion.live_seam_ledger), the range
//     decides: entirely below the seam reads the archive, at or above it
//     reads live. A range that straddles the seam is an error, because one
//     walk reads one bucket and either choice drops the other side.
//  3. With no seam, the live bucket. Its floor is not knowable from config,
//     and switching the default to the archive (an hourly mirror that lags
//     the tip) would break scripts/ops/ch-live-catchup.sh. The caller's
//     coverage check makes a wrong bucket fail loudly instead. Setting the
//     seam also changes the indexer's read path, so it stays an operator
//     decision.
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
// galexie-archive tip. Always opts into TolerateTrailingMissing; never
// override that downstream: without it a walk reaching the tip fails on
// the trailing-edge missing file.
//
// parallel is the number of concurrent ledgerstream.Stream walkers the
// CALLER will run against copies of the returned Config; single-walker
// callers pass 1.
//
// # Why this sets an explicit Buffered override
//
// Left nil, each Stream builds its own SDK buffered backend with a
// 10000-ledger queue, so N parallel walkers multiply that memory by N: on
// r1 `ch-backfill -parallel 2` and `-parallel 4` OOM-killed the 20G ops cap
// within ~1000 ledgers. Walkers are IO-latency-bound, so parallelism is the
// right lever once per-walker memory is bounded.
//
// boundedWalkerBufferBudget is a TOTAL ledger budget split across the N
// walkers, floored at boundedWalkerBufferMin so each keeps enough
// read-ahead to hide MinIO latency. NumWorkers stays under that floor to
// satisfy the SDK's NumWorkers <= BufferSize invariant; retry settings
// match the SDK defaults.
//
// The indexer's live-tail path (internal/pipeline.LedgerstreamConfig) runs
// one walker and keeps the SDK's larger default.
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
	// for the OOM it prevents.
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
	// BufferSize/NumWorkers need bounding.
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
