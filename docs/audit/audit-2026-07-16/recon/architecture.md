# Recon: architecture / topology / stores / seams (HEAD f84e2d0b)

## Topology (load-bearing for CON/REL/SEC)
- **ONE unclustered Hetzner host (R1, 136.243.90.96).** indexer+aggregator+api+Postgres/Timescale+Redis+MinIO+Galexie(+2× embedded captive-core) all colocated. `configs/ansible/inventory/r1.yml:157-160`.
- **R2/R3 NOT provisioned** — only `r2.example.yml`/`r3.example.yml` templates; `deploy.yml` region enum = `[r1]` only. HA roles (haproxy/patroni/redis-sentinel) exist under `configs/ansible/roles/` but **no playbook invokes them** (grep: zero hits) — unwired designs.
- **Redis has NO AUTH** — single-node, internal-bind-only (`tasks/16-prometheus-exporters.yml:54`). SEC: entirely network-isolation-dependent.
- Single-instance assumptions: `ingestion_cursors` one row + monotonic-advance guard (`cursors.go:105-158`); `hashdb` "not safe for concurrent use" — two handles same file, safe only single-process (`hashdb.go:93`). Rate-limit (Redis fixed-window) + SSE (Redis pub/sub) already multi-instance-safe.

## Data stores + authoritative record
- **Postgres/Timescale** (207 migration files, 108 up/down pairs): `trades` hypertable authoritative for observed trades; `prices_1m…1mo` CAGGs derive from it; `oracle_updates`; `soroban_events` (legacy landing zone, dual-written, decommission-pending #39); `completeness_snapshots` (1 row/source, ADR-0033 verdict); `accounts`/`api_keys`/`users`/`sessions`; `usage_daily`; `asset_supply_history` + per-class observer hypertables.
- **ClickHouse** (`deploy/clickhouse/tier1_schema.sql`, ReplacingMergeTree(ingested_at)): `ledgers`/`transactions`/`operations`/`operation_results` + **`ledger_entry_changes` (SOLE store anywhere)**; `contract_events` (the soroban_events replacement). CH=Tier1 lake, PG=Tier3 re-derived (ADR-0034). Full PG decommission PLANNED not executed — trades + soroban_events still dual-written today.
- **Redis**: mostly TTL cache (rebuildable) EXCEPT `rl:*` rate-limit counters + `usage:ep:*` HINCRBY counters = live authoritative (no durable equiv; usage folds to PG every 5m).
- **MinIO/S3**: galexie-archive/galexie-live + aws-public-blockchain cold fallback = ultimate raw-XDR source of truth.
- supply is **PG-sourced by deliberate choice**, NOT CH's broader `supply_flows` (watched-set-gated vs network-wide differ legitimately, migrations/0085:33-38).

## Seams (interaction-bug concentration)
- **ledgerstream→dispatcher→sink**: single-goroutine strictly-ordered walk → bounded channel (cap 256) → **8 parallel PersistEvents workers** that do NOT preserve per-source ordering (`sink.go:173-221`); mitigated only by full-tuple PK + ON CONFLICT DO NOTHING.
- **LEAD (DAT/REL)**: `sorobanevents/dispatcher_adapter.go:21-28` claims rows "never silently lost" but failed batch insert dropped with `logger.Warn`, NO retry/dead-letter (`:254-270`). Doc contradicts code.
- **LEAD (CON)**: 3 `SinkMode`s govern a live dual-writer race (dispatcher vs projector) during CH transition, resolved by ON CONFLICT DO NOTHING; a type misclassified in mode-selection switch → silent double-write or never-write, no compile-time check (`sink.go:53-112`).
- **projector**: one goroutine/source, own cursor, panic-isolated per row; reads CH `contract_events` by default (`ClickHouseProjectorSource:true`).
- **openapi→pkg/client**: HAND-WRITTEN (no codegen markers); drift caught by reflection test `pkg/client/spec_contract_test.go` in `go test`, not a CI codegen-diff. (web/explorer types.ts IS codegen'd with a CI gate.)
- **auth/ratelimit chain** (`server.go:1087-1206`): Logger→Recoverer→CORS→Auth→KeyPolicy→RequireEmailVerified→MonthlyQuota→UsageTracker→RateLimit (Auth before RateLimit so limits key off subject). Under `auth_backend=postgres`, Redis-backed self-service key-write path deliberately 503'd (split-brain guard, main.go:361-372).

## Integrations + trust
- CEX streamers (Binance/Bitstamp/Coinbase/Kraken, public, no key) feed VWAP. FX/aggregator pollers (CoinGecko/CMC/CryptoCompare/ECB/exchangeratesapi/PolygonForex/Frankfurter/Massive) excluded from VWAP by design. NO circuit breaker (bounded retry+fail-open).
- Divergence: CoinGecko + Chainlink (public eth_call) cross-check OUR VWAP, never inputs; staleness gate 3h crypto/76h FX; needs ≥2 refs to fire.
- Resend (email, 2 callers: magic-link + signup verify; Noop fallback). Healthchecks.io (3 layers). Discord alert fanout. Cloudflare Pages + Caddy trusts CF edge IPs for CF-Connecting-IP.
- Galexie/core = trusted first-party, version+SHA pinned. aws-public-blockchain + SDF/publicnode/lobstr history archives = adversarial-data, gzip-bomb-guarded + chain-link/checkpoint verified. Customer webhook targets = most adversarial outbound, SSRF-guarded (dial-time re-resolve, no redirect follow).

## Import layering (enforced)
`scripts/ci/lint-imports.sh` (458 lines, regex): banned-package (no stellarrpc in ingest, xdr→scval allowlist, no Horizon), foundation-purity (canonical/nettools/scale/version import zero internal), layering (pkg!→internal, sources!→app, storage!→compute [6 grandfathered], api import-restricted). Baseline shrank 15→6 on 2026-07-10 (internal/domain extraction). No cycle-detection, no "nothing imports cmd/**" rule (gaps).

## DOC-DRIFT / FALSE-CLAIMS (audit DOC dimension leads)
1. `docs/architecture/infrastructure/multi-region-topology.md` status:ratified reads as live 3-region; only R1 deployed.
2. `docs/architecture/ha-plan.md` status:ratified (ADR-0008) describes HAProxy/Patroni/Sentinel infra; NO playbook invokes those roles. Prior audit caught same for backup runbook.
3. CLAUDE.md ~216: projector writes "from soroban_events landing zone" — actually reads ClickHouse contract_events by default now.
4. CLAUDE.md:103 calls ratelimit "token bucket" — it's FixedWindowCounter (INCR+EXPIRE Lua), different algorithm.
5. `docs/architecture/storage-layering-spec.md` status:current says team decided NOT to create internal/domain — but internal/domain/* was created 2026-07-10 doing exactly that.
6. `docs/maintainability-audit-2026-07-01/D8-dependency-direction.md` claims lint-imports enforces "essentially none" — no longer true (4 rule families live).
7. DEAD SCHEMA: `api_usage_events` (migrations/0027:227) promises async Redis-stream→worker path; no consumer implemented anywhere (`grep AppendEvent` → zero).

## Dead binaries/code
`cmd/tmpxdrdump/` empty untracked dir. (See entrypoints.md for ops cache-prime/verify-invariants aspirational + supply seed-sac-balances --help drift.)
