---
title: Production-readiness — comprehensive remaining-work register
last_verified: 2026-07-22
status: living — the single list of everything between here and production
---

# Remaining work to production state

Companion to `production-readiness-master-plan-2026-07-18.md` (which holds the
phase narrative). This file is the **exhaustive checklist**, including the
horizontal-scale roadmap (R2/R3/R4) that the phase doc only gestures at.

Status key: ⬜ to-do · 🔵 in-flight · ✅ done · 🏗️ large project · 🟠 needs an
operator decision · ⛔ ruled out

---

## 1. Data pipeline — the critical path to go-live

| # | Item | Status | Notes |
|---|---|---|---|
| D1 | Archive-walk degraded ranges | ✅ | Both ranges `RANGE COMPLETE`; verified 0 missing ledgers in [2,38M] and [54M,63.05M] |
| D2 | in-CH `intra_ledger_seq` reproject, partitions 39–53 | 🔵 | p45 done + verified; 14 remaining, ~2h each (~28h). Driver: `scripts/ops/d2-ordinal-reproject.sh` |
| D3 | `ledger_entries_current` reproject | ⬜ | `deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql`, windowed, **drop MVs before RENAME** |
| D4 | `projector-replay` → Postgres served tier | ⬜ | INV-3 `derive_generation` guarded. **Also closes the sep41 projection gap ⟹ clears `supply_cross_check_divergence`** |
| D5 | Cleanup: census `DELETE` (`op_index=-1 AND tx_hash='' AND change_type='state'`) | ⬜ | ~1.9M rows/partition. Must run AFTER D2 (D2 deliberately preserves them) |
| D6 | Cleanup: `tx_hash`/`tx_hash_index` → ZSTD | ⬜ | ~0.3–0.5 TiB reclaim |
| E | **Phase E — prove (the go-live gate)** | ⬜ | See `scratchpad/phaseE-prove-gate.md`: contiguity, hash-chain, verify-lake, reconciliation, completeness, reconcile-balances vs Horizon, served-value supply, divergence clear |

**Genesis edge** [2 → 287,404] is likely unfillable (a prior ledger-2 walk left 0
rows) — accept + document, recover via op-census if ever needed.

## 2. Correctness / audit remainder

| Item | Status | Notes |
|---|---|---|
| F2 — auto `-to` resolves to max-in-lake, blind to lake-behind-tip | ⬜ | Needs a true network-tip source; partially mitigated because tools print their resolved range |
| F6 ≡ C2-16 — oracle-reconcile window netting | ⬜ | Deferred with rationale: needs a content-level per-update reconcile on a vintage-stable identity |
| F8 / F9 / F10 | ⬜ | Lower-severity fail-opens from the sweep |
| C2-11 / C2-18 — Soroban topics>4 schema + re-ingest; drop dead `classic_movements` | ⬜ | Structural, `[OP]`-coordinated. Re-ingest reads the topic-complete lake (cheap) |
| C3-7 — `stellarindex-indexer -dry-run` opens a live CH connection | ⬜ | Confirm/close during the E gate |
| M4 — caller-side supply close-timestamp fix | ⬜ | Regression guard landed; caller fix still deferred |
| Same-bug-class day-alignment in `protocol_reader.go` / `protocols.go` / `contracts_list.go` | ⬜ | The 17280 ledger-count window; fixed in `NetworkThroughput` only so far |

## 3. Explorer performance & product

| Item | Status | Notes |
|---|---|---|
| `/v1/accounts` background snapshot | ⬜ | **Won't self-heal.** 214M-row `FINAL` scan, no cache, 100% HTTP 500 at any load. Follow the `CoverageCache` pattern; start at a 30-min interval |
| `/v1/accounts` honest degradation | ⬜ | Currently claims "projection still backfilling, or pricing offline" — the real cause is a query timeout. Return 200 + `flags.degraded` |
| `Cache-Control` on public endpoints | ⬜ | `/v1/ledgers`, `/v1/operations`, `/v1/network/throughput` fall through to `private, no-store`. **Careful:** `/v1/account/*` (singular) is the authed surface vs `/v1/accounts` (plural) public |
| Put `api.stellarindex.io` behind Cloudflare | ⬜ | Verified NOT behind CF (no `cf-*` headers, `via: 1.1 Caddy`) ⟹ every `s-maxage` is currently inert |
| ETag / `If-None-Match` | ⬜ | Zero occurrences repo-wide; cheap 304s on polling endpoints |
| Instrument the Redis (T2) cache layer | ⬜ | Emits no `cache_ops_total`, so the miss-rate alert is blind to it. Redis is 1.7 MB of a 1 GB cap — ~1 GB idle |
| Materialize `ledger_entries_current` aggregates | 🏗️ | `classic_supply_current`, `account_wealth_snapshot` via the aggregator's rollup sweep. Permanent fix for `/assets` + `/accounts` |
| Partition `ledger_entries_current` | 🏗️ | Currently no `PARTITION BY` — why `FINAL` is costly. Low priority if the rollups land |
| Prewarm remaining cache keys | ⬜ | Prewarm warms `limit=199`; the explorer sends 25/100/500 |
| Frontend: unify the 3 embed sparklines; decompose monolith pages | ⬜ | `redesign-readiness.md` |
| `/assets/native` advertises 5,553 markets but lists 100 | ⬜ | Real UX gap |

## 4. Capacity — R1 software levers only

⛔ **R1 is NOT hardware-upgradeable.** Fixed 4× 7.68 TB NVMe, no 5th drive, no
raidz expansion. Never propose a drive upgrade.

