---
title: Consolidated deploy plan — cancel Phase 0 + deploy + single re-derive
last_verified: 2026-07-18
status: audited-NOT-YET (go-conditions below)
severity: P1
---

# Consolidated deploy plan (cancel Phase 0 → deploy → one re-derive)

**Rationale:** Phase 0 is `ch-backfill … -from 50115806 -to 54115805` on the **old** binary — the *same* operation the post-deploy re-derive runs on the *new* binary, over an overlapping range. Finishing Phase 0 then re-deriving does the same multi-day work twice. Consolidate: cancel Phase 0, deploy, run **one** `ch-backfill [genesis→tip]` on the new binary that fills the entry-change genesis-edge gap **and** closes Phase 0's op-fidelity gap **and** populates `intra_ledger_seq` **and** writes the corrected extraction — one contiguous pass, full-fidelity across all of Stellar history under a single binary.

## Lake-completeness attestation (2026-07-18, live-verified on R1)
Confirmed the "capture once, decode forever" substrate is comprehensive across **all eras** at the ledger/tx/op/event level, with two boundaries on the state-diff table:
- **`stellar.ledgers` (commit spine): [2 → tip], contiguous, ZERO missing** — every ledger in Stellar history present.
- **`transactions`/`operations`/`operation_results`/`contract_events`: [3 → tip]** — full activity history, classic **and** Soroban eras (contract_events holds 12.7B pre-Soroban rows — it captures classic events too).
- **`ledger_entry_changes` (state diffs): two boundaries →** (1) a genesis-edge floor at 287,404 (below it 0 rows; only 42 rows in the first 1M ledgers — the 2015 near-empty era); (2) the [38.1M→62M] op-fidelity gap. **This genesis→tip re-derive closes both** (`287,404` is not a code floor — grep-confirmed — just where the old backfill started; the archive has the genesis data, proven by tx/op reaching ledger 3).

