---
title: "Top-tier explorer gap analysis + roadmap"
status: analysis
date: 2026-07-17
north_star: "The canonical, comprehensive Stellar explorer — where people go for any transaction, smart-contract invocation, asset, or protocol."
---

# Top-tier explorer — gap analysis & roadmap

Compiled 2026-07-17 from three ground-truth surveys of the current codebase (UI depth, point-lookup serving path, contract-decoding + infra/HA), read against the north star. This is the "what's left to be genuinely top-tier" map, beyond the audit bug-fixes.

## Where we stand — two tiers of maturity

The product has **two tiers, split by domain**:
- **Market / price / protocol / DeFi analytics** (assets, markets, protocols, dexes, lending, oracles, sources, exchanges, issuers) — **RICH, arguably best-in-class for Stellar.** This is the center of gravity and the north-star pillars **asset ✓** and **protocol ✓** are already met and then some.
- **Classic forensic explorer** (transaction, operation, ledger, account, contract-*invocation*) — **materially thinner, and much of the thinness is API-contract-level, not just UI.** The pillars **transaction** and **smart-contract invocation** are the frontier.

The data lake is ahead of both the UI and the serving path. The four gates below are, in priority order, where top-tier is won or lost.

---

## Gate 1 — Infrastructure / HA / scale (the #1 foundation gate)

**Current state (surveyed):** a **single Hetzner host (R1)** runs every stateful service on one ZFS pool — TimescaleDB, ClickHouse, Redis (single-node, no-auth, loopback), MinIO, galexie+captive-core, and one each of indexer/aggregator/api. **R2/R3 are design-only** (templates + skeleton state docs; ADR-0016 shapes are aspirational). The entire HA stack (Patroni, HAProxy, Redis-Sentinel) is **ratified design with roles written but wired into no active playbook** (`docs/architecture/ha-plan.md:7-12`). One pool loss = total loss.

**The sharpest sub-gap — warehouse/serving coupling:** the API serving reads, the live sink, AND the heavy ops backfills all hit the **same single ClickHouse** as the unauth `default` user. The dedicated `api_serving` CH profile (bounded threads/memory/timeout, ADR-0048 D4) is **codify-only, NOT applied to R1**. There is direct evidence of the failure mode: on 2026-07-09 a `contract_events_daily` merge "exceeded the kernel commit budget… starving queries + the live sink until the server was effectively wedged." **A re-derive can degrade the live explorer today.**

**Gaps, ranked:**
1. **Serving is not isolated from the warehouse.** Cheapest partial fix: actually apply the `api_serving` profile + move backfills off the `default` user. Proper fix: a separate serving instance / ReplicatedMergeTree read replica so re-derives never touch the query path.
2. **Single-box SPOF.** No Postgres replica (Patroni undeployed), no ClickHouse replication at all (no ReplicatedMergeTree/keeper), single-node Redis/MinIO. RPO/RTO are documented targets, not achieved.
3. **DR anti-pattern:** pgBackRest is the *only* live HA mechanism, but `repo1` sits on the same host pool as the DB (ADR-0043 CS-111); offsite `repo2` is gated off. ClickHouse has no backup (re-derive is the strategy, undrilled). Restore drills have never run.
4. **No storage tiering for the forever-lake.** The lake grows forever by design (correct), but ClickHouse hot/cold tiering is **absent** (prose in ADR-0034 only — no `storage_policy`, no `TTL … TO VOLUME`, no S3 disk). Raw-LCM object tiering (ADR-0027) is implemented but **dormant** (flag off, bulk trim never run).

**Actions (sequenced):** apply the serving profile + fence backfills off `default` (near-term, unblocks safe re-derives) → move pgBackRest `repo1` off-box + enable `repo2` → run the first restore drill → provision R2 (Postgres replica via the written Patroni role + a ClickHouse read replica) → CH cold-tiering (`storage_policy` + MinIO/S3 disk, cold partitions still queryable) → R3 + cross-region.

---

