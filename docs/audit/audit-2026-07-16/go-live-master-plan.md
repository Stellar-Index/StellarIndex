# Stellar Index — go-live master plan

Living reference for everything remaining before a public showcase + being the canonical Stellar source. Compiled 2026-07-17 from: the 2026-07-16 code audit, a live read-only R1 investigation over SSH, and the web-frontend audit. Supersedes ad-hoc status notes.

## North star (2026-07-17)

**Be the true top-tier, canonical, comprehensive Stellar explorer** — where people go for any transaction, smart-contract invocation, asset, or protocol (StellarExpert/Horizon-class). This is a *warehouse-grade* ambition, and it settles the biggest design question:

- **The full-fidelity, certified-complete, forever raw lake is the RIGHT foundation — resource it, do NOT lean it.** A comprehensive explorer must keep every op-result / tx / contract-event forever and prove completeness. So: no TTLs on the raw lake; the storage/HA problem is an *investment*, not a *trim*.
- **The gaps to top-tier are not data — the lake has the data — they are:** (1) infrastructure / HA / scale; (2) explorer UI depth; (3) point-lookup performance; (4) generic Soroban contract decoding. See "Road to top-tier" below.

## Go-live gate (what must be true before showcasing)

- [ ] **Supply is trustworthy** — cross-check divergence cleared (fixes deployed + re-derived). *Prices are already verified accurate (<0.25% vs CoinGecko/Chainlink live); supply is not yet showcase-grade.*
- [ ] **The frontend showstopper is fixed** — asset supply no longer renders 10^7× too large (see Web §). Frontend deploys independently of R1, so this can ship first.
- [ ] **The remediation is deployed** — R1 currently runs pre-fix code; every audited bug is still live until deploy.
- [ ] **Completeness is complete** — Phase 0 finished; `completeness_incomplete` cleared.
- [ ] **A story for the single-box SPOF** — at minimum backups off-box + an honest availability posture; ideally R2 online.
- [ ] External security review closed; SEV-1/2 paging drill done; rollback rehearsed (the project's own launch-day checklist).

---

## 1. Ship the remediation (PR #6 — 52 findings, all CI-green)
- [ ] **Merge PR #6** (safe any time — merge ≠ deploy).
- [ ] **After Phase 0:** deploy as a **clean tagged release** (not another `-oob` ad-hoc build) → apply migrations 0109–0112 → run the corrected re-derives **once** (heavy — schedule as the next phase after Phase 0).
- [ ] **Prove it (DAT-10):** reconcile served prices + total supply against external truth post-deploy.

## 2. Remaining audit findings (in flight / deferred)
- **In flight (agents running while Phase 0 runs):** C3-14/C3-17/C3-20 (config/security), M4-callers (supply timestamps), the web cluster (below).
- **Deferred, needs its own pass:** C2-11 (>4-topic truncation — PG soroban_events only; **CH lake is topic-complete, verified**, and the feed-switch reads CH, so this is lower-urgency than it looked), C2-4c (ledger_entries_current reproject), C2-16 (oracle reconcile — content-level design), C2-18 (dead-table DROP).

## 3. Web frontend (the showcase surface — audit done)
- [ ] **CRITICAL — supply 10^decimals too large** on asset listing + detail + sidebar (USDC shows "2.76Q" not 275.79M; contradicts market-cap on the same row). Convention drift — `SupplyTabPanel` already divides correctly; 3 components don't. *(fixer running)*
- [ ] Micro-price "$0.00" / "0.0000" formatting in sidebar + exchange/market tables (reuse the existing `toExponential` formatters). *(fixer running)*
- [ ] Sanitization gaps: changelog markdown lacks `isSafeHref`; `runbook_url` + SEP-1 icon hrefs unvalidated. *(fixer running)*
- **Verified SOUND:** JSON-LD escaping, home-domain gating, `isSafeHref` (C4-13), BigInt-exact stroop math.
- **NOT yet audited (large surface, directly relevant to the ambition):** contracts/[id], ledgers, network internals, dexes, lending, mev, sources, protocols, dashboard/auth flows, SearchModal, chart internals; **no accessibility pass**. → fold into the top-tier gap analysis.

## 4. Live R1 operational findings (new — not in the code audit)
- [x] **`ch-supply` was dead** — failed every run on a log-file permission error (07-03 non-root hardening broke ownership); supply gap-fill silently off for weeks. *Fixed on R1 (chown); needs a codified fix (ansible/journald) so it doesn't drift back.*
- [ ] **Supply cross-check divergence alert firing** — the headline correctness gap (targets: C2-4, M4, the revived gap-fill). Measure magnitude + confirm it clears post-deploy.
- [ ] **`ansible-drift` job has run 0 times** — the codified-≠-live guarantee never executes (needs the CI SSH secret, like the promtool/gitleaks secrets were dead). **Config drift is unmonitored.** Fix the job + run one `--check --diff`.
- [ ] **Manual-edit drift on R1** — `/etc/prometheus/rules.d.bak-*` + `rules.r1.bak-*` are leftovers from hand-edits (07-07). Reconcile to code.
- [ ] **No pool-capacity alert** — nothing alerts on the 90% pool. Add one (codified + loaded).
- [ ] **API p95-breach under Phase-0 load** — `/assets` p99 ~19s; C3-1/C3-2 timeout fixes bound it, but confirm steady-state latency post-Phase-0. `sla-probe` unit exits 1 on breach (noisy).

## 5. Storage / runway (corrected from the earlier snapshot-based assessment)
- **Reality:** ZFS **raidz1** (single parity, NOT raidz2), 4×7.68TB, pool ~90% (~1.6T usable free after the snapshot reclaim). Usable ≈ 66% of raw (parity + 4K padding). Pool expansion **DID complete** (2026-05-21). TSDB compression **IS running** (job 1034, 45% chunks).
- **What's eating it (usable):** ClickHouse 7.5T (lake — no TTL, grows forever — *correct for the ambition*), MinIO 5.56T (galexie raw LCM = re-derive source), pgBackRest 2.49T (backups **on the same pool** — DR anti-pattern), Postgres 1.21T.
- **Done now (Phase-0-safe):** [x] reclaimed ~583G stale migrate snapshots; [ ] add pool alert.
- **After Phase 0:** merges compact (~+0.5–1T); **pgBackRest off-box** (~2.4T + fixes the DR anti-pattern); more TSDB compression.
- **Structural (for the ambition):** the lake grows forever by design → needs **storage tiering** (hot recent NVMe / cold history object-store, still queryable) + eventually horizontal scale. raidz1-at-capacity is thin redundancy for a canonical source. `operation_results` (2.1T) is needed for a comprehensive explorer — keep it (earlier "trim it" advice retracted).

## 6. Phases / backfills
- **Phase 0** — ch-backfill re-derive of the CH lake `[38.1M→62M]`, ~3–4 days left (~07-20/21). **Validated healthy:** topic-complete, idempotent (RMT), not starving live (tip advancing), merges keeping pace. **Must be the LAST full re-derive** — the INV-3 fix makes future corrections incremental; if Phase 0 recurs, the treadmill isn't truly broken.
- **Historical `backfill` cursors** (early ledgers, advancing) = the other PG fills, running concurrently — quantify their remaining size for the real runway number.
- **Post-Phase-0 sequence (heavy, schedule deliberately):** deploy → migrations → corrected re-derives (another multi-day-ish job) → prove.

## 7. Road to top-tier (the ambition — beyond bug-fixing)
1. **Infrastructure / HA / scale (the #1 foundation gate):** single box → multi-region (R2/R3 real, not on-paper); storage tiering for the forever-lake; **separate the warehouse workload from serving** so re-derives never degrade the explorer (they do today).
2. **Explorer UI depth:** the data is ahead of the UI — build out rich tx / contract-invocation / asset / holder / protocol pages to StellarExpert/Horizon parity. Likely the largest product gap; the un-audited frontend surface (§3) is where it lives.
3. **Point-lookup performance:** ClickHouse is analytical; an explorer is lookup-heavy ("show me tx X"). May need a lookup-optimized serving path over the lake (the 909G tx_hash_index is a start).
4. **Generic Soroban contract decoding:** decode *any* contract's invocations/args/events, not just the hard-coded protocols — this is what separates "a few protocols" from "the Soroban explorer."

## What's verified GOOD (don't re-litigate)
- Pricing pipeline (VWAP + outlier rejection + oracle divergence): accurate live, sound methodology.
- Re-derivability + certified-completeness foundation: correct for the ambition.
- Ops maturity: monitoring, backups, restore-drill, non-root, heavy-job flock, drift-detector-by-design.
- Frontend has good bones (correct patterns exist; the bugs are drift, not concept).
