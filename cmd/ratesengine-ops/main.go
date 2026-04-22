// Binary ratesengine-ops is the admin CLI: backfill, gap-detect,
// cache-prime, docs-config, and other operational tasks that don't
// belong in the long-running binaries.
//
// Subcommands land alongside the features they support. Today only
// `docs-config` is wired; the rest land with the corresponding
// implementation PRs.
package main

import (
	"fmt"
	"os"

	"github.com/RatesEngine/rates-engine/internal/config"
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
  docs-config        Emit the generated config reference to stdout.
  version            Print version + build date.
  help               This help.

TODO subcommands (land with their feature PRs):
  backfill           Replay a ledger range into the trades hypertable.
  detect-gaps        Find cursor gaps in ingestion.
  cache-prime        Warm the Redis hot-path cache from Timescale.
  verify-invariants  Cross-check aggregated prices against divergence sources.
`, version.String())
}
