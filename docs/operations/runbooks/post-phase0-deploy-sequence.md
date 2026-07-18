---
title: Runbook — post-Phase-0 deploy sequence
last_verified: 2026-07-18
status: rehearsed
severity: P1
---

# Runbook — post-Phase-0 deploy sequence

The ordered, **rehearsed** sequence to run once ADR-0047 Phase 0 finishes on R1 and the deploy freeze lifts. It lands the whole remediation backlog (audit-2026-07-16 + the 2026-07-18 un-audited-surfaces work) as a clean tagged release, applies the schema + reprojects, re-derives the corrected served values once, and proves correctness.

## At a glance
| Field | Value |
| ----- | ----- |
| When | After Phase 0 completes (~ledger tip catches the [38.1M→62M] re-derive); freeze lifts |
| Risk | HIGH — swaps the ops binary, applies 6 migrations + a CH engine rebuild, heavy re-derives on the SPOF |
| Rehearsed | 2026-07-18 (scratch Docker Postgres + ClickHouse 24.8) — migration chain ✓ up/down; CH reproject DDL ✓ verbatim + corrects + rolls back; ops verbs ✓ exist |
| Rollback | Per-step below; the CH reproject keeps v1 serving until the ms-cutover and retains `_old` for a rename-back |

## Preconditions (go gate)
- [ ] **Phase 0 done** — the ch-backfill re-derive reached tip; `completeness_incomplete` cleared; merges settled.
- [ ] **[OP] secrets set** — `R1_SSH_KNOWN_HOSTS` (the hardened ansible-drift/deploy now fail-close without it), `ANSIBLE_VAULT_PASSWORD` current (drift assert), and — if deploying the frontend the same window — `CLOUDFLARE_API_TOKEN`.
- [ ] **galexie decision** — the deploy will try to upgrade galexie v26→v27 (drift-guard constants still need reconciling from a real v27 build; see audit deps F-002). Either build+reconcile v27 first, or `--skip-tags galexie` on the apply and do galexie as a separate step.
- [ ] **Backups** — a fresh pgBackRest backup + a ClickHouse DDL/cursor snapshot immediately before starting.

## The sequence

### 1. Cut a clean tagged release
`release.yml` (workflow_dispatch) → signed `v*` artefacts. **Not** another ad-hoc `-oob` build. Verify the release exists.

### 2. Deploy (this ALSO applies the migrations — one atomic step)
`deploy.yml` workflow_dispatch with the tag. `deploy-binary.yml` **syncs the migrations dir and runs `stellarindex-migrate up` BEFORE swapping any binary** (F-1220) — so migrations **0109–0114** apply here.
- **Rehearsed:** the full chain 0001→0114 applies up and rolls back down cleanly (scratch PG). 0113 DROPs the dead `classic_movements`; 0114 adds `soroban_events.topics_xdr`.
- **Verify:** `schema_migrations` at 0114; the three stellarindex services healthy; live tip advancing.
- **Rollback:** deploy the prior tag; `stellarindex-migrate down` is reversible per the round-trip test (but avoid down-migrating money tables with live data — prefer roll-forward).

### 3. ClickHouse `ledger_entries_current` reproject (C2-4c)  — heavy, windowed
Run `deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql` Steps 0–5 under `run-heavy-job.sh`, off-peak, **windowed** (never one statement).
- **Rehearsed:** every statement ran verbatim on scratch CH 24.8, zero DDL errors; it corrected a same-ledger update→remove tie (v1 resurrected a deleted entry; v2 → `removed`, `version=(ledger<<32)|ils`); cutover + both rollbacks clean; v2 matches `tier1_schema.sql` exactly.
- **⚠️ The reproject is a *shape* change — it does NOT by itself fix legacy history.** It only makes ties deterministic where `intra_ledger_seq` is populated. Historical rows are all `intra_ledger_seq=0`, so legacy same-ledger ties still resolve arbitrarily, **and the Step-3 verify query falsely reports them "corrected"** (only tell: `v2 intra_ledger_seq=0` ⇒ UNRESOLVED). So a required re-derive precedes the reproject — see **Step 3a**.

