package ingest

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/ops/opsutil"
	"github.com/Stellar-Index/StellarIndex/internal/projector"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// findDataGapsDefaultMinGapSize (1000 ledgers, ~1.5 h) filters legitimate no-event stretches. find-data-gaps scans soroban_events
// directly because cursor-derived density can read 100% while data is missing.
const findDataGapsDefaultMinGapSize = int64(1000)

type findDataGapsReport struct {
	ScannedAt time.Time `json:"scanned_at"`
	Source    string    `json:"source"`
	// ProjectorSource is the target's SourceNetKey (CanonicalSource, else Source): the name
	// projector-replay -source accepts when the target is projected.
	ProjectorSource     string                `json:"projector_source,omitempty"`
	Table               string                `json:"table"`
	MinGapSize          int64                 `json:"min_gap_size"`
	FromLedger          int64                 `json:"from_ledger"`
	ToLedger            int64                 `json:"to_ledger"`
	Gaps                []timescale.LedgerGap `json:"gaps"`
	TotalMissingLedgers int64                 `json:"total_missing_ledgers"`
}

// findDataGapsMultiReport groups one report per per-source target (find-data-gaps --source all, the default).
type findDataGapsMultiReport struct {
	ScannedAt time.Time            `json:"scanned_at"`
	Reports   []findDataGapsReport `json:"reports"`
}

