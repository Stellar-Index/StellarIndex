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
