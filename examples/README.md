# Rates Engine API examples

Hands-on examples for hitting the public API at
`https://api.ratesengine.net`. The OpenAPI spec at
[`openapi/rates-engine.v1.yaml`](../openapi/rates-engine.v1.yaml)
is the source of truth — these examples show common usage patterns
in three formats so you can pick what fits your workflow.

## What's here

| Directory | Best for |
|-----------|----------|
| [`curl/`](curl/) | quick checks, scripting, CI smoke tests |
| [`postman/`](postman/) | Postman / Insomnia / Bruno users |
| [`go/`](../pkg/client/) | Go clients — see the SDK in `pkg/client` |

## Authentication

The free tier (`Tier=anonymous`, 60 req/min) needs no header.
Paid tiers need `Authorization: Bearer <plaintext-key>` — get a
key by POSTing to `/v1/signup` (see
[`curl/02-signup.sh`](curl/02-signup.sh)).

## Conventions

- All responses are JSON envelopes: `{ data: ..., as_of: "...", flags: {...} }`.
- Streaming endpoints use Server-Sent Events (`text/event-stream`).
- Errors follow [RFC 9457 problem+json](https://datatracker.ietf.org/doc/html/rfc9457).
- Money values are JSON strings to avoid IEEE 754 precision loss
  on i128/u128 amounts (see CLAUDE.md invariant #1).
