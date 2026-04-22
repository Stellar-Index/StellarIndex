// Binary ratesengine-ops is the admin CLI: backfill, gap-detect,
// cache-prime, docs-config, and other operational tasks that don't
// belong in the long-running binaries.
//
// Subcommands land alongside the features they support. Today only
// `docs-config` is wired; the rest land with the corresponding
// implementation PRs.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/RatesEngine/rates-engine/internal/config"
	"github.com/RatesEngine/rates-engine/internal/stellarrpc"
	"github.com/RatesEngine/rates-engine/internal/version"
)

func main() {
	args := os.Args[1:]
	if len(args) == 0 {
		printUsage()
		os.Exit(2)
	}

	switch args[0] {
	case "docs-config":
		if err := config.EmitMarkdown(os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "docs-config: %v\n", err)
			os.Exit(1)
		}
	case "rpc-probe":
		endpoint := "http://127.0.0.1:8000"
		if len(args) > 1 {
			endpoint = args[1]
		}
		if err := rpcProbe(endpoint); err != nil {
			fmt.Fprintf(os.Stderr, "rpc-probe: %v\n", err)
			os.Exit(1)
		}
	case "version", "--version", "-v":
		fmt.Println(version.String())
	case "help", "--help", "-h":
		printUsage()
	default:
		// TODO(#0): backfill, detect-gaps, cache-prime, verify-invariants
		fmt.Fprintf(os.Stderr, "ratesengine-ops: unknown subcommand %q\n", args[0])
		printUsage()
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `ratesengine-ops %s

Usage:
  ratesengine-ops <subcommand>

Subcommands:
  docs-config             Emit the generated config reference to stdout.
  rpc-probe [endpoint]    Diagnostic probe against a stellar-rpc endpoint.
                          Default: http://127.0.0.1:8000.
  version                 Print version + build date.
  help                    This help.

TODO subcommands (land with their feature PRs):
  backfill                Replay a ledger range into the trades hypertable.
  detect-gaps             Find cursor gaps in ingestion.
  cache-prime             Warm the Redis hot-path cache from Timescale.
  verify-invariants       Cross-check aggregated prices against divergence.
`, version.String())
}

// rpcProbe runs a one-shot diagnostic against a stellar-rpc endpoint:
// getHealth, getLatestLedger, getNetwork, getVersionInfo, getFeeStats.
// Prints a human-readable report to stdout + returns the first fatal
// error (e.g. endpoint unreachable). Stale-rpc is not fatal — it's
// reported in the staleness line.
func rpcProbe(endpoint string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c := stellarrpc.New(endpoint, stellarrpc.WithTimeout(5*time.Second))
	fmt.Printf("stellar-rpc probe — %s\n\n", c.Endpoint())

	// getVersionInfo first — cheapest and tells us we can reach the thing at all.
	vi, err := c.VersionInfo(ctx)
	if err != nil {
		return fmt.Errorf("getVersionInfo: %w", err)
	}
	fmt.Printf("  version:         %s\n", vi.Version)
	fmt.Printf("  commitHash:      %s\n", vi.CommitHash)
	fmt.Printf("  captiveCore:     %s\n", vi.CaptiveCoreVersion)
	fmt.Printf("  protocolVersion: %d\n\n", vi.ProtocolVersion)

	net, err := c.Network(ctx)
	if err != nil {
		return fmt.Errorf("getNetwork: %w", err)
	}
	fmt.Printf("  network:         %s (protocol %d)\n\n", net.Passphrase, net.ProtocolVersion)

	// Health returns an error envelope on stale — report, don't fail.
	if _, err := c.Health(ctx); err != nil {
		fmt.Printf("  health:          ⚠ %v\n", err)
	} else {
		fmt.Printf("  health:          ✓ healthy\n")
	}

	l, err := c.LatestLedger(ctx)
	if err != nil {
		return fmt.Errorf("getLatestLedger: %w", err)
	}
	fmt.Printf("  latestLedger:    %d (closeTime %s, id %s…)\n\n", l.Sequence, l.CloseTime, l.ID[:12])

	fs, err := c.FeeStats(ctx)
	if err != nil {
		fmt.Printf("  getFeeStats:     ⚠ %v\n", err)
	} else {
		fmt.Printf("  fees (classic):  min=%s mode=%s p99=%s (%d ledgers)\n",
			fs.InclusionFee.Min, fs.InclusionFee.Mode, fs.InclusionFee.P99, fs.InclusionFee.LedgerCount)
		fmt.Printf("  fees (soroban):  min=%s mode=%s p99=%s (%d ledgers)\n",
			fs.SorobanInclusionFee.Min, fs.SorobanInclusionFee.Mode, fs.SorobanInclusionFee.P99,
			fs.SorobanInclusionFee.LedgerCount)
	}

	// Range of events available — 1-event probe from just before tip
	// so we know the retention window.
	start := l.Sequence - 1
	er, err := c.GetEvents(ctx, start, 0, nil, &stellarrpc.Pagination{Limit: 1})
	if err != nil {
		fmt.Printf("\n  getEvents:       ⚠ %v\n", err)
	} else {
		window := er.LatestLedger - er.OldestLedger
		fmt.Printf("\n  events window:   oldest=%d  latest=%d  (~%d ledgers ≈ %.1f d at 5s cadence)\n",
			er.OldestLedger, er.LatestLedger, window, float64(window)*5/86400)
		if len(er.Events) > 0 {
			fmt.Printf("  sample event:    contract=%s… type=%s topics=%d\n",
				er.Events[0].ContractID[:12], er.Events[0].Type, len(er.Events[0].Topic))
		}
	}

	fmt.Println()
	return nil
}
