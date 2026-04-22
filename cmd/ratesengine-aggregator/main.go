// Binary ratesengine-aggregator computes VWAP, TWAP, triangulated
// prices, and OHLC continuous aggregates over the ingested
// canonical trade stream.
//
// Phase-1 status: skeleton only. Full wiring lands in Week 4-6 of
// the delivery plan.
//
// See docs/discovery/delivery-plan.md §Week 4-6.
package main

import (
	"fmt"
	"os"

	"github.com/RatesEngine/rates-engine/internal/version"
)

func main() {
	// TODO(#0): wire up the aggregator — rolling-window VWAP/TWAP,
	// outlier filtering, triangulation, continuous-aggregate
	// refresh scheduling.
	fmt.Fprintf(os.Stderr, "ratesengine-aggregator %s — not yet implemented\n", version.String())
	os.Exit(0)
}
