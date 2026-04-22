// Binary ratesengine-migrate applies and rolls back TimescaleDB schema
// migrations under migrations/. Wraps golang-migrate with the project's
// connection-string resolution and safety rails (refuses to run
// destructive migrations without DOWN_CONFIRM=yes, etc.).
//
// Phase-1 status: skeleton only. Subcommands `up`, `down`, `status`,
// `create` land in Week 2 alongside the first migration.
package main

import (
	"fmt"
	"os"

	"github.com/RatesEngine/rates-engine/internal/version"
)

func main() {
	// TODO(#0): wire up subcommands — up, down N, status, create <name>.
	// See docs/discovery/delivery-plan.md §Week 2.
	fmt.Fprintf(os.Stderr, "ratesengine-migrate %s — not yet implemented\n", version.String())
	os.Exit(0)
}
