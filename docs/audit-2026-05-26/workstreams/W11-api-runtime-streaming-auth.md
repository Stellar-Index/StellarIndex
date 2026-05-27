# W11 — API runtime, contracts, streaming, auth

## Scope

50 handler files under `internal/api/v1/`. Every HTTP route
mounted on `cmd/ratesengine-api/main.go`. SSE / WS hubs. Auth
middleware. OpenAPI contract.

## Inputs

- `internal/api/v1/*.go` (50 files; mix of handlers, caches,
  envelope, server wiring)
- `cmd/ratesengine-api/main.go`
- `openapi/rates-engine.v1.yaml`
- `internal/api/streaming/`, `internal/api/streampublish/`
- `internal/auth/`
- `internal/ratelimit/`
- ADR-0018 (API consistency)

## Per-route loop (per-protocol §8)

For each registered route, fill:

| Check | Result | Evidence |
| --- | --- | --- |
| 1. OpenAPI presence | | |
| 2. Envelope conformance | | |
| 3. Auth gate | | |
| 4. Rate-limit identity | | |
| 5. Cache headers | | |
| 6. Pagination (ADR-0018) | | |
| 7. Empty/404 shape | | |
| 8. Latency budget | | |
| 9. Test coverage | | |
| 10. Removed-route hygiene | | |
| 11. Prewarm/handler arg drift | | |

## Route catalogue (audit pass populates)

The catalogue must enumerate every route. Initial seed from
`internal/api/v1/server.go`:

- /v1/healthz, /v1/readyz, /v1/version
- /v1/price, /v1/price/stream, /v1/price/tip, /v1/price/tip/stream
- /v1/ohlc, /v1/twap, /v1/vwap, /v1/changes, /v1/chart
- /v1/history
- /v1/markets, /v1/sources, /v1/pairs, /v1/coins (and removed
  variants — verify)
- /v1/assets/{slug} (dual-shape global vs canonical), /v1/assets/verified
- /v1/oracle, /v1/oracle/sep40, /v1/observations, /v1/observations/stream
- /v1/network/stats, /v1/ledger/tip, /v1/ledger/stream
- /v1/incidents, /v1/issuers, /v1/known_issuers, /v1/known_scams
- /v1/methodology, /v1/sac_wrappers, /v1/lending
- /v1/auth/sep10/challenge, /v1/auth/sep10/verify
- /v1/account, /v1/signup, /v1/signup_verify
- /v1/stripe-webhook (NEW)
- /v1/diagnostics/{cursors,ingestion} (admin or public?)
- /v1/status, /robots.txt, /.well-known/security.txt

## NEW since baseline checks

- /v1/stripe-webhook: W33 owns details; W11 verifies handler
  is registered + middleware chain.
- /v1/diagnostics/{cursors,ingestion}: are these admin-only?
- Multi-shape /v1/assets/{slug}: verify both shapes pass
  envelope conformance.

## Closure criteria

Every route in the catalogue has all 11 per-route checks
terminal. Findings on:
- any handler missing OpenAPI presence
- any route returning 200 on what should be 404 (or vice versa)
- any prewarm drift (memory `feedback_prewarm_handler_drift`)
- any rate-limit bypass via X-Real-IP forgery (ADR-0025)
