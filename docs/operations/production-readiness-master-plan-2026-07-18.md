---
title: Production-readiness master plan
last_verified: 2026-07-18
status: active
severity: P1
supersedes_context: docs/operations/runbooks/consolidated-deploy-plan-2026-07-18.md (folded in as Phase C/D)
---

# StellarIndex — production-readiness master plan

The single, honest, dependency-ordered plan to take StellarIndex (Stellar explorer + public pricing API on the R1 Hetzner box) from its current state to a production-live state fit to present to Stellar. Every number here is live-verified on R1 (2026-07-18) or cited to a rehearsed artifact.

## 1. Definition of "production ready" (the bar)

| Dimension | Bar |
|---|---|
| **Data substrate** | `ledger_entry_changes` full-fidelity genesis→tip (or documented, justified boundaries); completeness certified; contiguity proven |
| **Served correctness (money)** | Supply + prices within tolerance vs external truth; cross-check divergence alert clear; i128/decimal/FX correctness proven |
| **Deploy** | Repeatable signed-release pipeline that authenticates and health-checks; migrations apply before binary swap |
| **Capacity** | Disk headroom > 12 months live growth + any planned backfill, with the pool < 85% |
| **Resilience** | The Postgres/CH SPOF addressed (HA) *or* an explicitly accepted, documented risk with tested restore |
| **Observability** | Alerts live for supply divergence, ZFS capacity, ingest lag, service crash-loop; scheduled scans firing |
| **Security/DR** | Vault current; secrets least-privilege; a *tested* restore drill |

## 2. Current honest status (2026-07-18)

