package archive

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/archivecompleteness"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
)

// archiveCompleteness dispatches the `archive-completeness <mode>`
// subcommand per ADR-0017. Modes: check (PR A), fix (PR B),
// verify (PR C — this PR).
func archiveCompleteness(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("archive-completeness: subcommand required (check / fix / verify)")
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

// archiveCompletenessVerify is the daily-cron mode: runs check →
// fix → re-check, then emits a Prometheus textfile for
// node_exporter's textfile_collector to scrape. Also writes the
// JSON Report.
//
// This is the canonical command the systemd timer fires:
//
//	stellarindex-ops archive-completeness verify \
//	  -from 2 -to <network_head> \
//	  -textfile-output /var/lib/node_exporter/textfile_collector/archive_completeness.prom \
//	  -output-file /var/lib/galexie/last-completeness-report.json
//
// Exit semantics:
//   - 0: clean (no missing files after fix)
//   - 1: residual missing files (fallback chain exhausted some)
//   - other: I/O error
func archiveCompletenessVerify(args []string) error {
	fs := flag.NewFlagSet("archive-completeness verify", flag.ContinueOnError)
	archiveRoot := fs.String("archive-root", "/srv/history-archive",
		"Cross-anchor archive root.")
	from := fs.Uint("from", 2, "First ledger sequence (inclusive).")
	to := fs.Uint("to", 0, "Last ledger sequence (inclusive). Required.")
	workers := fs.Int("workers", 8, "Parallel fetch workers.")
	ownerUser := fs.String("owner-user", "stellar", "File owner user.")
	ownerGroup := fs.String("owner-group", "stellar", "File owner group.")
	outputFile := fs.String("output-file", "",
		"Path to write JSON report. Empty = stdout.")
	textfileOutput := fs.String("textfile-output", "",
		"Path to write Prometheus textfile (node_exporter textfile_collector format). Empty = no metrics emit.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *to == 0 {
		return fmt.Errorf("-to is required")
	}
	if uint64(*from) > uint64(*to) {
		return fmt.Errorf("-from (%d) must be <= -to (%d)", *from, *to)
	}

	startedAt := time.Now()

	// Phase 1 — initial check.
	checker := archivecompleteness.NewCrossAnchorChecker(*archiveRoot)
	preRes, err := checker.Check(uint32(*from), uint32(*to))
	if err != nil {
		return fmt.Errorf("initial cross-anchor check: %w", err)
	}

	report := archivecompleteness.NewReport(uint32(*from), uint32(*to))
	snapshot := archivecompleteness.NewMetricsSnapshot()

	// Phase 2 — fix any missing.
	var fillRes archivecompleteness.FillResult
	if len(preRes.Missing) > 0 {
		filler, err := archivecompleteness.NewCrossAnchorFiller(archivecompleteness.FillerOptions{
			ArchiveRoot: *archiveRoot,
			Workers:     *workers,
			OwnerUser:   *ownerUser,
			OwnerGroup:  *ownerGroup,
		})
		if err != nil {
			return fmt.Errorf("filler: %w", err)
		}
		fillRes = filler.Fill(context.Background(), preRes.Missing)
		fmt.Fprintf(os.Stderr,
			"archive-completeness verify: filled %d / %d missing checkpoints (workers=%d)\n",
			fillRes.Filled, len(preRes.Missing), *workers)
	}

	// Phase 3 — re-check; the post-fix state is what we report.
	postRes, err := checker.Check(uint32(*from), uint32(*to))
	if err != nil {
		return fmt.Errorf("post-fix cross-anchor check: %w", err)
	}
	report.SetCrossAnchor(*archiveRoot, postRes)

	// Populate metrics. LastSuccessTimestamp is set ONLY when the
	// post-fix state is clean — alert rules rely on this gauge
	// going stale when something's wrong.
	snapshot.PopulateFromReport(report)
	snapshot.PopulateFromFillResult(fillRes)
	snapshot.RunDurationSeconds = time.Since(startedAt).Seconds()
	if !report.AnyMissing() {
		snapshot.LastSuccessTimestamp = startedAt
	}

	// Write JSON report (operator-readable diagnostic).
	if err := writeReport(report, *outputFile); err != nil {
		return err
	}

	// Write Prometheus textfile (node_exporter scrapes this dir).
	if *textfileOutput != "" {
		if err := archivecompleteness.WriteTextfileAtomic(*textfileOutput, snapshot); err != nil {
			return fmt.Errorf("write textfile: %w", err)
		}
		fmt.Fprintf(os.Stderr,
			"archive-completeness verify: metrics written to %s\n", *textfileOutput)
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

// archiveCompletenessFix runs the `check` then fetches every
// missing checkpoint via the multi-source fallback chain. Read-
// then-write — does NOT mutate either archive without first
// confirming the file is missing.
//
// Exit semantics:
//   - 0: every previously-missing file has been placed
//   - 1: some files still missing after exhausting the chain
//   - other: I/O / config error
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
	outputFile := fs.String("output-file", "",
		"Path to write JSON post-fix report. Default: stdout.")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *to == 0 {
		return fmt.Errorf("-to is required (pass the network head ledger sequence)")
	}
	if uint64(*from) > uint64(*to) {
		return fmt.Errorf("-from (%d) must be <= -to (%d)", *from, *to)
	}

	// Phase 1 — check: enumerate the missing list.
	checker := archivecompleteness.NewCrossAnchorChecker(*archiveRoot)
	res, err := checker.Check(uint32(*from), uint32(*to))
	if err != nil {
		return fmt.Errorf("cross-anchor check: %w", err)
	}

	report := archivecompleteness.NewReport(uint32(*from), uint32(*to))
	report.SetCrossAnchor(*archiveRoot, res)

	if len(res.Missing) == 0 {
		// Already complete; nothing to do.
		return writeReport(report, *outputFile)
	}

	// Phase 2 — fix: fetch each missing checkpoint via the
	// multi-source fallback chain.
	filler, err := archivecompleteness.NewCrossAnchorFiller(archivecompleteness.FillerOptions{
		ArchiveRoot: *archiveRoot,
		Workers:     *workers,
		OwnerUser:   *ownerUser,
		OwnerGroup:  *ownerGroup,
	})
	if err != nil {
		return fmt.Errorf("filler: %w", err)
	}
	fillRes := filler.Fill(context.Background(), res.Missing)
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
	// reflects post-fix state. The Filler is idempotent (next run
	// will just skip files now present), so the re-check is the
	// authoritative measure of what's still missing.
	postRes, err := checker.Check(uint32(*from), uint32(*to))
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

// archiveCompletenessCheck implements the read-only `check` mode.
// Walks the cross-anchor archive (PR A; the primary archive scan
// lands in PR B), emits a JSON [archivecompleteness.Report].
//
// Exit semantics:
//   - 0: every section clean (no missing files in scope)
//   - 1: at least one section reported missing files
//   - other: I/O / config error before scan completed
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
	if uint64(*from) > uint64(*to) {
		return fmt.Errorf("-from (%d) must be <= -to (%d)", *from, *to)
	}

	report := archivecompleteness.NewReport(uint32(*from), uint32(*to))

	checker := archivecompleteness.NewCrossAnchorChecker(*archiveRoot)
	res, err := checker.Check(uint32(*from), uint32(*to))
	if err != nil {
		return fmt.Errorf("cross-anchor scan: %w", err)
	}
	report.SetCrossAnchor(*archiveRoot, res)

	// PR A scope: cross-anchor only. Primary section stays nil; PR B
	// will populate it.

	if err := writeReport(report, *outputFile); err != nil {
		return err
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
