package ingest

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/projector"
	"github.com/Stellar-Index/StellarIndex/internal/stellarrpc"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// detectGaps compares every LIVE per-source cursor ([timescale.LiveCursorSources]) against the stellar-rpc tip; exits non-zero on lag
// past threshold or no cursor. One-shot namespaces are excluded (last_ledger is a range end); multi-cursor sources use the MINIMUM.
func detectGaps(args []string) error {
	fs := flag.NewFlagSet("detect-gaps", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	threshold := fs.Uint("threshold", 100, "Ledgers of lag that count as a gap")
	rpcOverride := fs.String("rpc", "", "stellar-rpc endpoint URL for the network tip (overrides stellar.rpc_endpoints[0])")
	rpcMaxStaleness := fs.Duration("rpc-max-staleness", 5*time.Minute,
		"Max age of the RPC tip's own closeTime before the tip is treated as stale (catches a stuck/disconnected RPC node, not normal ~5s ledger-close jitter)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *cfgPath == "" {
		return fmt.Errorf("-config is required")
	}

	cfg, err := config.LoadWithEnv(*cfgPath)
	if err != nil {
		return err
	}

	// Derived from config alone — cheap, and fails fast on a broken
	// projector config before we ever touch the network or storage.
	expectedProjected, err := expectedProjectorSources(cfg)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// One-shot probe: no failover, just -rpc or the first configured endpoint.
	endpoint := *rpcOverride
	if endpoint == "" && len(cfg.Stellar.RPCEndpoints) > 0 {
		endpoint = cfg.Stellar.RPCEndpoints[0]
	}
	if endpoint == "" {
		return fmt.Errorf("no RPC endpoint — set -rpc or stellar.rpc_endpoints")
	}
	rpc := stellarrpc.New(endpoint, stellarrpc.WithTimeout(5*time.Second))
	tip, err := rpc.LatestLedger(ctx)
	if err != nil {
		return fmt.Errorf("rpc: %w", err)
	}

	closeTime, err := parseRPCCloseTime(tip.CloseTime)
	if err != nil {
		return fmt.Errorf("rpc tip closeTime: %w — cannot assert freshness, failing closed", err)
	}
	if staleness := time.Since(closeTime); staleness > *rpcMaxStaleness {
		return fmt.Errorf("rpc tip stale: sequence %d closed %s ago (closeTime %s UTC) — exceeds -rpc-max-staleness %s; the RPC node itself may be stuck or disconnected, which would otherwise make every source cursor read \"ok\" against a frozen tip",
			tip.Sequence, staleness.Round(time.Second), closeTime.UTC().Format(time.RFC3339), *rpcMaxStaleness)
	}

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer func() { _ = store.Close() }()

	cursors, err := store.ListCursors(ctx)
	if err != nil {
		return err
	}

	minBySource := minLedgerBySource(cursors)
	if len(minBySource) == 0 {
		// An empty or all-one-shot cursor table is the "every live source is stalled" state this probe exists to catch; it must not read as ok.
		return fmt.Errorf("no live cursor (%v) found against tip %d — ingest may never have started or every live cursor was lost",
			timescale.LiveCursorSources(), tip.Sequence)
	}

	lagging := writeGapReport(os.Stdout, minBySource, tip.Sequence, uint32(*threshold))

	missing := catalogueMissingProjectorSources(cursors, expectedProjected)
	if len(missing) > 0 {
		_, _ = fmt.Fprintf(os.Stdout, "MISSING (catalogued in ingestion.enabled_sources, no ingestion_cursors row — reaped or never started): %v\n", missing)
	}

	switch {
	case len(lagging) > 0 && len(missing) > 0:
		return fmt.Errorf("%d source(s) lagging past threshold %d (%v); %d catalogued source(s) missing a cursor (%v)",
			len(lagging), *threshold, lagging, len(missing), missing)
	case len(lagging) > 0:
		return fmt.Errorf("%d source(s) lagging past threshold %d: %v",
			len(lagging), *threshold, lagging)
	case len(missing) > 0:
		return fmt.Errorf("%d catalogued source(s) missing a cursor: %v — reaped or never started",
			len(missing), missing)
	}
	return nil
}

// expectedProjectorSources returns the ("projector", name) cursors the config commits to running, or nil when the projector is off.
// It builds the real registry so it stays in sync with buildSource's dispatch; only Source.Name is read.
func expectedProjectorSources(cfg config.Config) ([]string, error) {
	if !cfg.Ingestion.Projector.Enabled {
		return nil, nil
	}
	registry, err := projector.BuildRegistry(cfg.Ingestion.EnabledSources, cfg.Oracle, cfg.Supply.WatchedSEP41Contracts, nil)
	if err != nil {
		return nil, fmt.Errorf("projector registry: %w", err)
	}
	names := make([]string, len(registry.Sources))
	for i, s := range registry.Sources {
		names[i] = s.Name
	}
	return names, nil
}

// catalogueMissingProjectorSources returns names in expected with no ("projector", name) cursor row, which would otherwise vanish from the verdict.
func catalogueMissingProjectorSources(cursors []timescale.Cursor, expected []string) []string {
	have := make(map[string]bool, len(cursors))
	for _, c := range cursors {
		if c.Source == "projector" {
			have[c.Sub] = true
		}
	}
	var missing []string
	for _, name := range expected {
		if !have[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// parseRPCCloseTime parses getLatestLedger closeTime (Unix-seconds string). Missing or malformed fails closed, not "assume fresh".
func parseRPCCloseTime(raw string) (time.Time, error) {
	secs, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("%q: %w", raw, err)
	}
	return time.Unix(secs, 0), nil
}

// minLedgerBySource reduces cursors to the minimum LastLedger per source, restricted to [timescale.LiveCursorSources].
// One-shot job shards keep a historical last_ledger that never advances, so including them reports false LAGGING. For multi-cursor sources the slowest position wins.
func minLedgerBySource(cursors []timescale.Cursor) map[string]uint32 {
	minBySource := map[string]uint32{}
	for _, c := range cursors {
		if !timescale.IsLiveCursorSource(c.Source) {
			continue
		}
		if cur, ok := minBySource[c.Source]; !ok || c.LastLedger < cur {
			minBySource[c.Source] = c.LastLedger
		}
	}
	return minBySource
}

// writeGapReport prints the SOURCE/LAST LEDGER/TIP/LAG/STATUS table to w and returns sources lagging past threshold; sorted by source
// so output is stable for diff / grep.
func writeGapReport(w io.Writer, minBySource map[string]uint32, tipSeq, threshold uint32) []string {
	var lagging []string
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "SOURCE\tLAST LEDGER\tTIP\tLAG\tSTATUS\n")
	sources := make([]string, 0, len(minBySource))
	for s := range minBySource {
		sources = append(sources, s)
	}
	sort.Strings(sources)
	for _, source := range sources {
		last := minBySource[source]
		lag := uint32(0)
		if tipSeq > last {
			lag = tipSeq - last
		}
		status := "ok"
		if lag > threshold {
			status = "LAGGING"
			lagging = append(lagging, source)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%s\n", source, last, tipSeq, lag, status)
	}
	_ = tw.Flush()
	return lagging
}
