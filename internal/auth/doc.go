// Package auth holds the authentication primitives the v1 API
// middleware uses to identify callers + enforce per-tier rate limits.
//
// Three tiers, in increasing trust:
//
//   - "anonymous" — no credential. Default; rate-limited at the
//     lowest tier (60 req/min today; stellarindex_api §S9.3).
//   - "apikey"    — caller presents `Authorization: Bearer <key>`
//     or `X-API-Key: <key>`. Lookup yields a subject + tier.
//   - "sep10"     — caller presents a SEP-10 JWT in
//     `Authorization: Bearer <jwt>`. The JWT is what we issue from
//     the SEP-10 challenge/verify exchange.
//
// Operator config picks the active mode via [config.APIConfig].AuthMode:
// "none" (anonymous-only, no validators wired), "apikey", or "sep10".
//
// Implementation status:
//
//   - APIKey.Lookup — implemented by [RedisAPIKeyValidator]. Records
//     are stored under `apikey:<sha256-hex>` (see [APIKeyRecord]
//     for the JSON shape) with no TTL; expiry/revocation are encoded
//     in the record. The Noop stub remains as the failure-mode the
//     middleware lands on when auth_mode=apikey is configured but
//     no validator is wired (e.g. Redis unavailable at startup).
//   - SEP10.{Challenge,Verify,VerifyJWT} — implemented by the live
//     SEP-10 validator package when the API binary is configured
//     with a signing seed + JWT secret. [NoopSEP10Validator]
//     remains as the explicit disabled-state fallback.
//
// The package still keeps noop validators around as the explicit
// disabled-state fallback, but the runtime auth path is no longer
// speculative: the API binary can serve API-key and SEP-10-backed
// authenticated surfaces in this snapshot.
//
// References:
//
//   - Stellar SEP-10 (Web Auth):
//     https://github.com/stellar/stellar-protocol/blob/master/ecosystem/sep-0010.md
//   - ADR-0009 (latency budget) — auth middleware budget is 10 ms
//     on the steady-state hot path; SEP-10 verify must fit there.
//   - docs/architecture/coverage-matrix.md S9.3 — per-tier rate
//     limits this package's tier identifier feeds.
package auth
