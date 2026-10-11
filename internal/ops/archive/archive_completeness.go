package archive

import (
	"context"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/archivecompleteness"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// archiveCompleteness dispatches the `archive-completeness <mode>`
// subcommand per ADR-0017. Modes: check, fix, verify.
func archiveCompleteness(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("archive-completeness: subcommand required (check / fix / verify)")
	}
	if opsutil.IsHelpArg(args[0]) {
		return opsutil.Usage("usage: archive-completeness <check|fix|verify> [flags]")
	}
	switch args[0] {
	case "check":
		return archiveCompletenessCheck(args[1:])
	case "fix":
		return archiveCompletenessFix(args[1:])
	case "verify":
		return archiveCompletenessVerify(args[1:])
	default:
		return fmt.Errorf("archive-completeness: unknown mode %q (supported: check, fix, verify)", args[0])
	}
}

// archiveCompletenessVerifyOpts holds the parsed flags of archive-completeness verify, so a unit file's ExecStart argv can be proven parseable without running the phases.
type archiveCompletenessVerifyOpts struct {
	archiveRoot    string
	from, to       uint32
	workers        int
	ownerUser      string
	ownerGroup     string
	network        string
	outputFile     string
	textfileOutput string
	gate           *opsutil.WriteGate
}

// parseArchiveCompletenessVerifyFlags parses and validates the verify flags. -to has no usable zero value (a verify with no upper bound checks nothing), so 0 is refused.
func parseArchiveCompletenessVerifyFlags(args []string) (archiveCompletenessVerifyOpts, error) {
	fs := flag.NewFlagSet("archive-completeness verify", flag.ContinueOnError)
	archiveRoot := fs.String("archive-root", "/srv/history-archive",
		"Cross-anchor archive root.")
	from := fs.Uint("from", 2, "First ledger sequence (inclusive).")
	to := fs.Uint("to", 0, "Last ledger sequence (inclusive). Required.")
	workers := fs.Int("workers", 8, "Parallel fetch workers.")
	ownerUser := fs.String("owner-user", "stellar", "File owner user.")
	ownerGroup := fs.String("owner-group", "stellar", "File owner group.")
	network := fs.String("network", "pubnet",
		"Stellar network this archive belongs to. Cross-anchor FILL is PUBNET-ONLY (the built-in fallback sources are pubnet archives); a non-pubnet value makes the fill phase REFUSE rather than write pubnet checkpoints into a test-net store (audit 2026-08-26). Test nets self-heal archive gaps from their own galexie/core.")
	outputFile := fs.String("output-file", "",
		"Path to write JSON report. Empty = stdout.")
	textfileOutput := fs.String("textfile-output", "",
		"Path to write Prometheus textfile (node_exporter textfile_collector format). Empty = no metrics emit.")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return archiveCompletenessVerifyOpts{}, err
	}
	if *to == 0 {
		return archiveCompletenessVerifyOpts{}, fmt.Errorf("-to is required")
	}
	from32, to32, err := ledgerRangeUint32(*from, *to)
	if err != nil {
		return archiveCompletenessVerifyOpts{}, err
	}
	return archiveCompletenessVerifyOpts{
		archiveRoot:    *archiveRoot,
		from:           from32,
		to:             to32,
		workers:        *workers,
		ownerUser:      *ownerUser,
		ownerGroup:     *ownerGroup,
		network:        *network,
		outputFile:     *outputFile,
		textfileOutput: *textfileOutput,
		gate:           gate,
	}, nil
}

// ledgerRangeUint32 validates -from/-to and narrows them to ledger sequences;
// an out-of-range -to would otherwise wrap and scan a different range.
func ledgerRangeUint32(from, to uint) (uint32, uint32, error) {
	if from > to {
		return 0, 0, fmt.Errorf("-from (%d) must be <= -to (%d)", from, to)
	}
	if uint64(to) > math.MaxUint32 {
		return 0, 0, fmt.Errorf("-to (%d) exceeds the maximum ledger sequence (%d)", to, uint32(math.MaxUint32))
	}
	return uint32(from), uint32(to), nil
}

