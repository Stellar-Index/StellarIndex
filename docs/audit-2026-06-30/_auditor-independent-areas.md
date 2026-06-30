---
title: Auditor's independent area decomposition (to reconcile with mapper)
status: working note — pass 1.5
---

# Independent area decomposition — Audit 1

A second, independent decomposition drafted from the auditor's own model of the
system (CLAUDE.md repo map + session context), deliberately produced BEFORE
folding in the surface-inventory mapper, so the union catches blind spots in
either list. Each area carries an **attack list** — the concrete things an
adversary/skeptic probes. Reconcile + renumber into PLAN-1 once the mapper lands.

## Ingest & decode

- **A1 — Galexie→dispatcher→decoder ingest path** (`internal/ledgerstream`,
  `internal/dispatcher`, `internal/pipeline`). Attack: cursor reset/regression
  on restart (happened before — 2026-06-01); gap between live tip and backfill;
  off-by-one in ledger walking; partial-window silent loss; SDK
  `ApplyLedgerMetadata` error swallowing; back-pressure when sink is slow.
- **A2 — Soroban decoders** (`internal/sources/<venue>`, `internal/scval`,
  `internal/events`). Attack: i128→int64 truncation (ADR-0003) in EACH decoder;
  topic-shape assumptions (Phoenix 8-event, Soroswap Swap-then-Sync, Comet shared
  topic, Band zero-events, Redstone feed_id mismatch); Map-field-by-position vs
  by-name; SEP-41 transfer i128-or-map type test; CAP-67 4-topic vs 3-topic;
  contract-upgrade schema drift on backfill.
- **A3 — Projector / one-writer-per-domain** (`internal/projector`,
  `pipeline/sink.go::IsProjectedEvent`). Attack: a projected event type with no
  `IsProjectedEvent` arm (dual-write or silent drop); event_index=0 collision
  (fixed before — verify); projector cursor init near genesis causing empty
  crawl; replay idempotency / dedup.
- **A4 — External CEX/FX connectors** (`internal/sources/external/*`). Attack:
  per-source `Decimals` scaling (10^8 vs 10^6 FX vs on-chain per-asset) —
  mis-scale → wrong price; `IncludeInVWAP`/class gating leaking aggregators/
  oracles into VWAP; WS reconnect/backfill gaps; `BackfillSafe` flipped without
  audit.

## Aggregate & serve

- **A5 — Aggregation (VWAP/TWAP/OHLC/triangulation)** (`internal/aggregate`).
  Attack: `min_usd_volume=0` dust manipulation; stablecoin fiat-proxy late-bind
  correctness (depeg hiding); triangulation path selection; outlier filter
  bypass; closed-bucket contract (ADR-0015) violations; cross-region rate equality.
- **A6 — Pricing API read paths** (`internal/api/v1/price*.go`, vwap, twap,
  oracle, observations, chart, ohlc). Attack: non-sargable queries (the 50→400ms
  incident — re-scan for `func(col)` in WHERE); fallback chain serving stale as
  fresh (`flags.stale` correctness); prewarm cache-key drift; alias resolution
  (native vs crypto:XLM); the new FiatProxy self-peg arm (hide a real depeg?).
- **A7 — Supply derivation** (`internal/supply`, SEP-41 lake-flows path). Attack:
  Σmint−Σburn−Σclawback correctness; counterparty topic-index (the CAP-67 loss);
  NUMERIC arithmetic; circulating vs total; the on-demand lake sum perf/cost.
- **A8 — Assets/catalogue surface** (`internal/api/v1/assets*.go`,
  `internal/currency`). Attack: dual-shape `/v1/assets/{slug}` dispatch
  ambiguity; verified-currency trust surface (seed.yaml) injection; class filter
  correctness; unverified-collision warning bypass.

## Storage & data integrity

- **A9 — TimescaleDB served tier** (`internal/storage/timescale`, `migrations/`).
  Attack: rogue retention policy on `trades` (drift — ADR-0034 says forever);
  chunk-count perf cliff (6 inserts/s incident); ON CONFLICT PK coarseness;
  hypertable lock-table sizing; cagg refresh gaps.
- **A10 — ClickHouse raw lake** (`internal/storage/clickhouse`,
  `deploy/clickhouse/`). Attack: dedup correctness (FINAL); supply_flows
  completeness vs lake; ledger contiguity + hash-chain claim; the topic_0_sym=''
  undercount trap; query load wedging CH log channel (root-fill incident).
- **A11 — Redis cache + keys** (`internal/cachekeys`, `internal/ratelimit`).
  Attack: key builder drift handler-vs-prewarm; token-bucket correctness under
  concurrency (race / over-admit); BGSAVE-block fallback; Sentinel failover
  assumptions; cache poisoning.
- **A12 — Completeness/coverage** (`internal/completeness`,
  `internal/archivecompleteness`, `hashdb`). Attack: verdict reconcile soundness;
  watermark OVERWRITE-not-max regression; childgate staleness (the blend
  false-neg); the dead `hashdb` (zero callers — claimed feeder).

