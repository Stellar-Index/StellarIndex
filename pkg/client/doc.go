// Package client is the official Go SDK for the Rates Engine
// public API.
//
// # Stability
//
// Public surface under SemVer per [ADR-0005]. Within the v0.x
// pre-release line breaking changes may occur but are documented in
// CHANGELOG.md. Once we tag v1.0.0 the contract is binding:
// removing or repurposing any exported identifier requires a major
// version bump.
//
// # Quick start
//
//	package main
//
//	import (
//	    "context"
//	    "fmt"
//	    "github.com/RatesEngine/rates-engine/pkg/client"
//	)
//
//	func main() {
//	    c := client.New(client.Options{
//	        BaseURL: "https://api.ratesengine.net",
//	        APIKey:  "rek_…",  // optional; anonymous works at low rate-limit
//	    })
//	    p, err := c.Price(context.Background(), client.PriceQuery{
//	        Asset: "native",
//	        Quote: "fiat:USD",
//	    })
//	    if err != nil {
//	        // err is a *APIError when the server returned a problem+json;
//	        // unwrap with errors.As to inspect HTTP status / type / detail.
//	        panic(err)
//	    }
//	    fmt.Printf("XLM/USD = %s (%s, observed %s)\n",
//	        p.Data.Price, p.Data.PriceType, p.Data.ObservedAt)
//	}
//
// # Authentication
//
// Three modes mirror the server's [config.APIConfig].AuthMode:
//
//   - **Anonymous** — no APIKey on the client; rate-limited per IP.
//   - **API key** — set Options.APIKey; sent as
//     `Authorization: Bearer <key>` on every request.
//   - **SEP-10** — pending; will be added when the server's SEP-10
//     verifier ships (Phase 5).
//
// # Error handling
//
// Methods return either the success envelope OR an error. Network
// / parse errors come through wrapped in fmt.Errorf; HTTP errors
// from the server come through as *[APIError]. Detect via
// [errors.As]:
//
//	var apiErr *client.APIError
//	if errors.As(err, &apiErr) && apiErr.Status == 404 {
//	    // pair not found — fall back to a different quote
//	}
//
// # Concurrency
//
// [Client] is safe for concurrent use after construction. The
// underlying *http.Client is shared across calls; each request
// opens a fresh connection from the transport pool.
//
// # Roadmap
//
// PR A (this PR) ships the skeleton: Client, options, errors,
// shared types, and a minimal endpoint set (Price, History,
// Assets, Account). Subsequent PRs add /v1/price/tip, /v1/observations,
// /v1/oracle/*, /v1/markets, and the SSE streaming counterparts —
// see docs/architecture/launch-readiness-backlog.md L3.10.
//
// [ADR-0005]: https://github.com/RatesEngine/rates-engine/blob/main/docs/adr/0005-monorepo.md
package client