### 3a. Re-derive `ledger_entry_changes` over the tie range — REQUIRED (100%-accuracy decision, 2026-07-18)
No compromise on integrity: fully resolve legacy ties, not a targeted subset.
- **Command:** `stellarindex-ops ch-backfill -from 38000000 -to <tip>` with the deployed **new** binary, **windowed** under `run-heavy-job.sh`, off-peak.
- **Idempotent-corrective:** `ledger_entry_changes` is a ReplacingMergeTree, so the re-run **overwrites in place** (re-inserts each change with its real `intra_ledger_seq`, the newer version winning) — **no truncate, no duplication, resumable.** It re-runs the C2-4c-fixed extractor (`extract_entry_changes.go`).
- **Range is PROVEN complete:** a full scan of genesis (287,404) → 37,999,997 found **zero** same-ledger multi-changes; they begin in (38M, 40M]. So `[38,000,000 → tip]` covers 100% of tie-bearing history with no reliance on sampling. Below 38M every key has ≤1 change/ledger — `intra_ledger_seq=0` is unambiguous there.
- **Cost:** ~25.5M ledgers ≈ Phase-0-scale (~3–4 days). The tie-floor (38M) equals Phase 0's floor (38.1M) — the same real data boundary — so this re-does Phase 0's range on the new binary (unavoidable: `intra_ledger_seq` can only be populated by re-extracting, which requires the deployed fix).
- **Then** the Step-2 reproject windows below run over `[38M → tip]`, yielding 100%-correct current state. **Verify:** every reprojected same-ledger tie key shows `v2 intra_ledger_seq > 0` (a `0` means that range wasn't re-derived).
- **⚠️ Ordering (validated):** Step 4 **must** drop both MVs *before* the RENAME — renaming a target out from under its MV throws `Code 60` on the next insert into the **source** `ledger_entry_changes`, which would **stall live ingest**, not just the projection.
- **Rollback:** before cutover, just `DROP … v2` (v1 never stopped serving); after cutover but before Step 5, rename `_old` back.

### 4. Corrected re-derives — ONCE, incremental (INV-3 keystone)
With 0109/0110's `derive_generation` guard, `projector-replay` now **overwrites the affected ranges in place** (DO UPDATE) instead of no-op'ing — a *bounded* re-project, not a from-genesis rebuild. Land the known correctness fixes (decimals, FX-snap, supply close-time, protocol tables) as this one pass.
- **Verify:** a spot re-project of a known-wrong range returns `rowsUpdated>0` (not the old `rowsInserted=0` no-op) and the served value corrects.

### 5. C2-11 soroban topic re-ingest + `ch-supply` catch-up
- `stellarindex-ops projector-replay -ch -from <soroban-genesis> -to <lake-tip>` — recovers >4-topic events from the topic-complete lake into the widened `soroban_events`.
- `ch-supply` gap-fill catches up automatically (the journald fix + the new `stellarindex_ch_supply_gapfill_failed` alert mean a failure is now loud, not silent).

### 6. Apply the ansible config (the 33-task drift)  — `--check` first
`ansible-playbook … --check --diff` (rehearsed clean after the Jinja fix, PR #13), then apply. Lands: the `api_serving` CH profile (serving/warehouse isolation), CH tuning drop-ins, ch-supply→journald, pool-capacity alerts, captive-core T1 quorum (galexie restart, brief live-tip pause), Loki/MinIO loopback binds (service restarts).

### 7. Prove (DAT-10) — the go-live gate
Reconcile served **total supply** and **prices** against external truth (CoinGecko/Chainlink live). Prices were already <0.25% accurate; **supply is the one to confirm** (the cross-check divergence alert must clear). Run `reconcile-balances` + `compute-completeness`; confirm no `supply_cross_check_divergence`.

## Rollback summary
Each step is independently reversible (release retag, `migrate down`, `DROP v2` / rename-back, re-project with the prior generation). The safest posture: **roll forward** on money tables; only the CH reproject and the binary are true swaps, and both keep the prior state live until an atomic cutover.

## Timing / sequencing notes
- Steps 3–5 are the multi-hour heavy ops — schedule off-peak, one window at a time, under the heavy-job flock.
- If historical `ledger_entry_changes` correctness is wanted, its re-derive is the heaviest single op and must precede Step 3's windows for the ranges you care about.
- The frontend (supply + money fixes) deploys independently via Cloudflare — not gated on this sequence, but currently blocked on `CLOUDFLARE_API_TOKEN`.

## Related
- The reproject artefact: `deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql` (C2-4c).
- Migrations `migrations/0109`–`0114`; the deploy playbook `configs/ansible/playbooks/deploy-binary.yml`.
- The go-live master plan: `docs/audit/audit-2026-07-16/go-live-master-plan.md`; the un-audited-surfaces ledger + [OP] list: `docs/audit/audit-2026-07-18-unaudited-surfaces/findings-ledger.md`.
- Companion incident runbooks: `zfs-pool-full.md`, `ch-supply-gapfill-failed.md`, `stellar-stack-version-lag.md`.