**Solid:** activity lake (`ledgers`/`transactions`/`operations`/`operation_results`/`contract_events`) is comprehensive + contiguous genesis→tip; main CI green; scheduled scans now firing; 52-finding audit remediation (PR #6) + un-audited-surfaces remediation merged; migrations 0109–0114 rehearsed up/down; the CH reproject DDL + post-deploy sequence rehearsed; prices already < 0.25% accurate.

**The blockers, in priority order:**
1. **🔴 Capacity + a wider-than-documented data gap** (this plan's long pole — see Phase A/D).
2. **🔴 Deploy pipeline can't authenticate** — `R1_HOST`/`DEPLOY_SSH_PRIVATE_KEY`/`R1_SSH_KNOWN_HOSTS` absent (`[OP]`).
3. **🟠 Served supply correctness unproven** (prices fine; supply is the open cross-check).
4. **🟠 SPOF** — HA roles ratified but undeployed, with latent deploy-blocking findings.

## 3. The critical path (phased, dependency-ordered)

### Phase A — Capacity relief  🔴 gates all data work
The comprehensive fill needs ~2–2.4 TiB of new data; only **943 GiB is free (pool 94%) and shrinking** from live ingest + Phase 0. Measured levers make it fit *without hardware*, though a modest expansion buys margin.

- **A0 — halt the collision — ✅ DONE (2026-07-18).** Discovered `/root/phase0.sh` was an **unattended 3-day `for`-loop** backfilling `[38.1M→62M]` in 4M chunks, already advanced into the degraded `[54.1→58.1M]` chunk. Its per-chunk wrapper `run-heavy-job.sh` watches **only the 49 G root FS, not the ZFS data pool** — so nothing would have stopped it filling the shared pool and breaking **live ingest** (~2 TiB of writes remained; ~0.94 TiB free). Halted cleanly: killed the orchestrator (PID 3809732) + gracefully stopped the running scope (checkpoint flushed, resumable). Live serving/ingest unaffected. **Completed chunks `[38M→54M]` are kept** (full-fidelity; need only the in-CH ordinal). **Do not re-run `/root/phase0.sh` or `/root/phase0-seam.sh` — the remaining `[54→62M]` + the seam `[62→63.05M]` fold into Phase D's disk-safe walk.**
- **A0b — fix the watchdog gap (F-phase):** `run-heavy-job.sh` must guard the **data pool** (`zpool free` / `df /var/lib/clickhouse`), not just root — otherwise any heavy job can silently run CH out of disk and take down live ingest. This is a real reliability bug, not just a Phase-0 artifact.
- **A1 — prune pgbackrest** (fast ~1 TiB): 2.52 TiB is ~13 daily diffs off one full; set `repo1-retention-diff` to keep ~5 days. `[OP]` decision (shortens PITR window). Immediate headroom.
- **A2 — recompress the XDR columns to ZSTD** (**measured 1.75× on `entry_xdr`**): `ALTER … MODIFY COLUMN entry_xdr CODEC(ZSTD(3))` + `key_xdr`, then `OPTIMIZE … PARTITION p FINAL` **one partition at a time** (transient ≤ one partition ≈ 424 GiB, well within free space; each pass *grows* free space). Reclaims **~1.5 TiB** from existing data, and every subsequently-backfilled row lands ~40% smaller — so the comprehensive dataset ≈ fits the current envelope.
- **A3 — (recommended) storage expansion:** the pool was already grown once; adding NVMe for ~3–4 TiB headroom turns "tight" into "comfortable" and covers 12-month live growth. `[OP]` / hardware.
- **Gate:** `free_space > (measured comprehensive-fill size) + 15%`, proven by measurement — not before.

### Phase B — Deploy enablement  🔴 `[OP]`, parallelizable with A
- **B1** set `R1_HOST`, `DEPLOY_SSH_PRIVATE_KEY`, `R1_SSH_KNOWN_HOSTS` (or run `deploy-binary.yml` by hand from an SSH-capable controller with `-e` vars).
- **B2** rename the Cloudflare secret `CLOUDFLARE_API_SECRET` → `CLOUDFLARE_API_TOKEN` (unblocks the frontend deploy).
- **B3** cut a signed release (`release.yml`, e.g. `v0.17.0`) on the intended main SHA; verify sigstore artefacts.

### Phase C — Deploy binary + migrations  (needs B; small disk footprint)
Order matters (silent-wedge traps caught in audit):
1. Add CH `intra_ledger_seq UInt32 DEFAULT 0` **before** the binary swap (metadata-only, old-binary-safe).
2. `deploy.yml` with `binaries="…,stellarindex-ops,stellarindex-migrate,…"` (default excludes `stellarindex-ops` → silent no-op). Migrations 0109–0114 apply first with `lock_timeout` in a low-write window (R1 = TSDB 2.26.4 → `ADD COLUMN` metadata-only).
3. Refresh the host `stellarindex-ops-ch` binary (`cp` the deployed ops binary).
4. Verify: `schema_migrations=0114`; services active; **CH-lake tip advancing** (not just PG); `/v1/*` serving.

### Phase D — Comprehensive data backfill + reprojects  🔴 (needs A + C)
The fidelity is a patchwork — full only `[38M→~54M]` + `[~63M→tip]`; degraded `[genesis→38M]` + `[54M→~63M]`. Per-op rows are **absent from CH** (code-traced), so the degraded ranges need an **archive walk**; only `[63M→tip]` can be fixed in-CH.
- **D1 — archive-walk the degraded ranges** `[genesis→38M]` + `[54M→~63M]`, **per-partition, disk-safe:** drop-old-partition-before-reinsert (or staging + `REPLACE PARTITION`) to bound transient to ≤ one partition; **fresh state file** (the existing `done-windows.txt` already marks genesis→62M "done" → would silently skip everything); new binary; `PAR≤3`; monitor ZFS. Lands as ZSTD.
- **D2 — in-CH ordinal reproject for `[38M→54M]` + `[63M→tip]`** (everywhere the per-op rows already exist): `intra_ledger_seq = row_number() OVER (PARTITION BY ledger_seq ORDER BY tx_index, change_index) - 1` (join `transactions` on `(ledger_seq, tx_hash)`). **Not** `op_index` in the ORDER BY (reintroduces the update→remove bug). Hours, no walk. **Verified (2026-07-18):** `git diff 0dcf4636..main` on the extractor is import-rename + the ordinal threading ONLY — Phase 0's `[38–54M]` rows are content-identical to the improved binary's, so they need the ordinal, not re-extraction. Certify with `reconcile-balances`/`verify-contiguity` in Phase E.
- **D3 — `ledger_entries_current` reproject** (`ledger_entries_current_intra_ledger_seq.sql`, windowed, drop-MVs-before-rename).
- **D4 — PG served-tier corrected re-derives** (`projector-replay`, bounded via the INV-3 `derive_generation` keystone) — lands the money-correctness fixes as one pass.
- Genesis-edge `[2→287,404]` is **likely unfillable** (a prior ledger-2 walk left 0 rows) — accept + document, recover via op-replay if ever needed.
- **C2-11** soroban >4-topic re-ingest reads the topic-complete lake (Tier-3, cheap — no walk).

### Phase E — Prove correctness  (the go-live gate)
`reconcile-balances` + `compute-completeness`; reconcile served **supply** + **prices** vs external truth (CoinGecko/Chainlink); the `supply_cross_check_divergence` alert must clear. Prices already pass; supply is the one to confirm. DAT-10.

### Phase F — Production hardening
- **F1 — HA (removes the SPOF):** fix the latent deploy-blockers first (F-001 Patroni REST bind 127.0.0.1 vs `ansible_host`; F-004 etcd plaintext/no-auth on RFC1918; F-002/003 unscraped HA metrics; F-006 keepalived multicast on VPC; F-009 first-run-only config skip), **then** first HA deploy. *Decision: HA as a v1 requirement, or a documented accepted-risk fast-follow?*
- **F2 — config drift apply** (33 tasks: `api_serving` CH profile, CH tuning, ch-supply→journald, captive-core T1 quorum, Loki/MinIO loopback binds, pool alerts) — `--check` first (rehearsed clean post-Jinja-fix). Post-capacity.
- **F3 — `[OP]`:** confirm vault rotation (encrypted vault was in public git history); build galexie v27 + reconcile drift-guard constants.
- **F4 — DR:** run a real restore drill (the `data/restore-drill` dataset exists but is empty); tune pgbackrest retention (ties to A1).
- **F5 — hygiene:** merge dependabot PRs #2–#5 (deps) on green CI.

## 4. `[OP]` items (need you / off-repo)
1. ~~**Deploy secrets** (B1)~~ — **DONE (2026-07-18):** `DEPLOY_SSH_PRIVATE_KEY`, `R1_HOST`, `R1_SSH_KNOWN_HOSTS` set (verified via Actions API); deploy pubkey in R1 root `authorized_keys`. (`R1_USER` unset = defaults to `root`, correct.)
2. ~~**Cloudflare token** (B2)~~ — **DONE:** `CLOUDFLARE_API_TOKEN` now present → frontend deploy unblocked. (Old `CLOUDFLARE_API_SECRET` still present — harmless leftover, can delete.)
3. **pgbackrest retention decision** (A1) + **storage-expansion decision** (A3).
4. **Vault rotation confirm** + **galexie v27 build** (F3).
5. **Ansible vault secrets NOT set** — `ANSIBLE_VAULT_PASSWORD` + `ANSIBLE_VAULT_FILE_B64` absent. **Not needed for the binary deploy (Phase C)**, but required for the config-drift apply (Phase F2) + the `ansible-drift` CI check. Set before Phase F.
6. ~~Re-register cron schedules~~ — **resolved** (schedules now firing).

## 5. Realistic timeline & effort
| Phase | Effort | Nature |
|---|---|---|
| A capacity | ~1 wk | pgbackrest prune fast; recompress multi-day background |
| B enablement | days | `[OP]`-gated |
| C deploy | ~1 day | attended |
| D backfill | ~1–2 wk | heavy, per-partition, mostly background/automatable |
| E prove | days | attended |
| F harden | ~1–2 wk | HA + config + DR |

**Long pole = A + D (the data substrate).** Core "present to Stellar" state ≈ **3–5 weeks**, gated on the `[OP]` items landing early. A and B can run in parallel now.

## 6. Key risks & open decisions
- **Capacity is the true #1** — no data work is safe until Phase A proves headroom. The box is at 94% and growing from live ingest alone (~months of runway before *any* backfill).
- **HA v1-or-fast-follow** is a genuine product decision — a single-box money system is a real SPOF.
- **Recompression is itself heavy** — it must be per-partition and monitored; it's the enabler, so it's first.
- **"Never again" holds structurally** once D lands: RMT idempotent-corrective + INV-3 `derive_generation` → future extractor fixes are bounded re-derives of only the affected range, never from-genesis.

## 7. Related
- Deploy detail: `runbooks/consolidated-deploy-plan-2026-07-18.md` (Phase C/D, now BLOCKED-on-capacity).
- Post-Phase-0 sequence: `runbooks/post-phase0-deploy-sequence.md`. Reproject DDL: `deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql`.
- Audit records: `docs/audit/audit-2026-07-16/` + `docs/audit/audit-2026-07-18-unaudited-surfaces/findings-ledger.md`.