## Platform, auth, customer

- **A13 — Auth (API-key + SEP-10)** (`internal/auth`, `internal/api/v1` middleware).
  Attack: key-prefix split-brain (rek_/sip_ rebrand leftover); timing-safe
  compare; `EmailVerifiedAt` zero-time guards; SEP-10 challenge replay/expiry;
  RequireEmailVerified bypass.
- **A14 — Rate limiting** (`internal/ratelimit`, `internal/usage`). Attack:
  bucket refill math; per-tier limits; bypass via header spoof; trusted-proxy
  CIDR + X-Forwarded-For trust; usage counter accuracy.
- **A15 — Platform/dashboard** (`internal/platform`, `dashboardwebhooks`,
  `customerwebhook`, `notify`). Attack: API-key issuance bugs (rek_ prefix,
  instant-revoked, 2055 last-used — zero-time); webhook HMAC signing + retry
  storms; magic-link nil-Now panic class; email-verify flow dead-ends.

## Infra, CI/CD, ops

- **A16 — Ansible roles** (`configs/ansible/roles/*`). Attack: Jinja
  string-"false"-truthy bugs (the migrations-skip bug); idempotency; secret
  handling in vault; the Discord migration render correctness; preflight asserts.
- **A17 — systemd timers/services + drivers** (`.service/.timer.j2`, `files/*.sh`).
  Attack: textfile perms (0644 node_exporter); peer-auth-under-systemd DSN;
  self-chunk memory guards; OnCalendar collisions; Persistent= backlog storms.
- **A18 — CI/CD + supply chain** (`.github/workflows/*`, `scripts/ci/*`). Attack:
  SHA-pinning gaps; gitleaks allowlist over-broad (just touched); release.yml
  SHA256SUMS integrity; deploy.yml rollback path; billing-limit failure mode;
  docs-drift guards that silently drifted (postman, web types).
- **A19 — Secrets & dependency posture** (`go.mod`, `VERSIONS.md`,
  `/etc/default/*` referenced). Attack: leaked creds in history/output (the
  postgres_exporter DSN); govulncheck currency; pinned-dep audit; the
  archived-stellar/go → go-stellar-sdk migration completeness.
- **A20 — Monitoring/alerting** (`deploy/monitoring`, `configs/prometheus`,
  `configs/alertmanager`). Attack: rule pairing (multi-host↔R1); runbook
  orphans; dead alerts (no evaluator); the Discord receivers degrade-silently
  correctness; deadmansswitch.

## Frontend & docs

- **A21 — Explorer frontend security** (`web/explorer`). Attack: JSON-LD
  injection via attacker-controlled SEP-1 fields (the stored-XSS class —
  serializeJsonLd); CF Pages Functions (OG/price) SSRF/secret exposure;
  client-side API-key handling; dangerouslySetInnerHTML; open-redirect in
  /auth/callback; prerender data-trust.
- **A22 — Explorer correctness/build** (`web/explorer`, `web/status`). Attack:
  API-type drift (web-generate-api not drift-guarded); next/font build-time
  fetch; SSG fallback routes; broken links; embed/widget XSS.
- **A23 — Documentation integrity** (`docs/`, `CLAUDE.md`, ADRs, runbooks).
  Attack: docs that lie about current behavior (the doc-hygiene class — just
  found 6); last_verified staleness; ADR invariants claimed-but-unenforced
  (i128 analyzer doesn't exist); reference/ generated-doc drift.
- **A24 — Public SDK** (`pkg/client`). Attack: wire-shape compatibility promise;
  error taxonomy; i128 string handling in the SDK; example correctness.

## Cross-cutting

- **A25 — i128 invariant enforcement (ADR-0003)** end-to-end. Attack: every
  parse site of `Int128Parts`; NUMERIC columns vs BIGINT; JSON-as-string; the
  claimed-but-absent golangci analyzer + migration lint.
- **A26 — Obs/metrics correctness** (`internal/obs`, `obstest`). Attack: metric
  cardinality blowups (per-asset series); label drift; the paired
  Total/DurationSeconds pattern gaps.
- **A27 — Config schema** (`internal/config`). Attack: tag drift (config-tag
  drift root class); validate()-on-copy panics (the magic-link bug); default
  foot-guns (sep41 projector default; enabled_source KnownSources).
- **A28 — Cross-package data-flow integrity.** Attack: a `consumer.Event` type
  with no sink arm; canonical type round-trips (Asset/Amount/Pair) through
  storage→API→SDK; precision loss at any boundary.

## Coverage assertion (to verify in Pass 2)

Every top-level dir (cmd, internal, pkg, migrations, configs, deploy, web,
scripts, test, docs, openapi, examples, .github) must map to ≥1 area above.
Gaps to check when reconciling: `examples/`, `internal/incidents`,
`internal/divergence`, `internal/supply` sub-observers, `internal/metadata`
(SEP-1), `internal/version`.
