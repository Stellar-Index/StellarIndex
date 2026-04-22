// Binary ratesengine-ops is the admin CLI: backfill, gap-detect,
// cache-prime, docs-config-dump, and other operational tasks that
// don't belong in the long-running binaries.
//
// Phase-1 status: skeleton only. Subcommands land alongside the
// features they support.
package main

import (
	"fmt"
	"os"

	"github.com/RatesEngine/rates-engine/internal/version"
)

func main() {
	// TODO(#0): wire up Cobra subcommands: backfill, detect-gaps,
	// cache-prime, docs-config, verify-invariants.
	fmt.Fprintf(os.Stderr, "ratesengine-ops %s — not yet implemented\n", version.String())
	os.Exit(0)
}
