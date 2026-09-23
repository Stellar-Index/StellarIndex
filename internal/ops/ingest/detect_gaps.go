package ingest

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"
	"time"

	"github.com/Stellar-Index/StellarIndex/internal/config"
	"github.com/Stellar-Index/StellarIndex/internal/stellarrpc"
	"github.com/Stellar-Index/StellarIndex/internal/storage/timescale"
)

// detectGaps compares every per-source cursor against the
// stellar-rpc network tip and reports any source lagging by more
// than `threshold` ledgers. Exits non-zero when at least one source
// is lagging so the command works as a prometheus-style health
// probe from a cron / k8s Job.
//
// For sources that track multiple sub-cursors (Soroswap per-pair
// cursors), the MINIMUM last-ledger across the source's rows is
// used — we care about the slowest position, not the fastest.
func detectGaps(args []string) error {
	fs := flag.NewFlagSet("detect-gaps", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "Path to TOML config file (required)")
	threshold := fs.Uint("threshold", 100, "Ledgers of lag that count as a gap")
	rpcOverride := fs.String("rpc", "", "stellar-rpc endpoint URL for the network tip (overrides stellar.rpc_endpoints[0])")
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

	store, err := timescale.Open(ctx, cfg.Storage.PostgresDSN)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	defer func() { _ = store.Close() }()

	cursors, err := store.ListCursors(ctx)
	if err != nil {
		return err
	}

	// Per-source min across sub_source rows.
	minBySource := map[string]uint32{}
	for _, c := range cursors {
		if cur, ok := minBySource[c.Source]; !ok || c.LastLedger < cur {
			minBySource[c.Source] = c.LastLedger
		}
	}

	if len(minBySource) == 0 {
		fmt.Printf("(no cursors stored — nothing to check against tip %d)\n", tip.Sequence)
		return nil
	}

	var lagging []string
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintf(w, "SOURCE\tLAST LEDGER\tTIP\tLAG\tSTATUS\n")
	// Sorted iteration so output is reproducible across invocations
	// — operators pipe into diff / grep and expect stable ordering.
	sources := make([]string, 0, len(minBySource))
	for s := range minBySource {
		sources = append(sources, s)
	}
	sort.Strings(sources)
	for _, source := range sources {
		last := minBySource[source]
		lag := uint32(0)
		if tip.Sequence > last {
			lag = tip.Sequence - last
		}
		status := "ok"
		if lag > uint32(*threshold) {
			status = "LAGGING"
			lagging = append(lagging, source)
		}
		_, _ = fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%s\n", source, last, tip.Sequence, lag, status)
	}
	_ = w.Flush()

	if len(lagging) > 0 {
		return fmt.Errorf("%d source(s) lagging past threshold %d: %v",
			len(lagging), *threshold, lagging)
	}
	return nil
}
