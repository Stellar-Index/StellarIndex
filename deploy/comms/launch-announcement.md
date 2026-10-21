<!--
Public launch announcement. Send T-0 (immediately after the
cut completes per v1-launch-plan.md §1 step 4).

Channels:
  - Email to the launch contacts (Stellar + Freighter)
  - Discord: #stellar-index-public
  - Project handle (Twitter / Mastodon / wherever applicable)

Fill before sending: {{utc_time}}, {{anon_rpm}} and {{key_rpm}}
(the `anon_rate_limit_per_min` / `key_rate_limit_per_min` the cutover
set on the public tier), {{your_name}}. Do not name price vendors
here until their serving rights are settled (INV-2767); /v1/sources
lists them.

Tone: factual, short, surfaces-first. The reader should be
able to make their first request in under 60 seconds.

Subject line: "Stellar Index — public launch (api.stellarindex.io live)"
-->

# Stellar Index — public launch

Stellar Index is live at **https://api.stellarindex.io** as of
{{utc_time}}, with the explorer at **https://stellarindex.io**.

## What this is

A public REST + SSE API and explorer for the Stellar network:
ledgers, transactions, operations, accounts and contracts; the asset
catalogue with supply and complete history; per-protocol data for the
Soroban protocols we index (`/v1/protocols`); and aggregated
VWAP / TWAP / OHLC prices built from on-chain DEX trades, oracles and
off-chain market feeds (`/v1/sources`). Every number is derived from
a verified raw ledger lake, and `/v1/coverage` states what has been
captured. Source code is public at
<https://github.com/Stellar-Index/StellarIndex>.

## First request

```sh
curl 'https://api.stellarindex.io/v1/price?asset=native&quote=fiat:USD' | jq .
```

API reference: <https://docs.stellarindex.io>.
Getting started: <https://github.com/Stellar-Index/StellarIndex/blob/main/docs/getting-started.md>.
Status: <https://stellarindex.io/status>.

## What's covered today

- Prices: `/v1/price` (closed-bucket VWAP), `/v1/price/tip`
  (rolling), `/v1/observations` (per-source raw), `/v1/ohlc`,
  `/v1/history/since-inception`.
- Streaming companions: `/v1/price/stream`, `/v1/price/tip/stream`,
  `/v1/observations/stream`.
- Asset metadata, supply, market cap, FDV and 24h volume on
  `/v1/assets/{id}`.
- SLA targets: p95 ≤ 200 ms, p99 ≤ 500 ms, ≥ 99.9 % availability,
  ≤ 30 s freshness. How each is measured:
  <https://github.com/Stellar-Index/StellarIndex/blob/main/docs/architecture/coverage-matrix.md>.
- One region at launch; more regions follow after v1.

## How to get a key

One call, no email or payment:

```sh
curl -X POST https://api.stellarindex.io/v1/register -H 'Content-Type: application/json'
```

The response carries your API key once; send it as `X-API-Key` or
`Authorization: Bearer <key>`. Anonymous requests are limited to
{{anon_rpm}} per minute per IP; a key raises that to {{key_rpm}}.

## Reaching us

- Bug reports / feature requests: <https://github.com/Stellar-Index/StellarIndex/issues>
- Security: `security@stellarindex.io` (see SECURITY.md; never a public issue)
- Operational status: <https://stellarindex.io/status>

— {{your_name}}
