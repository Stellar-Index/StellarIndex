package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/RatesEngine/rates-engine/internal/config"
	"github.com/RatesEngine/rates-engine/internal/storage/timescale"
)

// find-data-gaps — data-derived gap detector for Soroban-era ledger
// ingest coverage.
//
// Why this exists. The cursor-derived density projection in
// `/v1/diagnostics/ingestion` measures process state ("did we walk
// this ledger") and can read 100% while underlying data is missing.
// This subcommand scans the soroban_events hypertable directly and
// reports contiguous ledger-coverage gaps >= --min-gap-size.
// Operators use the output as the input to a *targeted* backfill —
// `ratesengine-ops backfill --from <gap.start> --to <gap.end>
// --source soroban-events` — rather than re-running every stalled
// cursor on faith.
//
// The threshold default of 1000 ledgers filters out the legitimate
// "no Soroban contract emitted in this block" stretches that are
// expected on mainnet (Soroban activity is dense but not gap-free).
// A 1000-ledger contiguous gap is ~1.5 h of network time and
// implies an ingest-side failure, not network quiet.
//
// JSON output mode (`--output json`) emits a plan-shaped document
// ready to feed automation; text mode is the operator-friendly
// default. Both modes report `total_missing_ledgers` so a fleet of
// invocations can be aggregated for a global gap-size dashboard.
const findDataGapsDefaultMinGapSize = int64(1000)

type findDataGapsReport struct {
	ScannedAt           time.Time             `json:"scanned_at"`
	MinGapSize          int64                 `json:"min_gap_size"`
	FromLedger          int64                 `json:"from_ledger"`
	ToLedger            int64                 `json:"to_ledger"`
	Gaps                []timescale.LedgerGap `json:"gaps"`
	TotalMissingLedgers int64                 `json:"total_missing_ledgers"`
}

func findDataGaps(args []string) error {
	fs := flag.NewFlagSet("find-data-gaps", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "path to ratesengine.toml (required)")
	from := fs.Int64("from", 0,
		"scan from this ledger (default 0 = first ledger in soroban_events). "+
			"Soroban era starts at L50,457,424 on pubnet; values earlier than "+
			"that yield no gaps because the table is empty there.")
	to := fs.Int64("to", 0,
		"scan to this ledger (default 0 = current tip from the live cursor)")
	minGapSize := fs.Int64("min-gap-size", findDataGapsDefaultMinGapSize,
		"only report gaps >= this many contiguous ledgers. 1000 default "+
			"filters out legitimate no-Soroban-activity stretches; lower "+
			"values surface more noise from quiet periods.")
	output := fs.String("output", "text",
		"output format: text (operator-friendly) or json (machine-readable plan)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return errors.New("-config required")
	}
	if *minGapSize < 1 {
		return fmt.Errorf("-min-gap-size (%d) must be >= 1", *minGapSize)
	}
	if *output != "text" && *output != "json" {
		return fmt.Errorf("-output (%q) must be \"text\" or \"json\"", *output)
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	rootCtx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
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

	gaps, err := store.FindSorobanEventsLedgerGaps(rootCtx, *from, *to, *minGapSize)
	if err != nil {
		return fmt.Errorf("find gaps: %w", err)
	}
	report := findDataGapsReport{
		ScannedAt:  time.Now().UTC(),
		MinGapSize: *minGapSize,
		FromLedger: *from,
		ToLedger:   *to,
		Gaps:       gaps,
	}
	for _, g := range gaps {
		report.TotalMissingLedgers += g.Size
	}

	switch *output {
	case "json":
		return writeFindDataGapsJSON(report)
	default:
		writeFindDataGapsText(report)
		return nil
	}
}

func writeFindDataGapsJSON(r findDataGapsReport) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

func writeFindDataGapsText(r findDataGapsReport) {
	// fmt.Fprint{,f,ln} errors against os.Stdout are not actionable —
	// a broken-pipe on operator-CLI output is the operator's terminal,
	// not a system fault — so we swallow them via _, _ = ... rather
	// than threading an error all the way up to main.
	_, _ = fmt.Fprintf(os.Stdout,
		"find-data-gaps: scanned [%d, %d] for gaps >= %d ledgers\n",
		r.FromLedger, r.ToLedger, r.MinGapSize)
	if len(r.Gaps) == 0 {
		_, _ = fmt.Fprintln(os.Stdout, "  no gaps found — soroban_events coverage clean above the threshold")
		return
	}
	_, _ = fmt.Fprintf(os.Stdout, "  %d gap(s), totalling %d missing ledgers:\n", len(r.Gaps), r.TotalMissingLedgers)
	for i, g := range r.Gaps {
		_, _ = fmt.Fprintf(os.Stdout, "    %2d  [%d, %d]  size=%d ledgers\n", i+1, g.Start, g.End, g.Size)
	}
	_, _ = fmt.Fprintln(os.Stdout)
	_, _ = fmt.Fprintln(os.Stdout, "  Targeted backfill plan:")
	for i, g := range r.Gaps {
		_, _ = fmt.Fprintf(os.Stdout,
			"    %2d  ratesengine-ops backfill --config /etc/ratesengine.toml --from %d --to %d --source soroban-events\n",
			i+1, g.Start, g.End)
	}
}
