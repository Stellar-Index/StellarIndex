---
title: v1 go-live evidence pack — artifact index
last_verified: 2026-07-28
status: living — one file per filed artifact, this README is the index
---

# v1 go-live evidence pack

Artifacts backing the §1 go-live gates of
[`../v1-launch-plan.md`](../v1-launch-plan.md) §2.6. Every artifact states
its capture command, date, verdict, and **what it does NOT prove** — a
filed artifact with an honest scope beats a remembered green check
(three prior generations of launch plans called for these files; none
were ever produced until now).

| Gate (§1) | Artifact | Verdict | Filed |
|---|---|---|---|
| Launch mechanics: route availability | [pre-deploy](2026-07-28-route-sweep-pre-deploy.txt) → [post-serving-flip](2026-07-29-route-sweep-post-serving-flip.txt) | 🔵 21×5xx → **10×5xx** (serving-auth class fixed; residual = slow-read timeout class, §2.4 40× read-amplification investigation) | 2026-07-29 |
| Capacity: cold-tier trim safety | [2026-07-28-soak-gate.md](2026-07-28-soak-gate.md) | ✅ PASSED + gate executed | 2026-07-28 |
| Supply trustworthy: CS-102 regression guard | [2026-07-28-cs102-regression-redgreen.md](2026-07-28-cs102-regression-redgreen.md) | ✅ red/green proven | 2026-07-28 |
| Supply trustworthy: vs external truth | [baseline](2026-07-28-supply-reconcile-horizon.md) → [post-fixes](2026-07-29-supply-reconcile-post-fixes.md) → [**post-re-seed**](2026-07-30-supply-reconcile-8of8.md) | ✅ **8/8 PASS** (USDC +0.14% after the TTL-gated full-history re-seed restored its 46,035 dormant holders) | 2026-07-30 |
| reconcile-balances (50-account) | [2026-07-29-reconcile-balances-50.md](2026-07-29-reconcile-balances-50.md) | ✅ **0 mismatches** (was 38% pre-ordinal) — E1 met | 2026-07-29 |
| Completeness green | tracked in plan loop-log (2026-07-29) | 🔵 sep41×2 `projection_ok=t` (hole closed); `complete=t` pending the timer's substrate pass; **redstone stays incomplete** — 866 undecodable events, code-side fix next release | — |
| verify-lake / contiguity / hash-chain | [2026-07-29-lake-verifiers.md](2026-07-29-lake-verifiers.md) | ✅ ALL PASSED — 0 broken hash links genesis→tip [2, 63,699,907] | 2026-07-29 |
| Prices vs independent references | [2026-07-29-price-divergence.md](2026-07-29-price-divergence.md) | ✅ 22/22 clear across 5 references (worst −0.55%); top-50 broadening needs the CG Pro key [OP] | 2026-07-29 |
| re-derive determinism | [2026-07-29-rederive-determinism.md](2026-07-29-rederive-determinism.md) | ✅ byte-identical (phoenix 50k-ledger window, 239 rows, same md5) | 2026-07-29 |
| SEV-1/2 paging drill + rollback rehearsal | — not yet filed | ⏳ blocked on paging being wired (Ash item #1) | — |
| verify-usd-volume calibration | [2026-07-30-verify-usd-volume-30d.md](2026-07-30-verify-usd-volume-30d.md) | ✅ **FULLY CLEAN** — pipeline exact + historical era re-stamped (13.3M rows, 66/66 dirty days → 0 violations); estimated-tier spread measured → keep coverage alerts, no sum threshold | 2026-07-30 |

Gaps are listed deliberately — an index that only shows what exists
reads as "done" when it is not.

## RFP acceptance-criteria mapping (2026-07-29 re-verification)

The original deliverable claim (2026-06-13) predates the July campaign;
this maps the verbatim ACs (docs/archive/deliverable-readiness-plan.md
§0) to CURRENT evidence:

| AC | Criterion | Status 2026-07-29 |
|---|---|---|
| AC1 | staleness ≤30s | ✅ `/price/tip` via sla-probe (continuous); freshness definition published |
| AC2 | p95 ≤200ms / p99 ≤500ms | ✅ k6 evidence (2026-06-13 origin-direct PASS) + weekly k6 cron restored 2026-07-06; re-run at launch load recommended post-snapshot-reader |
| AC3 | history ≥1yr (ideally inception) | ✅✅ daily OHLC to 2015 AND the lake now hash-chain-verified to genesis ([lake verifiers](2026-07-29-lake-verifiers.md)) |
| AC4 | ≥1000 req/min | ✅ k6 evidence + rate-limit tier configured |
| AC5 | code public + reproducible | ✅✅ public repo (2026-07-03) under the `Stellar-Index` org (2026-07-15 — the previously-pending org step is DONE); **fresh-clone reproducibility CAPTURED 2026-07-29**: `git clone https://github.com/Stellar-Index/StellarIndex` → `pnpm install` (web dirs) → `bash scripts/dev/verify.sh` → exit 0, "ALL CHECKS PASSED" — on a machine with only the documented toolchain |
| AC6 | production ~10 weeks | ✅ r1 live + serving for months; single-box posture per the HA decision (§4) |
| AC7 | API docs + self-service onboarding | ✅ docs.stellarindex.io live (HTTP 200 verified); signup/API-key flow shipped (rc.110/115 era) — one E2E onboarding walkthrough recommended at launch |
| SEV | SEV-1 ≤15/≤30, SEV-2 ≤30/≤60 | ⏳ runbooks + playbook exist; the TIMED drill is gated on paging being wired (operator item #1) |