// archiveCompletenessVerify is the daily-cron mode: check, fix, re-check, then write a Prometheus textfile and the JSON Report.
// Exit 0: clean; 1: residual missing files after the fallback chain; other: I/O error.
func archiveCompletenessVerify(args []string) error {
	opts, err := parseArchiveCompletenessVerifyFlags(args)
	if err != nil {
		return err
	}
	write := opts.gate.Banner()

	startedAt := time.Now()
	// The fix phase fetches over HTTP and writes/renames/chowns into
	// -archive-root; a signal-aware ctx here lets a mid-fill SIGTERM/SIGINT
	// stop the fetch loop cleanly instead of running to completion regardless.
	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	// Phase 1 — initial check.
	checker := archivecompleteness.NewCrossAnchorChecker(opts.archiveRoot)
	preRes, err := checker.Check(opts.from, opts.to)
	if err != nil {
		return fmt.Errorf("initial cross-anchor check: %w", err)
	}

	report := archivecompleteness.NewReport(opts.from, opts.to)
	snapshot := archivecompleteness.NewMetricsSnapshot()

	// Phase 2 — fix any missing.
	fillRes, err := archiveCompletenessVerifyFill(ctx, write, preRes.Missing, archiveCompletenessVerifyFillOptions{
		ArchiveRoot: opts.archiveRoot,
		Workers:     opts.workers,
		OwnerUser:   opts.ownerUser,
		OwnerGroup:  opts.ownerGroup,
		Network:     opts.network,
	})
	if err != nil {
		return err
	}

	// Phase 3 — re-check; the state after the fill is what we report.
	postRes, err := checker.Check(opts.from, opts.to)
	if err != nil {
		return fmt.Errorf("post-fix cross-anchor check: %w", err)
	}
	report.SetCrossAnchor(opts.archiveRoot, postRes)

	// Set LastSuccessTimestamp ONLY when the post-fill state is clean AND non-vacuous (a range with no checkpoint verified nothing); alerts rely on the gauge going stale.
	// Leaving it zero does not drop the series: WriteTextfileAtomic carries the last clean run's timestamp forward.
	snapshot.PopulateFromReport(report)
	snapshot.PopulateFromFillResult(fillRes)
	snapshot.RunDurationSeconds = time.Since(startedAt).Seconds()
	vacuous := report.Vacuous()
	if !report.AnyMissing() && !vacuous {
		snapshot.LastSuccessTimestamp = startedAt
	}

	// Write JSON report (operator-readable diagnostic).
	if err := writeReport(report, opts.outputFile); err != nil {
		return err
	}

	// Write Prometheus textfile (node_exporter scrapes this dir).
	if opts.textfileOutput != "" {
		if err := archivecompleteness.WriteTextfileAtomic(opts.textfileOutput, snapshot); err != nil {
			return fmt.Errorf("write textfile: %w", err)
		}
		fmt.Fprintf(os.Stderr,
			"archive-completeness verify: metrics written to %s\n", opts.textfileOutput)
	}

	if vacuous {
		fmt.Fprintf(os.Stderr,
			"archive-completeness verify: range [%d, %d] contains no checkpoint position — nothing was verified, not a clean pass\n",
			opts.from, opts.to)
		return opsutil.ErrExitSilently
	}
	if report.AnyMissing() {
		fmt.Fprintf(os.Stderr,
			"archive-completeness verify: %d residual missing checkpoint(s); see report\n",
			report.CrossAnchor.MissingCount)
		// opsutil.ErrExitSilently: realMain's deferred flush MUST run before
		// the process exits, so we return rather than os.Exit. The
		// message above already explains the failure; the wrapper
		// suppresses its generic "archive-completeness: <err>" prefix.
		return opsutil.ErrExitSilently
	}
	fmt.Fprintf(os.Stderr,
		"archive-completeness verify: clean (%.1fs)\n", snapshot.RunDurationSeconds)
	return nil
}

// archiveCompletenessVerifyFillOptions carries the filler config for
// archiveCompletenessVerifyFill (split out of archiveCompletenessVerify's
// flag vars to keep the fill phase testable in isolation).
type archiveCompletenessVerifyFillOptions struct {
	ArchiveRoot string
	Workers     int
	OwnerUser   string
	OwnerGroup  string
	Network     string
}

