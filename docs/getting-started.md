---
title: Getting started with Stellar Index
last_verified: 2026-07-05
status: living doc
---

# Getting started

Stellar Index is a public, aggregated, real-time and historical
pricing API for every asset on the Stellar network — native XLM,
classic credit assets, and SEP-41 Soroban tokens. This page walks
you from zero to your first authenticated request in under five
minutes.

> **Hosted endpoint:** `https://api.stellarindex.io`
> **Interactive explorer:** [`stellarindex.io`](https://stellarindex.io) — browse coins / markets / issuers / sources / diagnostics; every panel reveals the API call that produced it
> **Reference docs:** [`docs.stellarindex.io`](https://docs.stellarindex.io)
> **Status page:** [`status.stellarindex.io`](https://status.stellarindex.io)

## Quick start

```sh
# Current XLM/USD price — no auth required for the free tier.
curl -fsSL "https://api.stellarindex.io/v1/price?asset=native&quote=fiat:USD"

# 24-hour OHLC for XLM:
curl -fsSL "https://api.stellarindex.io/v1/ohlc?base=native&quote=fiat:USD&from=$(date -u -v-24H +%Y-%m-%dT%H:%M:%SZ)"

# Last-24h trade history for an asset:
curl -fsSL "https://api.stellarindex.io/v1/history?base=native&quote=fiat:USD&from=$(date -u -v-24H +%Y-%m-%dT%H:%M:%SZ)"
```

Every JSON response carries the same envelope:

```json
{
  "data":    { "...": "..." },
  "as_of":   "2026-04-28T12:00:00Z",
  "sources": ["sdex", "soroswap", "binance"],
  "flags": {
    "stale": false,
    "reduced_redundancy": false,
    "triangulated": false,
    "divergence_warning": false
  }
}
```

The `flags` block is the operational quality signal:

| Flag | Meaning |
|---|---|
| `stale` | Response degraded below the surface's documented contract (see [ADR-0018](adr/0018-api-consistency-surfaces.md) for the per-surface baseline) |
| `reduced_redundancy` | Cross-region archive completeness is degraded ([ADR-0017](adr/0017-archive-completeness-invariants.md)) |
| `triangulated` | Reserved for a future triangulated public-serving path; the current Timescale-backed API leaves this false in normal operation |
| `divergence_warning` | Anomaly detection or cross-reference observed a meaningful divergence; treat with caution |
| `frozen` | Anomaly detection refused to publish the new bucket; this response carries the previous bucket's last-known-good value ([ADR-0019](adr/0019-anomaly-response-and-confidence-scoring.md)) |
| `single_source` | Only one source contributed; combined with `frozen` this is the manipulation signature |

## Authentication

The free tier supports anonymous requests at a low rate limit.
Authenticated tiers unlock higher limits + access to private
surfaces (`/v1/observations`, `/v1/account/*`). The key is accepted
on either the `X-API-Key` header or `Authorization: Bearer <key>`
(the Go SDK always sends `Authorization: Bearer <key>`; `X-API-Key`
is a convenience for hand-written curl calls).

### Self-service signup — the ≤1-minute path

No Stellar wallet required. `POST /v1/signup` takes an email and
returns a usable key immediately:

```sh
curl -fsSL -X POST https://api.stellarindex.io/v1/signup \
     -H "Content-Type: application/json" \
     -d '{"email":"you@example.com","label":"my first key"}'
```

```json
{
  "data": {
    "plaintext": "sip_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
    "key_id": "kid_…",
    "key_prefix": "sip_xxxxxxxx",
    "tier": "apikey",
    "rate_limit_per_min": 1000,
    "email_verification_sent": false
  },
  "as_of": "…", "flags": { "...": "..." }
}
```

Store `data.plaintext` — it is shown exactly once. Use it on any
request via `X-API-Key` (or `Authorization: Bearer <key>`):

```sh
curl -fsSL https://api.stellarindex.io/v1/account/me \
     -H "X-API-Key: sip_xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
```

> If the deployment runs with email-ownership verification enabled
> (`signup_require_email_verification`), the key is issued pending and
> must complete the link emailed at signup before it authenticates;
> the `email_verification_sent` field tells you which mode you're in.

### Account-bound keys (SEP-10)

For a key cryptographically scoped to a single Stellar account
(G-strkey), authenticate via SEP-10 Web Auth and mint through
`/v1/account/keys`. The account's signature authorises the key:

```sh
curl -fsSL -X POST https://api.stellarindex.io/v1/account/keys \
     -H "Authorization: Bearer $YOUR_SEP10_TOKEN" \
     -H "Content-Type: application/json" \
     -d '{"label":"my laptop"}'
```

Rotation is available via `POST /v1/account/keys`; revocation is not
shipped yet in this snapshot.

## Rate limits

| Tier | Anonymous | Authenticated |
|---|---|---|
| Requests / minute | 60 | 1 000 |

Rate-limit headers on every response:

```
X-RateLimit-Limit:     1000
X-RateLimit-Remaining: 987
```

Exceeded limits return `429 Too Many Requests` with `Retry-After`.
Operators on a Postgres outage may see the rate-limit middleware
fail open; the `stellarindex_ratelimit_fail_open_total` counter
ticks during such windows (operators alert on a sustained spike).

## Endpoint families

| Family | URL prefix | Surface (per [ADR-0018](adr/0018-api-consistency-surfaces.md)) |
|---|---|---|
| Asset catalogue | `/v1/assets`, `/v1/assets/{id}`, `/v1/assets/{id}/metadata` | closed-bucket |
| Current price | `/v1/price`, `/v1/price/batch` | closed-bucket |
| Tip price | `/v1/price/tip` | tip (no cross-region consistency) |
| Observations | `/v1/observations` | per-source raw |
| History | `/v1/history`, `/v1/history/since-inception` | closed-bucket |
| Aggregates | `/v1/ohlc`, `/v1/vwap`, `/v1/twap` | closed-bucket |
| Markets / pairs | `/v1/markets`, `/v1/pairs` | closed-bucket |
| Oracle (SEP-40) | `/v1/oracle/lastprice`, `/v1/oracle/x_last_price`, `/v1/oracle/prices`, `/v1/oracle/latest` | closed-bucket |
| Sources | `/v1/sources` | closed-bucket |
| Account | `/v1/account/me`, `/v1/account/usage`, `/v1/account/keys` | private |

The three consistency surfaces are not interchangeable. **Query
parameters never shift the surface** — pick the URL that matches
your needs (see [ADR-0018](adr/0018-api-consistency-surfaces.md)).

## SDKs

| Language | Package | Status |
|---|---|---|
| Go | `github.com/Stellar-Index/StellarIndex/pkg/client` | v0.x — public-launch hardening |
| TypeScript | (planned) | — |
| Python | (planned) | — |

The Go client is a thin layer over the v1 REST API:

```go
import "github.com/Stellar-Index/StellarIndex/pkg/client"

c := client.New(client.Options{
    BaseURL: "https://api.stellarindex.io",
    APIKey:  "sip_xxxxxxxx...", // optional; anonymous tier works without
})
env, err := c.Price(ctx, client.PriceQuery{
    Asset: "native",
    Quote: "fiat:USD", // optional; defaults to "fiat:USD" server-side
})
if err != nil {
    // *client.APIError carries Status, Title, Detail, RequestID
    log.Fatal(err)
}
fmt.Printf("%s = %s %s (as of %s)\n",
    env.Data.AssetID, env.Data.Price, env.Data.Quote, env.AsOf)
```

The SDK returns the full `Envelope[T]` shape so consumers can read
`env.Flags.Stale`, `env.Flags.DivergenceWarning`, etc. alongside
`env.Data`. See [`pkg/client/doc.go`](../pkg/client/doc.go) for the
full surface — ~36 typed methods covering pricing (`Price`,
`PriceAt`, `PriceTip`, `PriceBatch`, `OHLC`, `History`,
`HistorySinceInception`, `VWAP`, `TWAP`), market data (`Markets`,
`Pair`, `Pools`), the asset catalogue (`Assets`, `Asset`,
`AssetMetadata`, `Issuers`, `SACWrappers`), and account
self-service (`Me`, `Usage`, `Keys`, `CreateKey`, `RevokeKey`).

## Errors

Errors follow [RFC 9457](https://www.rfc-editor.org/rfc/rfc9457.html)
(`application/problem+json`):

```json
{
  "type":    "https://api.stellarindex.io/errors/invalid-asset-id",
  "title":   "Invalid asset identifier",
  "status":  400,
  "detail":  "asset_id must match: native | <code>-<G-issuer> | <C-contract> | fiat:<CODE>",
  "instance": "/v1/assets/banana",
  "request_id": "abc123def456"
}
```

`request_id` echoes the `X-Request-ID` response header — include it
in support requests so the on-rotation engineer can correlate
without parsing logs.

## Operational integration

For oracle / on-chain integrations:

- **SEP-40 passthrough** — `/v1/oracle/lastprice` and friends
  return the same data shape Reflector / Redstone / Band oracles
  use on-chain. Use these when you want a drop-in replacement for
  an on-chain oracle's `lastprice()` call without contract
  invocations on every read.
- **SSE streams** — `/v1/price/tip/stream`, `/v1/observations/stream`,
  `/v1/price/stream` push closed-bucket events as they ship.
  Reconnect with `Last-Event-ID` to resume; heartbeats every 15 s.
- **Bulk lookup** — `/v1/price/batch` (GET, ≤ 100 assets) or
  POST (≤ 1000) for portfolios.

## Self-hosting

Stellar Index is Apache-2.0; the full stack runs locally with one
command:

```sh
git clone git@github.com:Stellar-Index/StellarIndex.git
cd stellar-index
make dev    # docker-compose: TimescaleDB + Redis + MinIO (app binaries run on the host)
```

For a full end-to-end operator guide — architecture, hardware/disk
sizing (including a recent-window-only "light mode" that skips the
multi-TB history mirror), and step-by-step bring-up of the whole
stack — see
[`docs/operations/self-hosting.md`](operations/self-hosting.md). The
internal bring-up recipe it adapts is
[`docs/operations/archival-node-bringup.md`](operations/archival-node-bringup.md),
and the fastest path if you're comfortable with Ansible. The tier-1
deployment runs three geographically-separated archival nodes per
[ADR-0004](adr/0004-tier1-validator-aspiration.md).

## Help

- **Documentation:** [`docs/`](.) in this repo, or the rendered
  reference at [`docs.stellarindex.io`](https://docs.stellarindex.io).
- **Issues:** open one at
  [github.com/Stellar-Index/StellarIndex/issues](https://github.com/Stellar-Index/StellarIndex/issues).
- **Security:** `security@stellarindex.io` (do not open a public
  issue for security findings — see [SECURITY.md](../SECURITY.md)).

## What changed recently

See [CHANGELOG.md](../CHANGELOG.md) for the per-release detail or
[GitHub Releases](https://github.com/Stellar-Index/StellarIndex/releases)
for the operator-facing summaries.
