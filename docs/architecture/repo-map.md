---
title: Repo map — what lives where
last_verified: 2026-09-03
status: living doc
---

# Repo map

```
.
├── README.md                  project overview, user-facing
├── AGENTS.md                  this file — agent orientation
├── LICENSE                    Apache-2.0
├── CHANGELOG.md               Keep-a-Changelog format
├── CONTRIBUTING.md            how to contribute
├── CODE_OF_CONDUCT.md         Contributor Covenant
├── SECURITY.md                vuln-disclosure process
├── CODEOWNERS                 review routing
├── VERSIONS.md                pinned SHAs of upstream deps we audit
├── Makefile                   canonical build/test commands
├── go.mod / go.sum            single Go module for the whole repo
├── .golangci.yml              lint config
├── .github/                   workflows + issue/PR templates
│
├── cmd/                       binary entry points (six in total)
│   ├── stellarindex-indexer/              ingestion pipeline: Galexie → ClickHouse raw lake + Timescale served tier (dual-sink, ADR-0034)
│   ├── stellarindex-aggregator/           VWAP/TWAP + continuous aggregates
│   ├── stellarindex-api/                  REST + SSE API server
│   ├── stellarindex-ops/          admin CLI: backfill, detect-gaps, verify-archive, wasm-history, …
│   ├── stellarindex-migrate/      db migration runner
│   └── stellarindex-sla-probe/    SLA-evidence harness: p50/p95/p99 latency + freshness pass/fail vs the latency + freshness SLA targets
│
├── internal/                  private packages (Go-enforced, not importable externally)
│   ├── canonical/                core types: Trade, Price, Asset, Pair, Amount
│   ├── domain/                   persisted data shapes shared by storage + consumers (blend, sorobanevents, mev, accounts, divergence)
│   ├── config/                   config loading + schema
│   ├── contractid/               ADR-0035 factory-descended contract-identity registry (child gate) that gated decoders embed
│   ├── consumer/                 transport-neutral ingest contract — the load-bearing `consumer.Event` interface used across indexer/ops/dispatcher/pipeline. (The legacy `Source`/`Orchestrator` per-source-goroutine seam was deleted 2026-07; prod ingest is dispatcher-based.)
│   ├── ledgerstream/             archive/live LedgerCloseMeta streaming
│   ├── dispatcher/               production ledger walker + decoder router
│   ├── pipeline/                 shared ingest-pipeline glue used by both indexer + `stellarindex-ops backfill`
│   ├── projector/                ONLY writer for Soroban-derived events — projects per-source tables from the ClickHouse contract_events lake by default (ADR-0031/0032/0034; Postgres soroban_events is the legacy fallback source)
│   ├── completeness/             ADR-0033 coverage verification: substrate + recognition + projection reconcile → completeness_snapshots
│   ├── events/                   transport-neutral Soroban contract-event types (RPC or LCM-extracted)
│   ├── scval/                    narrow SCVal primitives wrapper around go-stellar-sdk/xdr
│   ├── xdrjson/                  XDR → JSON rendering + SAC contract-id helpers (API/explorer detail views)
│   ├── sdexclaim/                shared helpers for interpreting SDEX claim atoms (dispatcher census + clickhouse extract)
│   ├── stellarrpc/               JSON-RPC client for diagnostics + fixture capture, not prod ingest
│   ├── sources/                  one package per source (on-chain + CEX + FX)
│   ├── aggregate/                VWAP/TWAP/outlier/triangulation
│   ├── decimalsguard/            served-price decimals-assumption guard (the 100×-scaling incident class)
│   ├── pricingguard/             serving-sanity guard shared by every raw price read path
│   ├── pricelesscoverage/        priceless-popular coverage tripwire — pages when a popular asset has no served price and no recorded reason
│   ├── pricealerts/              aggregator-side evaluator for customer price alerts
│   ├── storage/                  TimescaleDB (served tier) + ClickHouse (raw lake, ADR-0034) + Redis. Subpackages only (timescale/ clickhouse/ redisclient/) — NO top-level adapter files. MinIO/lake access is via internal/ledgerstream + go-stellar-sdk datastore, not a storage adapter.
│   ├── pgarray/                  Postgres array column adapters for database/sql
│   ├── archivecompleteness/      dual-archive completeness daemon (ADR-0017)
│   ├── hashdb/                   on-disk (ledger_seq → sha256(LCM)) record for drift-detection-vs-upstream-rewrites (ADR-0016). Wired into production 2026-07-09: the indexer's live LCM read loop appends on ingest, a periodic sweep (also in the indexer) re-verifies a trailing window against the same bucket. Off by default (`[hashdb].enabled = false`, opt-in first deploy); founding case is ledger 63332650. Alert `stellarindex_hashdb_drift_detected` + runbook docs/operations/runbooks/hashdb-drift-detected.md. The ADR-0033 "feeder" role is still aspirational.
│   ├── api/                      REST/SSE handlers (v1)
│   ├── httpx/                    tiny shared HTTP response helpers for handler packages
│   ├── ratelimit/                Redis-backed fixed-window counter (INCR+EXPIRE; NOT a token bucket)
│   ├── metadata/                 SEP-1 / stellar.toml resolution
│   ├── cachekeys/                canonical Redis key builders (ADR-0007)
│   ├── version/                  build-time version info (ldflags-populated)
│   ├── obs/                      metrics, tracing, logging
│   ├── worker/                   shared primitives for detached background worker goroutines
│   ├── ops/                      stellarindex-ops subcommand implementations (archive/ chops/ diagnostics/ discovery/ ingest/ supply/)
│   ├── supply/                   circulating/total/max supply derivation
│   ├── auth/                     API-key + SEP-10 auth primitives
│   ├── nettools/                 the single canonical SSRF blocklist (used by every outbound-URL feature)
│   ├── signupreaper/             deletes orphan speculative-account rows
│   ├── logincodereaper/          bounds the login_code_lockouts table
│   ├── magiclinkreaper/          bounds the magic_link_tokens table
│   ├── currency/                 verified-currency catalogue (hand-curated seed; R-018)
│   ├── divergence/               cross-check against CoinGecko + Chainlink-HTTP (a CMC poller exists under sources/external/ but is not wired into divergence)
│   ├── customerwebhook/          drains the webhook delivery queue — HMAC-signs + POSTs pending rows, backoff/retry on failure
│   ├── incidents/                loads + parses embedded customer-facing incident post-mortems for the API + status page
│   ├── notify/                   transactional-email abstraction (Sender iface; Resend impl + Noop for dev/tests)
│   ├── obstest/                  test helpers for asserting against Prometheus metrics (HistogramVec child counts)
│   ├── platform/                 customer + staff dashboard primitives (accounts, API keys, webhooks, usage) per platform-spec.md
│   └── usage/                    per-subject daily + per-endpoint×outcome request counters (Redis) + the 5-min rollup worker into the usage_daily hypertable (feeds /v1/account/usage)
│
├── pkg/                      public surface (SemVer-stable)
│   └── client/                   Go client SDK + wire-shape types
│                                 (Envelope, Flags, AssetDetail, …)
│                                 — types live alongside the client
│                                 in pkg/client/types.go rather than
│                                 a separate pkg/types directory.
│

├── migrations/                TimescaleDB migrations (golang-migrate)
├── configs/                   example.toml + Ansible roles (configs/ansible/{roles,inventory,playbooks}/) + R1 single-host overlays
│   ├── ansible/                  multi-host roles + playbooks for R1/R2/R3
│   ├── prometheus/               R1 single-host: prometheus.r1.yml + rules.r1/ (job names rewritten for the R1 scrape config)
│   ├── alertmanager/             R1 single-host: alertmanager.r1.yml + apply.sh (severity-routing for page/ticket/informational + deadmansswitch heartbeat)
│   ├── caddy/                    R1 reverse proxy — TLS termination via Let's Encrypt
│   ├── loki/                     R1 single-host log aggregation
│   ├── audit/                    curated auditor inputs (wasm-walk contract lists) feeding `stellarindex-ops wasm-history`
│   └── healthchecks/             per-binary heartbeat + 5-min API smoke timers (Healthchecks.io)
├── openapi/                   stellar-index.v1.yaml — source of truth for API
├── examples/                  curl scripts + Postman collection (auto-gen) for the public API
├── deploy/                    docker-compose (dev), systemd (production unit files), monitoring (Prometheus rules — multi-host), clickhouse/ (tier-1 lake DDL, ADR-0034), comms/ (customer-facing incident/launch templates). The shipped status page lives in the EXPLORER at `web/explorer/src/app/status/` (served at stellarindex.io/status; postmortems at `status/incident/[slug]`, loaded at build time from `internal/incidents/data/*.md` by `web/explorer/src/lib/incidents.ts`). `web/status/` is now a redirect-only Cloudflare Pages stub that 301s status.stellarindex.io there (`web/status/public/_redirects`, web/status/README.md); earlier scaffolds were retired (F-1211 / wave 57).
├── web/explorer/              Next.js 16 static-export explorer rendered at stellarindex.io (Cloudflare Pages); see web/explorer/AGENTS.md for the frontend/design brief
├── scripts/                   dev/ops/ci helpers (incl. ci/lint-docs.sh, dev/r1-smoke.sh)
├── test/                      integration / fixtures (build tag: integration), load (k6), chaos
│
└── docs/
    ├── architecture/             narrative designs (last_verified checked in CI)
    ├── adr/                      Architecture Decision Records (immutable)
    ├── reference/                auto-generated from OpenAPI + struct tags
    ├── operations/               runbooks (every alert links one), SEV playbook, release-process
    ├── methodology/              public methodology docs (how prices are computed/aggregated)
    ├── protocols/               per-protocol verification pages (one per integrated protocol)
    ├── engineering-standards.md  the non-negotiable engineering policy layer
    └── blog/                     dated blog posts published to the explorer/site
```

---

---

Kept out of `AGENTS.md` deliberately. A directory listing is reference
material: it is long, it goes stale the moment anything moves, and an
agent that needs it can run `ls`. `AGENTS.md` is for rules that can be
followed or violated.
