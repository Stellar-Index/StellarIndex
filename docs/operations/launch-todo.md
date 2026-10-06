---
title: Launch to-do — consolidated master list
last_verified: 2026-06-30
status: superseded — see v1-launch-plan.md
---

# Launch to-do — consolidated master list

> **SUPERSEDED by [`v1-launch-plan.md`](v1-launch-plan.md).** Open items
> (CoinGecko key, external review, tx_hash_index backfill, R2/R3, P4 tail) are
> carried there; many rows here are done-but-unmarked (Healthchecks wiring,
> min_usd_volume, ADR-0035 gating, cold-tier trim). Do not execute from here.
> Operator actions live in `audit-remediation-operator-actions.md`.

Compiled 2026-06-30. Operator decisions then: push to launch; multi-region is
committed (ADR-0008 / ADR-0016, 99.99% uptime claim, coverage-matrix S9.1;
active/active is still v2, R2/R3 serve + delegate-trust); CoinGecko goes to a
paid plan. Rationale: [`docs/adr/`](../adr/). `[OPS]` = operator-scale (heavy / touches
prod data); `[code]` = ordinary change.

## CURRENT STATUS (2026-06-30; the task sections below remain the P3/P4 scope)

Two numbering schemes: **launch phases P0-P5** (P0 ops fixes, P1 backfills,
P2 launch-blocking, P3 multi-region, P4 ADR features, P5 post-launch) and
**resync phases A-E** (A substrate, B trustworthy verdict, C re-derives, D
hygiene, E verdict green + steady-state; run inside P0/P1).

