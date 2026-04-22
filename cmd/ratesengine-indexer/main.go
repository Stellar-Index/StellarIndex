// Binary ratesengine-indexer orchestrates every configured Source,
// driving BackfillRange and StreamLive against the canonical
// ingestion layer. Writes to TimescaleDB.
//
// Phase-1 status: skeleton only. Full wiring lands in Week 2-3 of
// the delivery plan.
//
// See docs/discovery/delivery-plan.md §Week 2-3.
package main

import (
	"fmt"
	"os"

	"github.com/RatesEngine/rates-engine/internal/version"
)

func main() {
	// TODO(#0): wire up the Source registry, ingestion loop,
	// Timescale writer, and cursor persistence.
	fmt.Fprintf(os.Stderr, "ratesengine-indexer %s — not yet implemented\n", version.String())
	os.Exit(0)
}
