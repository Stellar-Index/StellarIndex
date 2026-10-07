// Package v1 is the HTTP serving plane for the Stellar Index public API v1.
//
// openapi/stellar-index.v1.yaml is the wire contract; a handler that
// disagrees with it is a bug in one or the other, never shipped silently.
// Every 2xx JSON response is an [Envelope] whose fields are always present
// (pagination only on list endpoints); every 4xx/5xx is RFC 9457
// problem+json ([Problem]).
//
// The four current-price surfaces (/v1/price, /v1/price/tip,
// /v1/observations, asset price_usd) are pinned to different windows by
// design (ADR-0018) and only /v1/price is cross-region deterministic; see
// docs/reference/api-design.md §5.3.
//
// Handlers are thin: parse input, call a storage or cache method, wrap in
// an Envelope. Auth is the middleware's job (handlers read
// auth.SubjectFrom(ctx)); business logic lives in internal/aggregate and
// internal/storage/timescale. The middleware order is documented once, in
// package middleware, and built by [Server.middlewareStack].
package v1