func findDataGaps(args []string) error {
	fs := flag.NewFlagSet("find-data-gaps", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to stellarindex.toml (required)")
	from := fs.Int64("from", 0,
		"scan from this ledger (default 0 = each source's genesis ledger; "+
			"a value below a source's genesis is raised to it)")
	to := fs.Int64("to", 0,
		"scan to this ledger (default 0 = current tip from the live cursor)")
	minGapSize := fs.Int64("min-gap-size", findDataGapsDefaultMinGapSize,
		"only report gaps >= this many contiguous ledgers. Unset, each "+
			"source uses its own threshold (1000 unless the source overrides "+
			"it), filtering out legitimate no-activity stretches; lower "+
			"values surface more noise from quiet periods.")
	output := fs.String("output", "text",
		"output format: text (operator-friendly) or json (machine-readable plan)")
	source := fs.String("source", "all",
		"which per-source target(s) to scan: a single source name "+
			"(e.g. blend-positions, soroban-events), \"all\" (every "+
			"registered target — default), or a comma-separated subset")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config required")
	}
	if *minGapSize < 1 {
		return fmt.Errorf("-min-gap-size (%d) must be >= 1", *minGapSize)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if !set["min-gap-size"] {
		*minGapSize = 0
	}
	if *output != "text" && *output != "json" {
		return fmt.Errorf("-output (%q) must be \"text\" or \"json\"", *output)
	}

	targets, err := ResolveFindDataGapsTargets(*source)
	if err != nil {
		return err
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	rootCtx, cancel := opsutil.SignalContext()
	defer cancel()

	store, err := timescale.Open(rootCtx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer func() { _ = store.Close() }()

	// Default `to` = live cursor's last_ledger (current tip).
	if *to == 0 {
		c, err := store.GetCursor(rootCtx, "ledgerstream", "")
		if err != nil {
			return fmt.Errorf("resolve tip from live cursor: %w", err)
		}
		*to = int64(c.LastLedger)
	}
	if *to < *from {
		return fmt.Errorf("-to (%d) < -from (%d)", *to, *from)
	}

	multi, err := scanDataGaps(rootCtx, store, targets, *from, *to, *minGapSize, time.Now().UTC())
	if err != nil {
		return err
	}

	switch *output {
	case "json":
		return writeFindDataGapsJSON(os.Stdout, multi)
	default:
		writeFindDataGapsMultiText(os.Stdout, multi)
		return nil
	}
}

// ledgerRangeGapFinder is the store seam find-data-gaps scans through.
type ledgerRangeGapFinder interface {
	FindLedgerGapsInRange(ctx context.Context, target timescale.GapDetectorTarget, from, to, minGapSize int64) ([]timescale.LedgerGap, error)
}

// scanDataGaps scans each target over [max(from, Genesis), to] with both edges bracketed, so a hole running to the tip, one
// starting at the source's genesis, or a range with no rows at all is reported rather than read as clean.
// minGapSize 0 means each target's own threshold, so a naturally quiet source is not reported as a gap.
func scanDataGaps(ctx context.Context, store ledgerRangeGapFinder, targets []timescale.GapDetectorTarget,
	from, to, minGapSize int64, scannedAt time.Time,
) (findDataGapsMultiReport, error) {
	multi := findDataGapsMultiReport{ScannedAt: scannedAt}
	for _, target := range targets {
		lo := max(from, target.Genesis)
		size := minGapSize
		if size == 0 {
			size = target.EffectiveMinGapSize()
		}
		report := findDataGapsReport{
			ScannedAt:       scannedAt,
			Source:          target.Source,
			ProjectorSource: target.SourceNetKey(),
			Table:           target.Table,
			MinGapSize:      size,
			FromLedger:      lo,
			ToLedger:        to,
		}
		if lo <= to {
			gaps, err := store.FindLedgerGapsInRange(ctx, target, lo, to, size)
			if err != nil {
				return multi, fmt.Errorf("find gaps source=%s table=%s: %w", target.Source, target.Table, err)
			}
			report.Gaps = gaps
			for _, g := range gaps {
				report.TotalMissingLedgers += g.Size
			}
		}
		multi.Reports = append(multi.Reports, report)
	}
	return multi, nil
}

// ResolveFindDataGapsTargets maps --source ("all", a name, or a comma list) to targets; an unmatched name is a hard error.
func ResolveFindDataGapsTargets(source string) ([]timescale.GapDetectorTarget, error) {
	if source == "" || source == "all" {
		return timescale.DefaultGapDetectorTargets, nil
	}
	wanted := make(map[string]bool)
	for _, s := range opsutil.SplitCSV(source) {
		wanted[s] = true
	}
	out := make([]timescale.GapDetectorTarget, 0, len(wanted))
	matched := make(map[string]bool, len(wanted))
	for _, t := range timescale.DefaultGapDetectorTargets {
		if wanted[t.Source] {
			out = append(out, t)
			matched[t.Source] = true
		}
	}
	for name := range wanted {
		if !matched[name] {
			return nil, fmt.Errorf("-source %q does not match any registered gap-detector target", name)
		}
	}
	return out, nil
}

func writeFindDataGapsJSON(w io.Writer, m findDataGapsMultiReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(m)
}

// writeFindDataGapsMultiText emits one block per target, including zero-gap ones, so the operator can confirm every target was scanned.
func writeFindDataGapsMultiText(w io.Writer, m findDataGapsMultiReport) {
	// fmt.Fprint{,f,ln} errors on operator output are not actionable — a broken-pipe on operator-CLI output is the operator's
	// terminal, not a system fault — so we swallow them via _, _ = ... rather than threading an error all the way up to main.
	_, _ = fmt.Fprintf(w, "find-data-gaps: %d targets scanned at %s\n",
		len(m.Reports), m.ScannedAt.Format(time.RFC3339))
	for _, r := range m.Reports {
		writeFindDataGapsText(w, r)
	}
}

func writeFindDataGapsText(w io.Writer, r findDataGapsReport) {
	_, _ = fmt.Fprintf(w,
		"\n  source=%s table=%s ledgers=[%d, %d] min_gap_size=%d\n",
		r.Source, r.Table, r.FromLedger, r.ToLedger, r.MinGapSize)
	if r.FromLedger > r.ToLedger {
		_, _ = fmt.Fprintln(w, "    range ends before this source's genesis ledger; nothing scanned")
		return
	}
	if len(r.Gaps) == 0 {
		_, _ = fmt.Fprintf(w, "    no gap >= %d ledgers in [%d, %d]\n", r.MinGapSize, r.FromLedger, r.ToLedger)
		return
	}
	_, _ = fmt.Fprintf(w, "    %d gap(s), totalling %d missing ledgers:\n", len(r.Gaps), r.TotalMissingLedgers)
	for i, g := range r.Gaps {
		_, _ = fmt.Fprintf(w, "      %2d  [%d, %d]  size=%d ledgers\n", i+1, g.Start, g.End, g.Size)
	}
	_, _ = fmt.Fprintln(w, "    Targeted backfill plan:")
	for i, g := range r.Gaps {
		// soroban-events uses the binary backfill subcommand; projected per-source tables are rewound with
		// projector-replay (ADR-0032); non-projected sources (sdex, external) use their own paths.
		switch r.Source {
		case "soroban-events":
			_, _ = fmt.Fprintf(w,
				"      %2d  stellarindex-ops backfill -write --config /etc/stellarindex.toml --from %d --to %d --source soroban-events\n",
				i+1, g.Start, g.End)
		case "sdex":
			_, _ = fmt.Fprintf(w,
				"      %2d  stellarindex-ops backfill -write --config /etc/stellarindex.toml --from %d --to %d --source sdex\n",
				i+1, g.Start, g.End)
		default:
			// r.ProjectorSource is SourceNetKey; KnownProjectorSources is buildSource's own registered set, so
			// this cannot drift like a hand-copied switch (a false "not projected" message would stop the operator trying the real command).
			if _, ok := projector.KnownProjectorSources[r.ProjectorSource]; ok {
				_, _ = fmt.Fprintf(w,
					"      %2d  stellarindex-ops projector-replay --config /etc/stellarindex.toml --source %s --from %d\n",
					i+1, r.ProjectorSource, g.Start)
				continue
			}
			_, _ = fmt.Fprintf(w,
				"      %2d  # target %q has no direct projector source — see internal/projector/registry.go\n"+
					"          # for the source that writes this table, then:\n"+
					"          # stellarindex-ops projector-replay --config /etc/stellarindex.toml --source <SOURCE> --from %d\n",
				i+1, r.Source, g.Start)
		}
	}
}
