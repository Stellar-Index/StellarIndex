---
title: Evidence — classic supply vs Horizon AFTER CS-102 deploy + phantom-row delete
last_verified: 2026-07-29
status: acceptance — 7/8 PASS, 8th dispositioned (re-seed pending)
---

# Classic supply vs Horizon — 2026-07-29 ~05:45Z (post-v0.21.2, post-delete)

`scripts/ops/reconcile-supply-vs-horizon.sh`, ±1% tolerance. Baseline =
[2026-07-28 baseline](2026-07-28-supply-reconcile-horizon.md) (3 FAILs).

| asset | delta before | delta now | verdict |
|---|---|---|---|
| PHO | **+156.90%** | **−0.0002%** | PASS — the TTL-gated delete of archived seeded rows WAS the entire error |
| EURC | −1.20% | +0.09% | PASS — CS-102 unfreeze, as predicted |
| KALE | +1.29% | +0.53% | PASS — CS-102 unfreeze, as predicted |
| AQUA | +0.18% | +0.16% | PASS — integrity survived the seeded-row delete |
| VELO | +0.00% | +0.00% | PASS |
| BLND | −0.48% | +0.02% | PASS |
| yXLM | +0.09% | +0.09% | PASS |
| USDC | +0.04% | **+1.12% FAIL** | dispositioned: the approved delete removed USDC's 48,505 seeded dormant holders (the provenance table's largest cohort); the TTL-gated re-seed is parked on a CH read-amplification blocker (below) and restores this on landing |

**Re-seed blocker (parked, diagnosed):** five attempts failed on a
chain of three independent causes — (1) 5,000-key IN-lists exceed the
256 KiB `max_query_size` parse cap (fixed live+ansible: 4 MiB), (2/3)
the TTL classifier's scan OOMs the **client-pinned** 10 GiB
`openRead` cap (gate.go G12-04 — server-side profile raises cannot
override client settings). Root measurement: the identical probe costs
**122 MiB against `ledger_entries_current_old` but 4.76 GiB against the
post-D3 table** — a ~40× read amplification in the cutover table
(same ORDER BY, both fully merged; cause not yet identified — ALSO the
prime suspect for the residual 8s explorer route timeouts). Fix path:
next release shrinks classifier batches + sets per-query
spill/memory settings; investigate the table regression first.

**What this does NOT prove:** SEP-41/Soroban-native supply (separate
path — its projector is at tip and its completeness runs are in
flight), XLM total supply, or the ~30k unwatched assets.
