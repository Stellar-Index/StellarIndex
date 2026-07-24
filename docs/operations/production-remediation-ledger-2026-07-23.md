---
title: Production remediation ledger — fix everything, verified
started: 2026-07-23
status: OPEN
rule: a finding is CLOSED only when the fix is LANDED on main AND independently verified
      (proven-red-then-green test for money/data; live re-check for ops). "Designed" and
      "decided" do NOT count — see B11-F1/N-F1 (the $0.01 OHLC floor sat "decided, not
      implemented" for weeks). No self-attestation; every close cites its evidence.
---

# Remediation ledger

Companion to `production-confidence-campaign-2026-07-23.md` (the findings) and
`production-readiness-master-plan-2026-07-18.md` (the substrate work). This doc is the
**fix tracker** — the proof that everything found gets really fixed.

## Verification discipline (the "really" in "fixed everything really")

Per finding, CLOSED requires ALL of:
1. **Reproduce** the finding first (confirm it still bites at HEAD).
2. **Fix** it (best-practice, not smallest patch).
3. **Regression guard** — a test that FAILS without the fix, PASSES with it (money/data),
   or a documented live re-check (ops).
4. **Land** on main (commit SHA recorded) + deployed where relevant (verified live).
5. **Independent verify** — a second pass (fix-verifier / live oracle) confirms the
   finding is gone AND nothing regressed.

## Fix backlog (every campaign finding → fix → verification → status)

### 🔴 Blockers

| ID | Finding | Fix approach | Verification | Status |
|---|---|---|---|---|
| **C-F1** | `/accounts/{g}/operations`, `/transactions`, `/contracts/{id}/code-history` 500 for all inputs (8s scan-by-non-PK) | account-ordered access: per-account table (like `account_movements`) or extend `operation_participants` to include the op's own source so both arms point-lookup; capacity-aware | both endpoints 200 <SLO for a whale AND a small account; `sla_probe` clears | OPEN |
| **F4-F1** | ClickHouse 8.7 TiB lake has no backup | install `clickhouse-backup` + timer + off-site (S3) target; document RTO | a real backup exists; **test-restore one table** into scratch and diff | OPEN |
| **B11-F1 / N-F1** | OHLC wicks (0.20 high, 0.1333 low) — $0.01 notional floor designed, never shipped | implement `max(price) FILTER (WHERE usd_volume>=0.01)` + COALESCE in the 7 caggs; remove `combinedOutlierBandRatio` | Jul-17 06:00 low no longer 0.1333, Jul-15/16 highs no longer 0.20; known-good bars byte-unchanged | **IN-PROGRESS** |

**B11-F1 progress (2026-07-23):**
- ✅ Root-caused: fix site = `migrations/0002` OHLC caggs' `max/min(quote_amount/base_amount)`, unfiltered (7 caggs: prices_1m…1mo). Serve-layer 2× band (`ohlc_fiat_combine.go`) is a symptom-patch to remove.
- ✅ **De-risked the design's blocking open question #3: TimescaleDB (2.26.4) DOES accept
  `COALESCE(max(v) FILTER (WHERE uv>=0.01), max(v))` in a continuous aggregate** (scratch
  cagg created cleanly). Fix goes directly in the cagg definitions.
- ✅ Sized the deployment: the 7 caggs total **~1.1 TB** (prices_1m 733 GB, 15m 191, 1h
  99, 4h 50, 1d 19, 1w 5.4, 1mo 2). Full re-materialization from 535M trades is
  hours-long — **must NOT contend with the running D2 backfill** (the design's "schedule
  off the D2 window" constraint; running a 1.1 TB re-mat mid-D2 risks both jobs on the
  single box).
- ✅ **BOUNDED PROOF PASSED (2026-07-23, non-invasive, real data):** the served Jul-17
  06:00 low (0.1333) is the inverse of ONE dust trade — `USDC-GA5Z…/native @ 7.5`,
  usd_volume **$0.00000027** (40,000× below the $0.01 floor), a 2↔15-stroop SDEX fill.
  With the filter the USDC/native bucket high goes **7.500000 → 5.487531** (251/321
  trades kept), so the **served low corrects 0.1333 → 0.1822** — matching the real CEX
  low that hour (0.1822) to the digit. Fix confirmed correct end-to-end.
- **NEXT (deployment only):** author migration 0115 (drop+recreate 7 caggs with the
  FILTER, faithful to current defs + policies) + remove the serve-layer band; land the
  code; **execute the 1.1 TB re-mat the moment D2 completes (early Fri)**, then re-run
  the live OHLC endpoint to confirm 0.1333→0.1822 in the SERVED output + known-good
  bars byte-unchanged. CLOSE only after that live post-deploy verify.
| **N-F2** | completeness `retentionStart=tip−1.5M` → `complete:true` only certifies ~100 days | set `retentionStart` = actual-min-served (DECISION item 3) | a synthetic sub-min-served loss flips `complete`→false; full-history reconcile runs | OPEN |

### 🟠 Mediums

| ID | Finding | Fix approach | Status |
|---|---|---|---|
| C-F2 | `/lending/pools/{pool}/reserves` 15s, no cap | add handler timeout + optimize query | OPEN |
| C-F3 | cold detail scans ~8s (account-scan class) | same as C-F1 / `account_wealth_snapshot` rollup | OPEN |
| B4-F1 | SEP-41 supply/transfers `complete:false` (C2-11 >4-topic truncation) | re-ingest topic-complete lake; fix truncation | OPEN |
| E4 / N-F3 | supply cross-check firing P3 (classic-vs-SAC on PHO/KALE/BLND); SACWrapped follow-up unbuilt | build the classic-vs-SAC reconciliation or justify the 1-stroop tolerance | OPEN |
| F4-F3 | backups local-only (no off-site) | S3/off-site repo for pgbackrest + CH | OPEN |
| F3-F1 | restore never drilled | execute a restore drill into scratch (never touch prod) | OPEN |
| N-F4 | rate-limiting on paid tiers not built | implement `RateLimitPerMin` | OPEN |
| N-F5 | ADR-0027 LCM cold-tier unshipped (~3–4 TB reclaim) | flip flag + first trim + monthly timer | OPEN |
| N-F6 | anomaly 30-min-extension (`_sustained`) state machine not built | implement the state machine | OPEN |
| B12 | junk/scam-asset SDEX trades pollute non-volume-weighted native-XLM metrics | notional floor everywhere max/min/last is taken; `CODE-ISSUER` keys never `CODE-%` | OPEN |

### 🟡 Lows

| ID | Finding | Fix | Status |
|---|---|---|---|
| B10-F1 | ~0.16% price spread across endpoints (freshness) | document window semantics or reconcile | OPEN |
| F4-F2 | PG backups unencrypted (`cipher: none`) | enable pgbackrest cipher | OPEN |
| H3 | confirm intended CORS policy | decide + document/enforce | OPEN |
| L4-F1 | `/coverage` ~8.5h stale | (downgraded — it's cadence; confirm acceptable) | LIKELY-OK |

### ✅ Already resolved during the campaign (chased to ground, benign)
- B10-F2 (native==crypto:XLM==XLM one price), L1 (honest CS-087 flag), the -610B
  trustline "corruption" (Int64 overflow on same-code scam tokens; real supply exact).

## Still-pending discovery (may add findings before the ledger is complete)
Explorer (D), config/schema drift (K), injection/error-leakage (H1/H4), load (C5),
SEP-41 deep-dive (B4), precision fixtures (B8), census/soroban (A6/A7), the Phase-E
gate (E1–E3, backfill-gated), and **the multi-agent cold code audit (I)** — the last
of which is the real completeness check on "did we find everything." **The ledger is
not provably complete until discovery + the audit are done.**

---

## Audit code-findings (chunk 1 — money surface, 2026-07-23)

The multi-agent cold code audit found what live-probing structurally couldn't. Chunk 1 = 55 skeptic-CONFIRMED (1 crit, 7 high, 40 med, 6 low). Full list: `docs/audit/audit-2026-07-23/findings.md`. Chunk 1 hit the session limit mid-verify (not converged — resume pending); chunks 2–6 (ingest/api/completeness/web/plan) not yet run.

| ID | Sev | Finding | Fix | Status |
|---|---|---|---|---|
| **A-CRIT-1** (DAT-15) | CRIT | `projected-rebuild -write` re-derives DEX trades with `derive_generation=now()` but never `InstallUSDVolumeResolution` → overwrites correct `usd_volume` with NULL, unrecoverable (gen guard blocks live gen-0 restore) | add `InstallUSDVolumeResolution` after `SetDeriveGeneration` in `projected_rebuild.go:137`; best: fail-closed if resolvers absent when gen>0 | OPEN |
| A-H-1 (MNY-06 ×3) | HIGH | Served VWAP combines SDEX both-directions by **trade-count not volume**; `/v1/history`,`/v1/chart` drop reverse orientation → wrong served price | volume-weight the union across directions; read both orientations | OPEN |
| A-H-2 (COR-14) | HIGH | Confidence = normalized geometric mean, but ADR-0019 + freeze threshold need un-normalized product → wrong freeze decisions | align to ADR-0019 (un-normalized) or fix the threshold | OPEN |
| A-H-3 (MNY-22 ×2) | HIGH | USD-vol gate ignores USDT/USDC/DAI/PYUSD legs; baseline slow-drift defense self-defeats (z-scores its own drifting baseline) | count stablecoin legs in the gate; anchor drift detection to an external ref | OPEN |
| A-H-4 (MNY-04 ×2) | HIGH | Freshness gate accepts a stalled observer's frozen supply re-stamped at current ledger; /coverage has no live-tip gate | require component-observer freshness ≤ threshold; add live-tip gate | OPEN |
| **A5-REVISED** (DAT-10 ×12) | HIGH | **CONTRADICTS my A5 "benign" call:** 7+ served reads (`RecentOperations`,`AccountOperations/Transactions`,`StreamEntryChanges`,`BuildTxHashIndex`,`backfillClassicOpsWindow`…) read RMT tables WITHOUT FINAL → over-count unmerged-part duplicates | add FINAL/LIMIT-1-BY/argMax to each; chokepoint a deduped reader | OPEN |
| A-M-8 (MNY-08) | MED | XLM circulating can go **negative** — Alg-1 lacks the zero-clamp classic+SEP-41 have | clamp circulating ≥ 0 | OPEN |
| A-M-COR14b (COR-14) | MED | Non-USD fiat asset-detail serves `price_usd=null` AND `market_cap_usd=null` (detail path never got the fx-cross fix) | wire the fx-cross into the detail path | OPEN |
| A-M-5 (MNY-05) | MED | On-chain DEX confidence USD-volume uses **1e8 instead of 1e7** (decimals bug) | use 1e7 for on-chain 7-dp | OPEN |
| A-M-SEC08 (SEC-08) | MED | `isSafeImageURL` scheme-only, docstring falsely guarantees XSS-safety for issuer SEP-1 fields | validate/deny, fix docstring | OPEN |
| **B11-F1 EXPANDED** | — | audit confirms + expands: OHLC **open/close also unfiltered**, non-fiat `?interval=` serves raw CAGG high/low, stablecoin fallback no depeg bound | extend the `$0.01` notional floor to open/close + non-fiat path + depeg bound | folds into B11-F1 |
| **N-F2 CONFIRMED** (MNY-02) | — | independent confirm: /coverage `complete` certifies only ~100 days | (= N-F2) | folds into N-F2 |

## Audit code-findings (chunk 2 — ingest/sources, 2026-07-24)

64 skeptic-CONFIRMED (13 high, 39 med, 11 low). Full list appended to `docs/audit/audit-2026-07-23/findings.md`. Partial (session limit mid-verify). **A-CRIT-1 DOUBLE-CONFIRMED here (MNY-03) — cross-chunk double-find.** Key highs:

| ID | Sev | Finding | Fix |
|---|---|---|---|
| A2-H-drainloss (RFC-2/CON-10) | HIGH | sorobanevents batch drops rows on insert-fail (no dead-letter, cursor advances past); sink drain 90s > shutdown 30s → buffered trades silently lost on SIGTERM | dead-letter/retry on insert-fail; align drain budget ≤ shutdown deadline |
| A2-H-infradrop (REL-08 ×3) | HIGH | on-chain trades + external-retry-buffer + non-trade events DROPPED (not block-retried) on Postgres INFRA fault (disk-full/OOM) | classify infra vs data fault; block-retry infra faults |
| A2-H-wedge (COR-11 ×2, COR-01) | HIGH | deterministic Validate-fail (oracle/transfer) misclassified transient → permanently wedges the sole-writer projector; negative SEP-41 amount same | classify deterministic errors as data-fault; skip+count, don't wedge |
| A2-H-supplydrift (DAT-10) | HIGH | claimable_balances + liquidity_pools observers never emit removals → served total/circulating supply over-counts | emit removal events |
| A2-H-gapblind (DAT-09 ×2) | HIGH | TolerateTrailingMissing masks mid-range archive holes; blend_backstop genesis 5.1M ledgers too late → gap-detect blinded | fix tolerance to trailing-only; correct genesis ledgers |
| A2-H-census (INT-01) | HIGH | census counts SDEX trades the decoder drops → count vs served mismatch | align census to decoder's drop rules |

## Audit code-findings (chunk 3 — api/auth/security, 2026-07-24)

110 CONFIRMED (16 high, 67 med, 25 low) — full run, no limit. Full list in findings.md. Key highs:

| ID | Sev | Finding | Fix |
|---|---|---|---|
| A3-H-rlfailopen (REL-06) | HIGH | rate-limiter fails OPEN under flapping Redis (1 success/<30s resets dwell clock) → defeats rate-limit + auth brute-force + signup throttle (runtime-verified) | trip fail-closed on consecutive-failure count / error-rate, not last-success timestamp; circuit breaker shared across limiters |
| A3-H-unauthDoS (PRF-03 ×3) | HIGH | unauth /holders (2 FINAL scans 43.6M rows), AccountState (2 account_id FINAL reads), /contracts (365d GROUP BY billions) exhaust 8-conn pool → whole-API DoS | cache + bound + auth/ratelimit these; account-ordered access (ties C-F1) |
| A3-H-ssecrash (AGT-12 ×2, REL-05 ×2) | HIGH | SSE producer goroutines no panic-recovery → one panic crashes process; Hub topic map unbounded; caps enforced after attacker-keyed topic created | recover() per producer; bound topic map; enforce caps before topic create |
| A3-H-sep10dead (AGT-11) | HIGH | SEP-10 auth dead-on-arrival: two-phase validator build errors on nil-rdb first pass | fix the nil-rdb build path |
| A3-H-emaildrop (NTF-11, NTF-13) | HIGH | missing Resend key silently drops magic-link email while reporting 'sent'; retry budget 8h not documented 72h | fail-closed on missing key; fix retry budget |
| A3-H-movetail (API-05) | HIGH | /accounts/{g}/movements ?asset= silently skips post-P23 Postgres-tail rows after pagination | paginate across both stores |
| A3-H-absence (negative-space) | HIGH | no kill-switch to suspend/close an account; no billing-downgrade rate-limit enforcement; no per-account active-key quota; Stripe 'paid-but-no-keys' no dead-letter | build the kill-switch, downgrade enforcement, key quota, dead-letter |

**229 confirmed across chunks 1–3 (1 crit, 36 high). Chunks 4–6 pending.**