## Gate 2 — Explorer UI depth (where the transaction/contract promise is won)

**Current state (surveyed):** analytics pages are RICH; the **core forensic objects are thin, and mostly limited by the API contract, not the UI.** `TxDetail` = `TxSummary` + `operations[]` + `events[]` only; `Operation` has no effects/state-change fields; `ContractEvent.topics/data` are explicitly "lossy display format" and the tx/op views show only `topic_0`. There is **no contract-*invocation* detail page** and **no contract-state browser**. The contract *page* itself (WASM/exports/WAT/decompile/code-history/transfers/interactions) is excellent — exceeds StellarExpert. Accessibility is a real, above-average pass (skip-link, focus-trapped search dialog, landmarks, honest ARIA restraint).

**Gaps, ranked by north-star impact:**
1. **Transaction forensics are shallow (#1 UI gap).** Missing: signatures/signers, preconditions/timebounds, sequence, fee-bump/inner-tx, Soroban resource-fee metering, and critically **effects / balance changes / ledger-entry deltas** — a user cannot answer "what changed on-chain and who was affected." *Needs API surface first* (`TxDetail` is projection-only).
2. **No contract-invocation view + no contract state.** The north star literally names "a smart-contract invocation," yet there's no `InvokeHostFunction` page (arg tree, sub-invocation/auth call tree, return value, diagnostic events, state changes) and no storage/data-entry browser.
3. **Op `fields` rendered as raw `JSON.stringify`** across tx/operation/account views — a payment shows a stringified blob, not linked from/to accounts, an asset link, and a scaled amount. **Pure UI fix, data already decoded server-side — highest-leverage, lowest-cost, lifts every core object at once.**
4. **Soroban event decode is lossy + truncated** (topic_0 only; see finding C2-11 for the >4-topic truncation, now being fixed).
5. **No global recent-transaction feed** (`/transactions` is ledger-scoped only); operations/ledgers are single-direction cursor walks.
6. **Operations aren't stably addressable** (`/operation?tx=H&i=N` query params, no canonical op-ID URL — hurts deep-linking).
7. **Account auth-flags / data entries / sponsorship not surfaced** (flags fetched but never decoded — mostly UI-side).

**Actions (sequenced):** ship the two "data already exists" wins first — **rich op-field rendering (#3)** and **event topic decode (#4)** — then expand the API contract for tx effects/state-changes (#1) and the invocation view (#2), then the global feeds/addressability (#5/#6). This is likely the **largest product gap** and the single un-audited surface most worth deepening.

---

## Gate 3 — Point-lookup performance (analytical lake serving lookup-heavy traffic)

**Current state (surveyed):** ClickHouse is OLAP being asked to do lookup work, and the team **already has the right pattern** — dedicated sort-key-ordered lookup tables (`tx_hash_index` ORDER BY tx_hash; `account_movements` ORDER BY address; `operation_participants` ORDER BY account) turn CH into a competent µs KV *when the sort key IS the lookup key*. Ledger-by-seq and by-ledger reads are optimal. **A new external KV store is not yet justified.**

**Gaps, ranked:**
1. **The `tx_hash_index` historical backfill has not been run on R1** — until it does, every deep-linked pre-deploy transaction pays a ~5s full-scan (96.6M residual rows). **Zero code work; the single biggest live win** — just the windowed operator job. *(This is now a post-Phase-0 operator step.)*
2. **No read cache in front of the immutable CH entity endpoints.** `tx/{hash}`, `ledger/{seq}`, ops/events-by-tx are immutable once a ledger closes — cache-forever — yet nothing caches them (Redis was wired only onto the Postgres/Timescale catalogue path). **Cheapest high-leverage lever, and it directly relieves the C3-1 pool pressure** (cached responses never touch the 8-conn pool).
3. **Account-state lookup is the worst remaining class** — bloom (prunes, can't seek) + FINAL (read-time merge) + unpartitioned, 3 scans/request. Fix: a sibling current-state table whose ORDER BY leads with `account_id` (mirror the tx_hash_index precedent) so it becomes a PK range scan and FINAL can be dropped.
4. **C3-1's 8s per-handler timeout is only PARTIALLY wired** — applied in `account_state.go` + `contracts_list.go` only; `tx.go`, `ledgers.go`, `accounts.go`, `movements.go`, `operations.go`, `positions.go` still pass raw `r.Context()` (rely on the 15s blanket middleware). **This is a remediation gap worth closing now.**

**Actions (sequenced):** run the tx_hash_index backfill (post-Phase-0) → add Redis/edge caching on immutable entity endpoints → finish wiring the 8s timeout across all explorer handlers → build the account_id-ordered current-state sibling. All tune-CH-plus-cache; no second datastore.

---

## Gate 4 — Generic Soroban contract decoding (the "any contract" promise)

**Current state (surveyed):** decoding is **100% hard-coded per protocol** — an ordered first-match decoder chain gated on contract identity, with a hand-written registry (`internal/pipeline/dispatcher.go:90-227`). There is **no SEP-48 contract-spec path** (grep for `ScSpecEntry`/`contractspecv0`/`sep-48` = empty). For an **unknown** contract: events are captured **raw (nothing lost)** into the `soroban_events` landing zone + CH lake, and rendered via a **lossy, type-only** `scval.Display` (no field names, no typed reconstruction). **Event-less unknown calls (the Band shape) are structurally invisible** beyond raw XDR. Coverage gaps are documented (e.g. soroswap-router at an 8729× decoder gap).

**Gap to close:** (1) SEP-48 contract-spec **fetch + cache** — nothing exists; since RPC is banned (ADR-0001), it must parse the `contractspecv0` WASM custom section from the uploaded WASM already in the lake, cached per wasm-hash; (2) a **spec-driven ScVal → typed/named JSON** renderer (today's `scval.Display` is type-only — can't label `amount: i128` vs `to: Address`); (3) a **per-revision ABI registry** keyed by wasm-hash to survive `update_contract` drift; (4) a **state/poll completeness model** distinct from ADR-0033's event-coverage model.

**Status:** architecturally **deferred** (ADR-0045, reaffirmed 2026-07-10) because no sustained un-ingested target was found. **Recommendation:** this is the true differentiator between "a few protocols" and "the Soroban explorer," but it's the deepest bet. The pragmatic on-ramp is generic *display* (already have `scval.Display`) + generic *detection* (already have the discovery sniffers) + **spec-driven typed rendering fed by the WASM custom section** — deliver typed/named args+events for any contract whose WASM carries a spec, before attempting a universal completeness model.

---

## Recommended sequencing (the short version)

**Now / near-term (mostly exists, high leverage):**
- Apply the CH `api_serving` profile + fence backfills off `default` (Gate 1 — stops re-derives degrading the API).
- Rich op-field rendering + event topic decode in the UI (Gate 2 #3/#4 — data already there).
- Redis/edge cache on immutable entity endpoints + finish the 8s-timeout wiring (Gate 3 #2/#4).

**Post-Phase-0 operator steps:**
- Run the `tx_hash_index` historical backfill (Gate 3 #1 — biggest live lookup win).
- pgBackRest off-box + `repo2` on + first restore drill (Gate 1 #3).

**Structural bets (sequenced, larger):**
- R2 (Postgres replica + ClickHouse read replica) → serving/warehouse fully separated → R3 + cross-region (Gate 1).
- Expand the API contract for tx effects/state-changes + the contract-invocation view (Gate 2 #1/#2).
- CH cold-tiering for the forever-lake (Gate 1 #4).
- SEP-48 spec-driven generic contract decoding (Gate 4) — the deepest differentiator.

## Cross-cutting note
The codebase is unusually honest about design-vs-reality (audit banners, "codify-only" markers, skeleton state docs). Ground truth for every HA/tiering/multi-region claim is **single-box R1 with the stack scaffolded but not deployed** — the roadmap above is the ordered path from that reality to top-tier.