### Why genesis→tip, not [38M→tip] (the "never again" decision)
User decision (2026-07-18): comprehensive, no compromise for simplicity, never repeat this backfill. One contiguous genesis→tip pass gives **uniform full-fidelity provenance across all history under one binary** — no seam between "old-binary below 38M" and "new-binary above," and it fills the genesis-edge gap in the same pass. After it, the entry-change substrate is complete and correct end-to-end; future extractor fixes are **bounded, idempotent-corrective re-derives of only the affected range** (RMT overwrite + the INV-3 `derive_generation` keystone) — never a from-genesis rebuild. That is the structural guarantee that this is a one-time cost. *(Narrower alternative, if the extra ~1–2 days aren't wanted: fill only [2→287,404] + re-derive [38M→tip], leaving the proven-correct [287,404→38M] middle untouched — comprehensive but with a provenance seam. Recommended: the full pass.)*

## AUDIT VERDICT (2026-07-18): NOT-YET — three independent reviewers
The consolidation logic is **verified sound** (the new-binary `ch-backfill` genuinely supersedes Phase 0; cancelling breaks no dependency — the "gate" is data-derived, not a flag). But the first draft had **two silent-failure traps + a live-lock exposure**, and the deploy is **blocked on operator-only secrets**. All corrections are folded into the sequence below. Do not execute until the **Go-conditions** hold.

### Defects the audit caught (now fixed in the sequence)
1. **Ordering:** deploying the new binary *before* adding the CH `intra_ledger_seq` column **silently wedges live lake ingest** (the 13-col INSERT fails → whole flush aborts → the commit-marker/watermark freeze; Postgres stays green so the `is-active` probe misses it). → **Add the column FIRST** (metadata-only, `DEFAULT 0`, old-binary-safe; column confirmed absent on R1, live_sink confirmed `true`).
2. **Deploy scope:** `deploy.yml`'s default `binaries` **excludes `stellarindex-ops`** → the ~4-day re-derive would run the **old** extractor → `intra_ledger_seq` never populated → **silent no-op**. → **Pin `binaries=…,stellarindex-ops`.**
3. **`-ch` binary:** the driver `scripts/ops/ch-full-backfill.sh` defaults `OPS=/usr/local/bin/stellarindex-ops-ch` — a **separate, uncodified host binary** the deploy never updates. → After deploy, `cp -f /usr/local/bin/stellarindex-ops /usr/local/bin/stellarindex-ops-ch` (or pass `OPS=`); verify `stellarindex-ops-ch version` = new tag.
4. **ch-backfill flags:** the naive command misses `-config` (fails) and defaults the **trimmed** bucket. → Use `scripts/ops/ch-full-backfill.sh` with `BUCKET=galexie-archive`, `-ch-addr 127.0.0.1:9300`, **pinned `PAR≤3 flush-every≤200`** (heavier profile than Phase 0 — it writes more per ledger), `-from 2` (genesis, to fill the entry-change edge gap), `-to` = `SELECT max(ledger_seq) FROM stellar.ledgers` (~63.5M; NOT `ledger_entry_changes`).
5. **Migration locks:** 0109–0114 set no `lock_timeout`; targets are hot compressed money tables (`trades`/`soroban_events`/`oracle_updates`/`asset_supply_history` — confirmed compressed). R1 is **TSDB 2.26.4** so `ADD COLUMN` is metadata-only (no rewrite/error), but the `ACCESS EXCLUSIVE` grab can convoy live writes; `0110` holds it across ~25 ALTERs in one txn. → Apply with a short `lock_timeout`+retry in a low-write window (or a brief sink pause).
6. **Rollback wording:** post-ingest, `migrate down` is **NOT** the prod lever (`0112` down fails / `0114` down loses data). → **Rollback = binary-revert + roll-forward.** Point of no return = the moment the new binary begins ingesting.

## Go-conditions (hard gate — ALL must hold before execution)
- [ ] **[OP] Deploy secrets set** (currently **absent** — verified): `R1_HOST`, `DEPLOY_SSH_PRIVATE_KEY`, `R1_SSH_KNOWN_HOSTS` (`R1_USER` optional). *(Or run `deploy-binary.yml` by hand from a controller with SSH + `-e` vars — no vault needed.)*
- [ ] **[OP] Confirm** `/etc/default/stellarindex` on R1 defines `STELLARINDEX_POSTGRES_DSN` (the migrate step sources it).
- [ ] **Version decision** — a clean tag (latest is `v0.16.3` → e.g. `v0.17.0`).
- [ ] **Fresh backups** immediately before Step 4 (pgBackRest + CH DDL/cursor snapshot).
- [ ] **Low-write window** identified for the migration ALTERs (lock convoy mitigation).
- [ ] **[OP] Codify or hand-verify** the host-only `run-heavy-job.sh` + `stellarindex-ops-ch` (not in the repo — a separate finding).

## Sequence (corrected)
1. **Cut + push the tag** `v0.17.0` on the intended main SHA (auto-runs `release.yml` → signed artefacts, all 6 binaries built). Verify the Release + `SHA256SUMS.sigstore.json`.
2. **Cancel Phase 0:** `systemctl list-units '*.scope'` to confirm, then `systemctl stop heavy-phase0-ec-2952240.scope`. Clean (SIGTERM→ctx-cancel, not fatal; RMT-idempotent; flock released on cgroup kill; live sink untouched; no cursor). Superseded by Step 5's `[38M→tip]`.
3. **Add the CH column NOW (old-binary-safe):** `clickhouse-client --port 9300 -q "ALTER TABLE stellar.ledger_entry_changes ADD COLUMN IF NOT EXISTS intra_ledger_seq UInt32 DEFAULT 0 AFTER balance;"` — must precede the binary swap.
4. **Deploy binaries + migrations (first hard-to-reverse point):** `gh workflow run deploy.yml -f region=r1 -f version=v0.17.0 -f binaries="stellarindex-indexer,stellarindex-aggregator,stellarindex-api,stellarindex-ops,stellarindex-migrate,stellarindex-sla-probe"`. Migrations 0109–0114 apply first (idempotent), with a `lock_timeout` in a low-write window. **Verify:** `schema_migrations=0114`; services active; **CH-lake tip advancing** (not just PG); `/v1/*` serving.
5. **[OP] Refresh the re-derive binary:** `cp -f /usr/local/bin/stellarindex-ops /usr/local/bin/stellarindex-ops-ch`; verify version.
6. **Single re-derive — GENESIS→tip (comprehensive, one-time):** `run-heavy-job.sh phase-rederive env FROM=2 TO=<tip> BUCKET=galexie-archive PAR=3 WINDOW=1000000 OPS=/usr/local/bin/stellarindex-ops bash scripts/ops/ch-full-backfill.sh` (tip from `stellar.ledgers`). One contiguous pass: (a) **fills the entry-change genesis-edge gap [2 → 287,404]** (currently 0 rows); (b) closes the [38.1M→62M] op-fidelity gap; (c) populates `intra_ledger_seq` everywhere (0 below 38M = correct — no ties there). Idempotent-corrective (RMT overwrite, no truncate, resumable). Cost: full ~63.5M ledgers, but the pre-40M era is sparse (low change-volume → fast walk) so ≈ **+1–2 days over the dense-range-only plan → ~5–7 days total**. Watch the CH memory-guard + `stellarindex_galexie_catchup_refused`. **Verify after:** `min(ledger_seq)` in `ledger_entry_changes` ≤ 3 (gap filled) and the [2,1M) count is no longer ~42.
7. **Reproject** `deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql` Steps 1–5 (`clickhouse-client --port 9300`, windowed **templates** — not a whole-file pipe; drop-MVs-before-rename cutover). **Current-state reproject window stays [38M→tip]** — that's where same-ledger ties exist; the genesis fill affects *point-in-time historical* state, not current state (2015 entries are superseded by recent changes). Verify tie keys show `v2 intra_ledger_seq > 0`.
8. **PG served-tier corrected re-derives** (`projector-replay`, incremental via INV-3) — these land the M-series *served values* (the ch-backfill only fixes the *lake substrate*).
9. **C2-11 re-ingest + `reconcile-balances` + `compute-completeness` + PROVE (DAT-10)** vs external truth; supply cross-check divergence must clear.

## Separate, later (NOT this plan)
The 33-task config apply (`archival-node.yml`: serving/warehouse isolation, binds, alerts) + galexie v27 (after building it + reconciling the drift-guard constants). The frontend deploy is independent — **it's blocked because the CF token is set as `CLOUDFLARE_API_SECRET` but the workflow reads `CLOUDFLARE_API_TOKEN`; rename it and it unblocks.**

## Rollback model
Steps 1–3 fully reversible. **Point of no return = the new binary ingesting (Step 4).** Rollback thereafter = **re-deploy the prior tag (binary revert) + roll schema forward** — NOT `migrate down`. The reproject keeps v1 serving until the ms-cutover and retains `_old` for a rename-back.
