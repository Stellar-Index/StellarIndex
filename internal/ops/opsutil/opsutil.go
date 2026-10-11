// Copyright 2026 Stellar Index contributors
// SPDX-License-Identifier: Apache-2.0

// Package opsutil holds helpers shared across the stellarindex-ops subcommand packages (ingest, archive, discovery, supply, diagnostics, chops).
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

// ErrExitSilently makes stellarindex-ops exit 1 without the dispatcher's extra prefix line; use it instead of os.Exit(1) so realMain's defer drains the fd 2 filter.
var ErrExitSilently = errors.New("exit silently")

// ExitCodeError reports a specific positive exit code through realMain so its flush defer still runs; NEVER call os.Exit from a handler (see realMain). Err is optional context to print.
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

// WriteGate is the shared fail-closed write toggle: a command previews by default and applies only with -write, because a default-write shape mutates a money surface once a flag is forgotten.
type WriteGate struct {
	write  *bool
	dryRun *bool
}

// WriteFlagUsage is the -write help text; every write-gated synopsis must name -write since dry run is the default.
const WriteFlagUsage = "apply changes to the datastore. Without it this command is a fail-closed DRY RUN: it reports what would change and writes nothing."

// RegisterWriteGate registers -write and a no-op -dry-run alias on fs; without -write the command is a fail-closed dry run, and -write wins if both are passed.
func RegisterWriteGate(fs *flag.FlagSet) *WriteGate {
	return &WriteGate{
		write: fs.Bool("write", false, WriteFlagUsage),
		dryRun: fs.Bool("dry-run", false,
			"preview only, writing nothing — the DEFAULT. Retained as an explicit no-op alias for existing callers; pass -write to actually apply."),
	}
}

// NewMutatingFlagSet builds the FlagSet for a mutating subcommand and arms the shared WriteGate in the same call, so none can skip the gate (ContinueOnError so realMain's fd-2 filter drains).
func NewMutatingFlagSet(name string) (*flag.FlagSet, *WriteGate) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	return fs, RegisterWriteGate(fs)
}

// IsHelpArg reports whether arg is a help spelling realMain accepts, for verbs that read arguments before any FlagSet exists.
func IsHelpArg(arg string) bool {
	switch arg {
	case "help", "-h", "-help", "--help":
		return true
	}
	return false
}

// Usage writes usage to stderr and returns flag.ErrHelp, which the dispatcher maps to exit 0.
func Usage(usage string) error {
	fmt.Fprintln(os.Stderr, usage)
	return flag.ErrHelp
}

// Enabled reports whether the operator opted into writing (passed -write).
func (g *WriteGate) Enabled() bool { return *g.write }

// ErrWriteModeUnstated is returned by RequireStatedMode when a run passed neither -write nor -dry-run.
var ErrWriteModeUnstated = errors.New("state the mode: pass -write to apply, or -dry-run to preview")

// RequireStatedMode refuses a run with neither -write nor -dry-run, so scripted callers written for the old default-write contract fail loudly instead of silently previewing.
func (g *WriteGate) RequireStatedMode() error {
	if *g.write || *g.dryRun {
		return nil
	}
	return ErrWriteModeUnstated
}

// DryRun reports whether this run writes nothing (the default unless -write was passed), the negation of Enabled.
func (g *WriteGate) DryRun() bool { return !*g.write }

// DryRunStated reports whether the operator passed -dry-run explicitly.
func (g *WriteGate) DryRunStated() bool { return *g.dryRun }

// Banner prints the fail-closed mode banner once, after flag checks and before slow or mutating work, and returns Enabled.
func (g *WriteGate) Banner() bool { return PrintWriteBanner(*g.write) }

// PrintWriteBanner prints the mode banner for a plain-bool write decision and returns it, so the banner text has one definition.
func PrintWriteBanner(write bool) bool {
	if write {
		fmt.Fprintln(os.Stderr, "═══ WRITING — applying changes ═══")
		return true
	}
	fmt.Fprintln(os.Stderr, "═══ DRY RUN — no writes; pass -write to apply ═══")
	return false
}

// RequireExplicitRange refuses an implicit full-history run (every bound at default) unless full is set; fs must already be parsed.
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

// SignalContext cancels on SIGINT/SIGTERM so long passes can flush a final checkpoint; a second signal gets the default action and kills a wedged flush.
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

// ResolveActor names who ran a privileged command (-actor, else OS login); an action with no actor is refused.
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

// SplitCSV splits a comma-separated flag value into trimmed, non-empty parts.
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

// AssertNonVacuous refuses to certify a verification that observed nothing; an empty scope or zero observed is an unexamined run, not a pass.
func AssertNonVacuous(observed, total int, what string) error {
	if total <= 0 {
		return fmt.Errorf("0 %s in scope — nothing to verify; refusing to certify a pass vacuously", what)
	}
	if observed <= 0 {
		return fmt.Errorf("0 of %d %s produced any observation — nothing was verified; refusing to certify a pass vacuously", total, what)
	}
	return nil
}

// Truncate shortens s to at most n bytes on a UTF-8 rune boundary, appending "...(truncated)" when it does.
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

// MkBackfillLogger returns the plain stderr text logger used for subcommand progress output.
func MkBackfillLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
}

// RangeChunk is one worker's slice of an overall [From,To] ledger range.
type RangeChunk struct{ From, To uint32 }

// RangeCursorKey is the ingestion_cursors sub_source for a range-scoped resume checkpoint; UpsertCursor is monotone-forward, so a key without its range lets a later range turn an earlier one into "nothing to do".
func RangeCursorKey(from, to uint32) string {
	return fmt.Sprintf("%d-%d", from, to)
}

// SplitRange divides [from,to] into n contiguous chunks, the last absorbing the remainder; it degrades to one chunk when n <= 1, the range is one ledger, or n exceeds the span.
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

// HistoricReadBucket resolves the bucket for a read over a backfilled range: -bucket, else the archive (live is trimmed), else live.
func HistoricReadBucket(cfg config.Config, override string) (string, error) {
	for _, b := range []string{override, cfg.Storage.S3BucketArchive, cfg.Storage.S3BucketLive} {
		if b != "" {
			return b, nil
		}
	}
	return "", fmt.Errorf("no bucket: set -bucket or storage.s3_bucket_archive / s3_bucket_live")
}

// ResolveStreamBucket picks the galexie bucket for a bounded walk: -bucket wins, else the live seam decides (a straddling range is an error), else live; a historic range on live would record a permanent hole as done.
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

// NewBoundedLedgerStreamConfig is the ledgerstream.Config for bounded walks that may reach the archive tip: it sets TolerateTrailingMissing (never override) and an explicit Buffered budget split across parallel walkers, since the default per-walker 10000-ledger queue OOM-killed the ops cap.
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
	// boundedWalkerBufferBudget is the total ledger read-ahead shared across concurrent bounded walkers (see NewBoundedLedgerStreamConfig for the OOM it prevents).
	boundedWalkerBufferBudget = 200

	// boundedWalkerBufferMin is the floor each walker's BufferSize is
	// clamped to, so a high -parallel doesn't starve any one walker's
	// read-ahead below what's needed to hide MinIO fetch latency.
	boundedWalkerBufferMin = 32

	// boundedWalkerNumWorkers is the fixed per-walker S3 GET concurrency, kept <= boundedWalkerBufferMin for the SDK's NumWorkers <= BufferSize invariant.
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