| Item | Status | Reclaim | Notes |
|---|---|---|---|
| `galexie-archive` → cold S3, then trim local | 🏗️ | **~5.5 TiB** | Biggest single lever. Config-supported (`s3_cold_bucket_archive`); reads fall through transparently. Cold default = the FREE AWS public dataset. **Do AFTER D1→E** |
| `tx_hash` → `FixedString(32)` | 🏗️ | ~0.6–1 TiB | Stored as 64-char hex today; unique-per-row so it barely compresses. Schema + binary migration |
| TimescaleDB compression policies | ⬜ | ~0.2–0.4 TiB | 19 hypertables are compression-*eligible* but have no policy. Staged: `scripts/ops/add-missing-compression-policies.sql`. **Run post-D4** |
| CH `system.*_log` TTL | ⬜ | ~40 GiB recurring | Drop-in staged; applies at next operator-coordinated CH restart via F2 |
| `max_partition_size_to_drop` raised to 1 TiB | ✅ | — | Needed for D2's `REPLACE PARTITION`; deliberately not unlimited |

## 5. Infrastructure & horizontal scale — R2 / R3 / R4

**The roadmap is horizontal and R1 is transitional.** Scale = new boxes; R1 gets
**retired when near-full**, migrating via bootstrap-from-verified-snapshot.

### 5a. HA latent blockers (must fix BEFORE the first HA deploy)
| Item | Status | Notes |
|---|---|---|
| F-001 — Patroni REST binds 127.0.0.1 vs `ansible_host` | ⬜ | Blocks cross-node health checks |
| F-002 / F-003 — HA metrics unscraped | ⬜ | No visibility into failover readiness |
| F-004 — etcd plaintext, no auth | ⬜ | Security + correctness |
| F-006 — keepalived multicast | ⬜ | May not work in the target network |
| F-009 — config skip | ⬜ | |
| F-010 | ⬜ | |

### 5b. The HA topology itself
| Item | Status | Notes |
|---|---|---|
| Patroni Postgres HA (1 primary + 2 sync replicas) | 🏗️ | ADR-0008 / ADR-0016 |
| **ClickHouse HA — MISSING from ha-plan §3** | 🏗️🟠 | The topology predates ADR-0034. CH is now the **biggest store and a total SPOF**. Recommendation: bootstrap-from-verified-snapshot |
| `ha-plan.md` §3 — add the ClickHouse tier | ⬜ | §4/§8 already rewritten |
| VIP / failover routing | ⬜ | |

### 5c. Machine roadmap
| Item | Status | Notes |
|---|---|---|
| **R2 provisioning** | 🏗️🟠 | Second box. Unblocks HA/DR *and* capacity. Needs an operator decision + budget |
| **R3 provisioning** | 🏗️ | Third box completes the Patroni quorum (1 primary + 2 sync) |
| **R4 provisioning** | 🏗️ | Growth capacity |
| **R1 retirement** | 🏗️ | When near-full: migrate to newer boxes via bootstrap-from-verified-snapshot. **Planned, not a failure** |
| Off-site backup (S3) | 🏗️🟠 | pgbackrest repo is currently the ONLY backup copy |

## 6. Ops hardening (Phase F)

| Item | Status | Notes |
|---|---|---|
| F2 — config drift apply (33 tasks) | ⬜ | api_serving CH profile, CH tuning, ch-supply→journald, captive-core quorum, Loki/MinIO binds, pool alerts. **Blocked on ansible vault secrets** |
| F3 — vault rotation confirm | ⬜🟠 | The encrypted vault was in public git history |
| F3 — galexie v27 build + drift-guard constants | ⬜ | |
| F4 — real DR restore drill | ⬜ | `data/restore-drill` dataset exists but is EMPTY — the drill has never actually run |
| F4 — pgbackrest retention tune | ⬜🟠 | |
| `compute-completeness` systemd timeout | ⬜ | Hit its 3h `TimeoutStartSec` under load and left verdicts 64h stale. Raise it, or make the driver resumable across restarts |
| Alert triage: `sla_probe_unit_failed`, `aggregator_outlier_storm`, `slo_availability_burn_slow` | ⬜ | Currently firing; individually small |
| Dependabot hygiene | ✅ | All merged; ci-health tripwire cleared |

## 7. Security & compliance

| Item | Status | Notes |
|---|---|---|
| Vault secret rotation | ⬜🟠 | Encrypted vault was in public git history |
| API auth / rate-limit review before public launch | ⬜ | |
| TLS cert expiry self-probe | ✅ | F-0051 |
| Dependency advisory cadence | ⬜ | Two CI breaks in one day from newly-published advisories (`x/text`, `sharp`) — both fixed, but the pattern will recur |

## 8. 🟠 Operator decisions needed

1. **HA: v1 requirement or accepted-risk fast-follow?** (gated on a 2nd server)
2. **Stand up R2 now?** — unblocks HA *and* capacity
3. **S3 provider** for archive offload + off-site DR
4. **pgbackrest retention** policy (only backup until off-site exists)
5. **Ansible vault secrets** — `ANSIBLE_VAULT_PASSWORD` + `ANSIBLE_VAULT_FILE_B64` unset; blocks F2 and the `ansible-drift` CI check
6. **`min_usd_volume`** restore-to-10000 (post CS-040)
7. **Served-tier retention / serve-window** policy
8. **Genesis backfill scope** — accept the unfillable edge?
9. **Peg-set thresholds**

## 9. Launch readiness (presentation to Stellar)

| Item | Status |
|---|---|
| Public status page reflects reality (not backfill noise) | ⬜ |
| API reference / docs current | ⬜ |
| SLA definition + error budget | ⬜ |
| Explorer loads "instantly" on all main pages | 🔵 |
| Alert list clean enough to show | 🔵 |
