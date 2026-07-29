---
title: Supply reconciliation vs Horizon — 8/8 PASS (post SAC re-seed)
captured: 2026-07-30T23:33Z (UTC 2026-07-29 23:33Z)
command: bash scripts/ops/reconcile-supply-vs-horizon.sh (tolerance 1.0%)
verdict: ✅ 8/8 within tolerance — the supply go-live gate's external-truth check
---

# Supply vs Horizon — 8/8 PASS

Captured minutes after the v0.21.4 TTL-gated SAC re-seed completed
(`supply seed-sac-balances -full-history`: 47,093 rows across 38/38
wrappers, USDC's 46,035 dormant holders restored — the class the six
failed scan-era attempts could not seed).

```
ASSET      TRUSTLINES      CLAIMABLE          POOLS      CONTRACTS    HORIZON_TOTAL             OURS DELTA_PCT  VERDICT
AQUA   80641086579.45 13816797779.12   518921092.83  4946757995.39   99923563446.78  100079999451.75  +0.1566%  PASS
BLND      36066200.05      388817.85       77544.69    75771803.83     112304366.42     112328919.21  +0.0219%  PASS
EURC       1346818.12          91.66      101128.76     1178865.75       2626904.29       2629094.39  +0.0834%  PASS
KALE     201713771.73     1091196.04    53007573.78    48190805.79     304003347.34     305608035.54  +0.5279%  PASS
PHO       76455959.77           3.92        6961.65     1388990.54      77851915.89      77851794.89  -0.0002%  PASS
USDC     343010470.59       21936.30     2467687.42    38096430.61     383596524.92     384128484.51  +0.1387%  PASS
VELO   23948716618.98      158526.00    35366069.04    15512828.92   23999754042.94   24000379415.67  +0.0026%  PASS
yXLM     146109777.16       45023.25     3288142.67     5467597.95     154910541.04     155056641.78  +0.0943%  PASS

assets=8 outside_tolerance=0
```

Notes:

- A run 5 minutes earlier showed USDC +1.0695% FAIL — that was the
  supply refresher's snapshot lagging the just-finished seed, not data:
  a direct per-component SQL readback at the same moment already summed
  to 384.13M (the passing figure). Lesson: wait one refresher cycle
  after a seed before reading the API's aggregate.
- Residual +0.14% on USDC decomposes as claimable +35k and SAC +499k
  vs Horizon — consistent with live-observer rows for since-archived
  holders (we do not ingest eviction events; the [DECIDE] eviction row
  in the launch plan). Bounded, explained, inside tolerance.

**What this does NOT prove**: per-holder correctness (see the 50-account
reconcile-balances artifact for that); the CONTRACTS column tracks
Horizon's own SAC accounting, which shares our blind spot for balances
held in non-SAC custom-token contracts.