// archiveCompletenessVerifyFill runs verify's Phase 2: a no-op when
// nothing is missing, a dry-run notice when -write was not passed, or
// an actual fetch-and-place otherwise.
func archiveCompletenessVerifyFill(ctx context.Context, write bool, missing []uint32, opts archiveCompletenessVerifyFillOptions) (archivecompleteness.FillResult, error) {
	switch {
	case len(missing) == 0:
		// Nothing to fix.
		return archivecompleteness.FillResult{}, nil
	case !write:
		// This mode runs unattended from the systemd timer with no preview, so a stale -archive-root or bad mount would fetch into the wrong tree.
		// Fail-closed DRY RUN by default; the shipped units pass -write.
		fmt.Fprintf(os.Stderr,
			"archive-completeness verify: DRY RUN — %d missing checkpoint(s) found, not fixed (pass -write to apply)\n",
			len(missing))
		return archivecompleteness.FillResult{}, nil
	default:
		filler, err := archivecompleteness.NewCrossAnchorFiller(archivecompleteness.FillerOptions{
			ArchiveRoot: opts.ArchiveRoot,
			Workers:     opts.Workers,
			OwnerUser:   opts.OwnerUser,
			OwnerGroup:  opts.OwnerGroup,
			Network:     opts.Network,
		})
		if err != nil {
			return archivecompleteness.FillResult{}, fmt.Errorf("filler: %w", err)
		}
		fillRes := filler.Fill(ctx, missing)
		fmt.Fprintf(os.Stderr,
			"archive-completeness verify: filled %d / %d missing checkpoints (workers=%d)\n",
			fillRes.Filled, len(missing), opts.Workers)
		return fillRes, nil
	}
}

// archiveCompletenessFix runs check, then fetches every missing checkpoint via the multi-source fallback chain; it mutates nothing until a file is confirmed missing.
// Exit 0: all placed; 1: some still missing; other: I/O or config error.
func archiveCompletenessFix(args []string) error {
	fs := flag.NewFlagSet("archive-completeness fix", flag.ContinueOnError)
	archiveRoot := fs.String("archive-root", "/srv/history-archive",
		"Cross-anchor archive root (default: /srv/history-archive).")
	from := fs.Uint("from", 2, "First ledger sequence (inclusive).")
	to := fs.Uint("to", 0, "Last ledger sequence (inclusive). Required.")
	workers := fs.Int("workers", 8, "Parallel fetch workers (default 8).")
	ownerUser := fs.String("owner-user", "stellar",
		"Local user that should own placed files. Empty disables chown.")
	ownerGroup := fs.String("owner-group", "stellar",
		"Local group that should own placed files. Empty disables chown.")
	network := fs.String("network", "pubnet",
		"Stellar network this archive belongs to. Cross-anchor FILL is PUBNET-ONLY (the built-in fallback sources are pubnet archives); a non-pubnet value makes the fill phase REFUSE rather than write pubnet checkpoints into a test-net store (audit 2026-08-26). Test nets self-heal archive gaps from their own galexie/core.")
	outputFile := fs.String("output-file", "",
		"Path to write JSON post-fix report. Default: stdout.")
	gate := opsutil.RegisterWriteGate(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *to == 0 {
		return fmt.Errorf("-to is required (pass the network head ledger sequence)")
	}
	from32, to32, err := ledgerRangeUint32(*from, *to)
	if err != nil {
		return err
	}
	write := gate.Banner()

	// A signal-aware ctx lets a mid-fill SIGTERM/SIGINT stop the fetch
	// loop instead of letting it run to completion regardless.
	ctx, cancel := opsutil.SignalContext()
	defer cancel()

	// Phase 1 — check: enumerate the missing list.
	checker := archivecompleteness.NewCrossAnchorChecker(*archiveRoot)
	res, err := checker.Check(from32, to32)
	if err != nil {
		return fmt.Errorf("cross-anchor check: %w", err)
	}

	report := archivecompleteness.NewReport(from32, to32)
	report.SetCrossAnchor(*archiveRoot, res)

	if report.Vacuous() {
		// [from, to] contained no checkpoint position at all —
		// nothing to fix because nothing was checked. Do not report
		// this as "already complete".
		if err := writeReport(report, *outputFile); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr,
			"archive-completeness fix: range [%d, %d] contains no checkpoint position — nothing was verified\n",
			*from, *to)
		return opsutil.ErrExitSilently
	}
	if len(res.Missing) == 0 {
		// Already complete; nothing to do.
		return writeReport(report, *outputFile)
	}
	if !write {
		// The fill HTTP-fetches and writes into -archive-root with no preview, so a stale default or mis-templated mount would write into the wrong tree unseen.
		// Fail-closed DRY RUN by default.
		fmt.Fprintf(os.Stderr,
			"archive-completeness fix: DRY RUN — %d missing checkpoint(s) would be fetched via the fallback chain (pass -write to apply)\n",
			len(res.Missing))
		if err := writeReport(report, *outputFile); err != nil {
			return err
		}
		return opsutil.ErrExitSilently
	}

	// Phase 2 — fix: fetch each missing checkpoint via the
	// multi-source fallback chain.
	filler, err := archivecompleteness.NewCrossAnchorFiller(archivecompleteness.FillerOptions{
		ArchiveRoot: *archiveRoot,
		Workers:     *workers,
		OwnerUser:   *ownerUser,
		OwnerGroup:  *ownerGroup,
		Network:     *network,
	})
	if err != nil {
		return fmt.Errorf("filler: %w", err)
	}
	fillRes := filler.Fill(ctx, res.Missing)
	fmt.Fprintf(os.Stderr,
		"archive-completeness fix: %d filled / %d failed (workers=%d)\n",
		fillRes.Filled, len(fillRes.Failed), *workers)
	for source, count := range fillRes.PerSourceSuccess {
		fmt.Fprintf(os.Stderr, "  source %s: %d fetched\n", source, count)
	}
	for _, f := range fillRes.Failed {
		fmt.Fprintf(os.Stderr, "  FAILED seq=%d reason=%s\n", f.Seq, f.Reason)
	}

	// Phase 3 — re-check: after the fill, scan again so the report
	// reflects the state after the fill. The Filler is idempotent (next run
	// will just skip files now present), so the re-check is the
	// authoritative measure of what's still missing.
	postRes, err := checker.Check(from32, to32)
	if err != nil {
		return fmt.Errorf("post-fix cross-anchor check: %w", err)
	}
	report.SetCrossAnchor(*archiveRoot, postRes)

	if err := writeReport(report, *outputFile); err != nil {
		return err
	}
	if report.AnyMissing() {
		fmt.Fprintf(os.Stderr,
			"archive-completeness fix: %d checkpoint(s) still missing after fallback chain — see report\n",
			report.CrossAnchor.MissingCount)
		// opsutil.ErrExitSilently: see archiveCompletenessVerify for rationale.
		return opsutil.ErrExitSilently
	}
	return nil
}

