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
| Launch mechanics: route availability | [2026-07-28-route-sweep-pre-deploy.txt](2026-07-28-route-sweep-pre-deploy.txt) | ⚠️ **BASELINE, failing** — 21 server-5xx (known explorer outage, INBOX #1); acceptance = re-run post-deploy showing 0 | 2026-07-28 |
| Capacity: cold-tier trim safety | [2026-07-28-soak-gate.md](2026-07-28-soak-gate.md) | ✅ PASSED + gate executed | 2026-07-28 |
| Supply trustworthy: CS-102 regression guard | [2026-07-28-cs102-regression-redgreen.md](2026-07-28-cs102-regression-redgreen.md) | ✅ red/green proven | 2026-07-28 |
| Supply trustworthy: vs external truth | [2026-07-28-supply-reconcile-horizon.md](2026-07-28-supply-reconcile-horizon.md) | ⚠️ 5/8 PASS — the 3 FAILs are dispositioned (2× CS-102 pending deploy, 1× SAC-seed pending INBOX #5) | 2026-07-28 |
| Completeness green | — not yet filed | ⏳ 3 sources incomplete until v0.21.2 + replays | — |
| reconcile-balances (50-account) | — not yet filed | ⏳ deferred until D3 releases the ClickHouse slot (partial 8-account signal: 0 mismatches) | — |
| verify-lake / contiguity / hash-chain | — not yet filed | ⏳ queued behind D3 | — |
| Prices vs CoinGecko/Chainlink top-50 | — not yet filed | ⏳ campaign B1 | — |
| re-derive determinism | — not yet filed | ⏳ campaign E3 | — |
| SEV-1/2 paging drill + rollback rehearsal | — not yet filed | ⏳ blocked on paging being wired (INBOX #3) | — |
| verify-usd-volume calibration | — not yet filed | ⏳ first run post-deploy | — |

Gaps are listed deliberately — an index that only shows what exists
reads as "done" when it is not.
