// Binary ratesengine-api is the public REST + SSE API server.
//
// Phase-1 status: skeleton only. Full wiring lands in Week 7 of
// the delivery plan — handlers, middleware, rate limiting, auth,
// streaming, CDN integration.
//
// See docs/discovery/delivery-plan.md §Week 7.
package main

import (
	"fmt"
	"os"

	"github.com/RatesEngine/rates-engine/internal/version"
)

func main() {
	// TODO(#0): wire up HTTP server, middleware, handlers, and
	// observability. See openapi/ratesengine.v1.yaml for the
	// contract this server must satisfy.
	fmt.Fprintf(os.Stderr, "ratesengine-api %s — not yet implemented\n", version.String())
	os.Exit(0)
}