// writeReport encodes the Report to outputFile (or stdout when empty).
func writeReport(report *archivecompleteness.Report, outputFile string) error {
	var w io.Writer = os.Stdout
	if outputFile != "" {
		f, err := os.Create(outputFile) //nolint:gosec // operator-supplied path
		if err != nil {
			return fmt.Errorf("create output file: %w", err)
		}
		defer func() { _ = f.Close() }()
		w = f
	}
	return report.WriteJSON(w)
}

// archiveCompletenessCheck is the read-only `check` mode: walks the cross-anchor archive and emits a JSON [archivecompleteness.Report].
// Exit 0: clean; 1: missing files in some section; other: I/O or config error.
func archiveCompletenessCheck(args []string) error {
	fs := flag.NewFlagSet("archive-completeness check", flag.ContinueOnError)
	archiveRoot := fs.String("archive-root", "/srv/history-archive",
		"Cross-anchor archive root (default: /srv/history-archive).")
	from := fs.Uint("from", 2, "First ledger sequence (inclusive).")
	to := fs.Uint("to", 0,
		"Last ledger sequence (inclusive). Required — pass the network head.")
	outputFile := fs.String("output-file", "",
		"Path to write JSON report. Default: stdout.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *to == 0 {
		return fmt.Errorf("-to is required (pass the network head ledger sequence)")
	}
	from32, to32, err := ledgerRangeUint32(*from, *to)
	if err != nil {
		return err
	}

	report := archivecompleteness.NewReport(from32, to32)

	checker := archivecompleteness.NewCrossAnchorChecker(*archiveRoot)
	res, err := checker.Check(from32, to32)
	if err != nil {
		return fmt.Errorf("cross-anchor scan: %w", err)
	}
	report.SetCrossAnchor(*archiveRoot, res)

	// Cross-anchor only; the Primary section stays nil.

	if err := writeReport(report, *outputFile); err != nil {
		return err
	}

	// A range with no checkpoint position at all verified
	// nothing — must not read as a clean pass.
	if report.Vacuous() {
		fmt.Fprintf(os.Stderr,
			"archive-completeness check: range [%d, %d] contains no checkpoint position — nothing was verified\n",
			*from, *to)
		return opsutil.ErrExitSilently
	}
	// Non-zero exit when anything is missing so cron / k8s Job
	// invocations surface gaps as a Prometheus-style probe.
	if report.AnyMissing() {
		fmt.Fprintf(os.Stderr,
			"archive-completeness check: %d missing checkpoint(s) in cross-anchor archive (range [%d, %d])\n",
			report.CrossAnchor.MissingCount, *from, *to)
		// opsutil.ErrExitSilently: see archiveCompletenessVerify for rationale.
		return opsutil.ErrExitSilently
	}
	return nil
}