Done: P0 complete (P0-3 pending purchase); P1-1/3/5/6/7 done; resync A-E done
(verdict 15/15 green after the rc.149 preseed fix; SEP-41 supply from the lake;
`supply_flows` deduped; "never behind" watchdog live); P2 code-side complete;
P3-7 done. Alerting is Discord (`discord_configs`, two webhooks #pages /
#alerts), not Slack.

**Pending operator actions:**
1. Buy CoinGecko Pro, set `COINGECKO_API_KEY` on r1, restart indexer (P0-3).
2. Create the Healthchecks.io account + Discord webhooks; paste 4x
   `HEALTHCHECKS_URL_*`, deadmansswitch and `DISCORD_WEBHOOK_URL_PAGES`/`_ALERTS`
   on r1, rerun `pre-launch-check.sh` (P2-1).
3. Provision R2 (AWS) + R3 (Vultr); then `redis-sentinel` + region bringup
   roles can run (P3-1..P3-6).
4. Sequence the heavy backfills (P1-2, P1-4) when wanted; none blocking.
5. Optional: Cloudflare orange-cloud in front of `api.` (P2-1 4); book the
   external security review (P2-3); cutover + announcement (P2-5/P2-6).

---

## Phase 0 — Operational fixes (COMPLETE 2026-06-30)

| # | Item | Status |
|---|------|--------|
| P0-1 | `sep1-refresh` systemd timer (issuer `org_name`/`org_verified` re-froze without it) | DONE: Ansible templates, daily 05:12 UTC, installed on r1 |
| P0-2 | `compute-completeness` systemd timer (ADR-0033 verdict was frozen 17-21 days) | DONE: daily 05:30 UTC + `run-compute-completeness.sh`, self-chunking per-source driver in 25k windows so the SDEX reconcile never hits ClickHouse's 12 GiB limit; never regresses ahead sources |
| P0-3 | **CoinGecko paid plan**: oracle feed dead 11 days (10k free-tier limit, 429 loop) | CODE DONE: poller auto-switches to `pro-api.coingecko.com` when a Pro key is set (Pro keys 404 on the public host). OPERATOR: buy plan, set `COINGECKO_API_KEY` in `/etc/default/stellarindex`, restart indexer |
| P0-4 | ~~Massive FX poller stalled~~ | FALSE ALARM: the worker runs in the API binary (`fx_quotes persisted rows:797` hourly); audit misread `observed_at` |
| P0-5 | **Prometheus off the 49G root** (13G, root chronic >90%) | DONE: `data/prometheus` ZFS dataset (zstd 11.87x, 13G -> 1.31G), root 94% -> 60%; codified in the ZFS role defaults |
| P0-6 | `nft-drop` syslog spam (~10k lines / 200k) | DONE: rsyslog routes `nft-drop` to logrotated `/var/log/nft-drop.log` and stops |
| P0-7 | `massive` missing from `/v1/sources` | DONE: bridged into `external.Registry` as external FX (`IsOnChain=false`). `coinmarketcap`/`cryptocompare`/`exchangeratesapi` are intentionally-present disabled paid connectors; the duplicate FX connector was removed (#466) |

### Steady-state "never behind"
`data-freshness.{sh,timer}` (15-min) emits per-domain ingest freshness + the
per-source ADR-0033 verdict to the node_exporter textfile collector; 3 alerts
+ runbooks on r1 Prometheus. Any source past its cadence, a silent timer, or
a real served != lake gap now pages.

### Follow-ups surfaced during P0
- **Blend completeness false-negative: DONE (rc.149).** `compute-completeness`
  never called `preseedFactoryChildren` (only `verify-reconciliation` did), so
  factory-gated childgates were the stale `protocol_contracts` seed. It now
  preseeds from creation events before each re-derive; verdict 15/15 green.
- **SEP-41 is excluded from the Postgres-observer verdict** (`event_index`
  PK-collapse), moot for supply: served from the lake (`supply_flows`, summed
  on demand). The observer audit-trail (`sep41_transfers` per-account
  positions) stays a separate watch-list-gated feature.
- ~~FX-path debt~~ DONE 2026-07-04 (BACKLOG #42): `FXQuoteAtOrBefore` reads
  `fx_quotes` first (exact `*big.Rat` from `rate_usd`, USD-anchored
  inversion/cross, 7-day lookback), falling back to the legacy `trades` path
  (now covering `exchangeratesapi` alone).
  `AggregatorFXSnapFallbackTotal` should go quiet for massive-covered legs.
- **ZFS-dataset drift**: `data/{clickhouse,loki,pgbackrest}` exist on r1 but
  not in the Ansible `zfs_datasets` defaults; reconcile in a dedicated pass.

---

## Phase 1 — Data completeness & backfills

`[OPS]` heavy jobs: run in chunks under the root-<2G watchdog (CH-log
root-fill incident), and verify state before any multi-hour backfill.

| # | Item | Status |
|---|------|--------|
| P1-1 | F-1265 1-year `prices_1m`/CAGG backfill | DONE: `prices_1m` + `prices_1d` back to 2015-11-18 (migration 0031 retention removal + CAGG recompute) |
| P1-2 | **`/v1/tx` `tx_hash_index`**: ordered lookup + MV + 10.2B-row backfill (perf-todo §4) | Code shipped 2026-07-05 (schema + MV + two-step reader with scan fallback + `stellarindex-ops ch-txindex-backfill`). Remaining: apply schema on r1 + windowed backfill (exact invocation in perf-todo §4). Low priority (tx pages noindex) |
| P1-3 | galexie-archive "frozen at 62.2M" | DONE/stale: archive-completeness verified current to 63,259,021 (988,422/988,422 checkpoints); `galexie-archive-fill.timer` keeps it synced |
| P1-4 | **CH Phase 4 `ch-rebuild-projected`**: re-derive projected sources from the lake | Verify post-catch-up (closes rc.107 mis-keyed-forward data). CH-heavy |
| P1-5 | **`ch-supply` partition dedup + re-run**: `supply_flows` had 213M dup rows (820.6M -> 607.4M FINAL) | DONE (deduped). CH-internal: served supply comes from `asset_supply_history` + `supply_1d`; the dup only inflated the lake-side estimate and cost a 40x FINAL-read penalty. Fix was `OPTIMIZE ... PARTITION FINAL` + re-run |
| P1-6 | Broad CAGG recompute | DONE (the 2015-deep materialization) |
| P1-7 | **SEP-41 token supply** complete + served | DONE from the LAKE: `/v1/assets/{id}/supply` (all tokens) + `/v1/assets/{id}` detail (traded tokens, rc.150) sum `supply_flows` FINAL on demand (sum mint - sum burn - sum clawback, basis `sep41_lake_flows`). Complete vs lake, deduped, current; a defensive `ch-supply` gap-fill timer + watchdog keep it so. Postgres observer tables (`sep41_supply_events`/`sep41_transfers`, watch-list-gated, empty) are bypassed |
| P1-8 | Data-truth / Phase-C contract-WASM backfill (`state-snapshot -write -limit 0`; a `-limit`-truncated read is refused) | In progress |
| P1-9 | Pre-P20 ClaimAtom + pre-P23 classic-movement coverage | Low-priority historical caveat |
| P1-10 | **CH Phase 8 decommission**: drop `soroban_events`/old tables, refactor projector to read CH | Do LAST (gated on P1-4 + Phases 5-7) |

Real pending P1 then: P1-2, P1-4, P1-8, P1-10. Run CH-heavy items after the
compute-completeness catch-up to avoid stacking lake I/O.

---

## Phase 2 — Launch-blocking infra (pre-public-flip)

| # | Item | Ref | Status / notes |
|---|------|-----|----------------|
| P2-1 | **Pre-launch hardening**: 9 steps before flipping DNS (loopback bind, CORS narrow, Cloudflare proxy, Stripe secret, Healthchecks URLs, FX keys, smoke, backup baseline) | `pre-launch-hardening.md` | Verified 2026-06-30 via `scripts/ops/pre-launch-check.sh` against r1. Done: 1 `listen_addr = 127.0.0.1:3000` (external :3000 refuses); 2 CORS narrowed to the 4 stellarindex.io hostnames; 3 `trusted_proxy_cidrs`; 6/7 services + heartbeat/smoke timers; Caddy :443; zero `SECURITY:` startup warnings. Remaining (4 FAIL + 2 WARN) is operator account creation: 4x `HEALTHCHECKS_URL_*` into `/etc/default/stellarindex-healthchecks`; deadmansswitch + `DISCORD_WEBHOOK_URL_PAGES`/`_ALERTS` in `/etc/default/alertmanager-secrets`. 4 Cloudflare orange-cloud = recommended, not blocking; 5 Stripe secret = skip; 8 outside-smoke + 9 backup baseline = do at flip. Expect 0 fails after pasting URLs and rerunning |
| P2-2 | Stripe webhook (skipped per goal) + email-verification enforcement | r1-deployment-state | Stripe out of scope. Enforcement is fully built (`RequireEmailVerified` in `internal/api/v1/server.go`, `SignupRequireEmailVerification`, `EmailVerifiedAt`, `/v1/signup/verify` + `MarkEmailVerified`) but deliberately OFF on r1 to avoid the AC7 onboarding dead-end; enabling is a product decision gated on the Resend emailer + an end-to-end verify test |
| P2-3 | **External security review** | L5.6 | Operator procurement; book before public announcement. Standing internal gate: `/security-review` per diff + the audit register (`docs/audit-2026-06-11/`) |
| P2-4 | **Pricing polish** | r1-deployment-state | DONE: (a) `/v1/price/tip?asset=X&quote=fiat:USD` already 200 (param is `asset`+`quote`, not `pair`); (b) `crypto:USDC` 404 fixed by an `aggregate.FiatProxy` self-peg arm at the top of `tryStablecoinFiatProxy` (`internal/api/v1/price.go`): a `crypto:<STABLE>` whose peg fiat == the quote returns `1.0`/`price_type:peg` (USD/EUR/MXN pegs; shared by /price, /price/tip, /observations, /oracle). Bare `?asset=USDC` still 400s (`ParseAsset`); cross-peg `crypto:USDC/fiat:EUR` correctly stays 404; (c) `[aggregate] min_usd_volume = 0` on r1 is an intentional stop-gap (default 10000): re-raise to 10000 once CoinGecko/CEX connectors flow (tracked in P0-3) |
| P2-5 | **Launch-day checklist L6.4 cutover**: DNS flip, public rate-limit tier, public-flip, showcase + status go-live, 24h watch | `launch-day-checklist.md`, L6.4 | Technically live: `api.stellarindex.io` 200 over HTTPS direct to the R1 origin (grey-cloud); `stellarindex.io` via Cloudflare Pages. Remaining (operator): optional orange-cloud for `api.` (P2-1 4), confirm public rate-limit tier, announcement + status-page flip |
| P2-6 | API-walkthrough demo (L6.6) + first 24h watch (L6.7) | L6.6/6.7 | Operator launch-day ops; the data-freshness watchdog + smoke timers are the automated half |

---

## Phase 3 — Multi-region (committed, required for the uptime promise)

| # | Item | Ref | Status |
|---|------|-----|--------|
| P3-1 | **R2 (AWS us-east-1)** provisioning + bringup: galexie reads `aws-public-blockchain` S3 direct; Patroni replica off R1; weekly Tier A+D; `api-r2` DNS | L4.14, ADR-0016 | [OPS] not started |
| P3-2 | **R3 (Vultr)** provisioning + bringup: galexie-archive on Vultr Object Storage hybrid; initial ~6-12h AWS->Vultr bucket fill | L4.15, ADR-0016 | [OPS] not started |
| P3-3 | Cross-region DNS (geo/failover) | L4.16 | not started |
| P3-4 | Cross-region Postgres replication verify (Patroni standby R2/R3 <- R1) | L4.17 | not started |
| P3-5 | Region-failover chaos test | L5.8 | not started |
| P3-6 | Multi-region cutover runbook execution | `multi-region-cutover.md` | [OPS] not started |
| P3-7 | Redis Sentinel ansible sub-role + ha-plan §3.4 fix | ADR-0024 / Task #72 | DONE (verified 2026-06-30): `redis-sentinel` role fully built; §3.4 amended 2026-05-01 + ADR-0024. Remaining: operator deploy onto R2/R3 cache hosts (gated on P3-1/P3-2) |

---

## Phase 4 — Feature / program backlog (ADR-driven)

Standing goal: every event for every major Stellar protocol.

| # | Item | Ref | Notes |
|---|------|-----|-------|
| P4-1 | **Decoder contract-gating**: Phoenix, DeFindex, Aquarius, Comet (Comet has no factory namespace: pool allowlist or WASM-hash gate is open). Soroswap + Blend already gated | ADR-0035 | Each needs seed-protocol-contracts + per-source lake re-derive |
| P4-2 | ~~Supply observers~~ DONE 2026-07-05: three of four were doc drift (`StorageClassicSupplyReader`, `sep41_supply` decoder/reader/refreshers shipped; reserve reader is the chained live-LCM-first fallback). Closed: SEP-1 `max_supply` overlay in `/v1/assets/{id}` (`supply_basis: "sep1_declared_max"`) and `stellarindex-ops supply seed-observations` | ADR-0011/21/22/23 | Operator follow-up: run `supply seed-observations` on r1 (after a state-snapshot account-scope run if reserves are pre-62M-dormant) |
| P4-3 | ~~Explorer Phase C~~ DONE 2026-09-30: `getAccount` served; both data jobs ran genesis-to-tip on r1: `stellar.ledger_entry_changes` 163,658,626,082 rows, `stellar.operation_participants` 4,513,850,669, populated in every 1M-ledger partition 0..64. Measured with `SELECT partition, sum(rows) FROM system.parts WHERE database='stellar' AND table='<t>' AND active GROUP BY partition` | ADR-0038 | Phase B participant-index derive also ran |
| P4-4 | ~~Anomaly Phase 2/3~~ DONE 2026-07-05: cross-oracle agreement served via `confidence_factors.cross_oracle_checked`/`cross_oracle_agreement`; `anomaly.md#stellarindex_anomaly_freeze_engaged` runbook ratified | ADR-0019 | |
| P4-5 | **ADR-0027 LCM cold-tier**: set `s3_cold_bucket_archive` in production and prove one cold read (`stellarindex_ledgerstream_tier_read_total{outcome="cold"}` > 0), then enable the monthly `galexie-archive-trim.timer` (installed by the archival-node role, deliberately not enabled; `ExecStartPre` derives the 90-day cutoff from `ingestion_cursors`). First bulk trim already ran (below ledger 49,984,000, ADR-0043 §2) | ADR-0027, `lcm-cache-tiering.md` Step 5 | [OPS] |
| P4-6 | **i128 enforcement**: the claimed custom golangci analyzer + BIGINT/DOUBLE-refusing migration check did not exist; build them | ADR-0003 | `scripts/ci/lint-i128.sh` and `lint-migrations.sh` now exist |
| P4-7 | `canonical/strkey.go` SDK conversion + remaining SCVal decoder stubs (Soroswap/Aquarius/Phoenix) | ADR-0013 | |
| P4-8 | TWAP `/v1/chart?price_type=twap` (400s) · `change_24h_pct` on asset detail (L7.7) · SEP-41 `usd_volume` pure-Soroban shape (L7.6) | ADR-0020 | Post-launch polish |
| P4-9 | Smaller ADR debts: typed cache-key pkg (ADR-0007), AssetType switch-coverage lint (ADR-0010), CF-range firewall hardening (ADR-0025), DIA mainnet integration (L7.1) | various | Low priority |

---

## Phase 5 — Post-launch (park until after flip)

ADR-0004 Tier-1 own-validators (12mo post-launch) + ADR-0012 quorum-set ADR
(placeholder, gated on validators) · L7.2 99.99% uptime measurement · L7.5
GraphQL · ADR-0006 Parquet/DuckDB tiered storage · ADR-0007 DragonflyDB/KeyDB
revisit · ADR-0009 inline-cached JWT verify.

## Ongoing — doc hygiene

Stale "outstanding" snapshots found 2026-06-30: pgBackRest backups run (timer
fires daily); `sla-probe.timer` active; Blend `BackfillSafe=true`
(coverage-matrix S3.6). coverage-matrix X1.5's "completeness cron timer not
installed" means the *source*-completeness timer (P0-2, now installed), not
the *archive*-completeness timer. Many architecture docs are dated snapshots
(a plan marked "Proposed" while its units shipped):
stamp current status.
