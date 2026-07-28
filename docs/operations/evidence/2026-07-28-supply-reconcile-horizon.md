---
title: Evidence — classic supply vs Horizon reconciliation (pre-v0.21.2 baseline)
last_verified: 2026-07-28
status: baseline — re-run post-v0.21.2 deploy is the acceptance test
---

# Classic supply vs Horizon — 2026-07-28 07:23Z baseline

**Command:** `scripts/ops/reconcile-supply-vs-horizon.sh` — all 8 classic
watched assets against Horizon's full component sum (trustlines +
claimable + liquidity pools + SAC). Tolerance ±1%.

| asset | Horizon | ours | delta | verdict |
|---|---|---|---|---|
| AQUA | 99,923,674,166 | 100,104,871,790 | +0.18% | PASS ✅ |
| VELO | 23,999,754,042 | 24,000,220,889 | +0.00% | PASS |
| USDC | 351,793,900 | 351,945,989 | +0.04% | PASS |
| yXLM | 154,910,541 | 155,056,806 | +0.09% | PASS |
| BLND | 112,304,366 | 111,766,751 | −0.48% | PASS |
| EURC | 2,632,900 | 2,601,251 | −1.20% | FAIL |
| KALE | 303,494,623 | 307,406,857 | +1.29% | FAIL |
| PHO | 77,851,915 | 199,999,995 | +156.90% | FAIL |

**Headline:** the AQUA claimable fix is landed and evidenced
(−13.2% → +0.18%; the claimable component now reads 13.74B).

**The 3 FAILs are dispositioned, not open mysteries:**
- **EURC + KALE** — CS-102 freeze drift (both frozen assets; delta is
  accrued staleness, not a computation defect). Acceptance: re-run after
  the v0.21.2 deploy; both must return inside tolerance.
- **PHO** — SAC seed wrote ARCHIVED (TTL-lapsed) entries as live; our
  live observer is correct to 0.009%; the entire error is 39 seeded rows
  (122.1M units). Fix = TTL-gated re-seed + DELETE of archived seeded
  rows, gated on OPERATOR INBOX #5. PHO stays +157% until then — a
  post-deploy re-run showing PHO still failing is EXPECTED, not a failed
  deploy.

**What this does NOT prove:** anything about the ~30,745 other assets
the claimable seed touched, SEP-41/Soroban-native supply (40 assets —
frozen pending v0.21.2 + sep41 tail rebuild), or XLM total supply.
