---
title: Architecture overview — the system, its flows, and what lives where
last_verified: 2026-10-05
status: living doc
---

# Architecture overview

## The system in one paragraph

Stellar Index captures every ledger of the Stellar network from a
self-hosted Galexie archive into a certified **ClickHouse raw lake**
(ADR-0034). A **dispatcher** routes each ledger through per-source
decoders into a **TimescaleDB served tier**, with the **projector** as the
single writer of projected Soroban domains (ADR-0031/0032). A
three-claim **completeness verdict** verifies the chain end to end
(ADR-0033). Trades are aggregated into **VWAP/TWAP/OHLC** with exact
rational arithmetic (ADR-0003). Everything is served through a public
**REST + SSE API** and a static **explorer** at stellarindex.io.

## The load-bearing flows

| Flow | Path | Deep doc |
|---|---|---|
| On-chain ingest | Galexie MinIO → `ledgerstream` → `dispatcher` → decoders → sink / projector → Timescale, plus the CH lake dual-sink | [ingest-pipeline.md](ingest-pipeline.md) |
| Off-chain ingest | CEX/FX connectors (`internal/sources/external`) → the same event channel | [add-cex-connector](../contributing/procedures/add-cex-connector.md) |
| Re-derive / replay | CH lake → the same decoders → served tier | [ingest-pipeline.md § The replay decision rule](ingest-pipeline.md#the-replay-decision-rule) |
| Aggregation | trades → outlier filter → class gating → VWAP → freeze/confidence → Redis + CAGGs | [aggregation-plan.md](aggregation-plan.md) |
| Verification | lake substrate + recognition + per-ledger projection reconcile → `completeness_snapshots` | ADR-0033, ADR-0041 |
| Serving | Timescale CAGGs + Redis + CH explorer reads → `internal/api/v1` → REST/SSE | ADR-0015, ADR-0018 |

## Where truth lives

- **Raw truth:** the ClickHouse lake, hash-chained to genesis and
  re-derivable from the Galexie archives (ADR-0043).
- **Served truth:** TimescaleDB, verified faithful to the lake per
  ledger by the daily verdict, within what it holds.
- **Value truth:** `verify-served-values` reconciles flagship served
  numbers against independent sources (SDF, Stellar Expert).
  `verify-usd-volume` checks the denominator under most of them. The
  standing usd-volume alerts only measure coverage (is the column
  non-NULL). This check tests the exact tiers' value: where either leg
  is USD-pegged, `usd_volume` must equal `pegged_leg / 10^decimals`
  exactly. The FX/anchor-estimated tiers are only measured until a
  production distribution exists to calibrate against.
- **Contract truth:** `openapi/stellar-index.v1.yaml`; handlers, SDK and
  explorer types are machine-reconciled against it.

## Repo map

One Go module. `internal/` is private and `pkg/` is the public SemVer
surface (ADR-0005). For anything not listed, run `ls` or read the
package's `doc.go`.

**`cmd/`** has six binaries:

| Binary | Role |
|---|---|
| `stellarindex-indexer` | ingest: Galexie → CH lake + Timescale served tier (dual-sink); hosts the projector |
| `stellarindex-aggregator` | VWAP/TWAP, continuous aggregates, price alerts |
| `stellarindex-api` | REST + SSE server |
| `stellarindex-ops` | operator CLI; the `subcommands` map in `main.go` is the dispatch table and `help.go` the usage text |
| `stellarindex-migrate` | migration runner |
| `stellarindex-sla-probe` | SLA evidence: p50/p95/p99 latency + freshness vs targets |

**`internal/`**, grouped:

| Area | Packages |
|---|---|
| Core types | `canonical` (Trade, Price, Asset, Pair, Amount), `domain` (persisted shapes shared by storage + consumers), `events` (transport-neutral Soroban event), `scval` (the only SCVal/xdr wrapper, ADR-0013), `xdrjson`, `currency` (hand-vetted catalogue) |
| Ingest | `ledgerstream`, `dispatcher`, `pipeline` (sink + wiring shared by indexer and ops), `projector`, `consumer` (the `consumer.Event` contract), `sources/<venue>` + `sources/external`, `contractid` (ADR-0035 identity gate), `entrywalk` (within-block change order), `sdexclaim`, `sourcenet` (does a source exist on this network), `wasmaudit` (per-WASM replay gate) |
| Verification | `completeness` (ADR-0033), `archivecompleteness` (ADR-0017), `hashdb` (ADR-0016 ledger-hash drift record, `[hashdb].enabled` default false; alert `stellarindex_hashdb_drift_detected`), `divergence` (CoinGecko + Chainlink cross-check) |
| Pricing | `aggregate`, `decimalsguard`, `pricingguard`, `pricelesscoverage`, `pricealerts`, `supply`, `rwa` |
| Storage | `storage/{timescale,clickhouse,redisclient}` (no top-level adapter files; MinIO is read via `ledgerstream`), `pgarray`, `cachekeys` (ADR-0007) |
| Serving | `api` (v1 handlers), `httpx`, `ratelimit` (Redis fixed-window INCR+EXPIRE, not a token bucket), `metadata` (SEP-1), `incidents` |
| Platform | `auth`, `platform`, `usage`, `customerwebhook`, `notify`, `accounterasure` (the one writer for erasure + export), reapers (`signup`, `logincode`, `magiclink`, `retention`) |
| Shared | `config`, `obs`, `obstest`, `worker`, `version`, `nettools` (the one SSRF blocklist), `pii`, `redact`, `holds`, `ops` (ops subcommand bodies), `stellarrpc` (diagnostics and fixture capture only, never ingest) |

**Elsewhere:**

- `pkg/client`: Go SDK; its wire types live in `pkg/client/types.go`.
- `migrations/`: TimescaleDB migrations (golang-migrate).
- `openapi/`: the API source of truth.
- `configs/`: `example.toml`, `ansible/{roles,inventory,playbooks}` and the r1 single-host overlays (prometheus, alertmanager, caddy, loki, healthchecks); `audit/` feeds `wasm-history`.
- `deploy/`: docker-compose (dev), systemd units, monitoring rules, `clickhouse/` lake DDL, `comms/` templates.
- `web/explorer/`: the Next.js static-export site (see `web/explorer/AGENTS.md`). The status page ships from `web/explorer/src/app/status/` (stellarindex.io/status). `web/status/` is a redirect-only stub (`web/status/public/_redirects`).
- `examples/`: curl scripts + generated Postman collection.
- `scripts/{dev,ops,ci}`; `test/`: integration (build tag `integration`), fixtures, k6 load, chaos.
- `docs/`: `adr/` (immutable), `architecture/`, `operations/` (runbooks; every alert links one), `methodology/`, `protocols/` (one page per integrated protocol), `contributing/`, `reference/` (generated), `blog/`, plus `engineering-standards.md`.

## Reading order

1. `AGENTS.md`: the rules, invariants and traps.
2. This page.
3. [ingest-pipeline.md](ingest-pipeline.md): the binding ingest rules.
4. ADR-0033 and ADR-0034: the trust story.
5. `docs/engineering-standards.md`: the policy you are bound by.
6. The `doc.go` of the package you are touching.
