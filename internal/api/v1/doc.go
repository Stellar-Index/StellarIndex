// Package v1 is the HTTP serving plane for the Stellar Index public API v1.
//
// openapi/stellar-index.v1.yaml is the wire contract; a handler that disagrees with it is a bug in
// one or the other. Every 2xx JSON response is an [Envelope] (fields always present); every 4xx/5xx
// is RFC 9457 problem+json ([Problem]).
//
// The four current-price surfaces (/v1/price, /v1/price/tip, /v1/observations, asset price_usd) use
// different windows by design (ADR-0018); only /v1/price is cross-region deterministic. See
// docs/reference/api-design.md §5.3.
//
// Handlers are thin: parse, call storage or cache, wrap in an Envelope. Auth is middleware's job
// (auth.SubjectFrom(ctx)); middleware order is built by [Server.middlewareStack].
package v1
