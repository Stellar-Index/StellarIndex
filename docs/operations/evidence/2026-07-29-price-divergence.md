---
title: Evidence — served prices vs independent references (live divergence surface)
last_verified: 2026-07-29
status: filed — all clear; top-50 broadening remains a follow-up
---

# Price divergence vs independent references — 2026-07-29 ~09:40Z

**Source:** `/v1/divergence` (the continuously-computed cross-reference
surface; captured live).

**Result: 22/22 observations `clear`** across **5 independent
references** — CoinGecko (10), Reflector-CEX (4), Redstone (4),
Chainlink (2), Band (2). Worst absolute delta:

| pair | reference | delta |
|---|---|---|
| XLM/USD | band | −0.551% |
| BTC/USD | chainlink | −0.353% |
| XLM/USD | redstone | +0.264% |
| XLM/EUR | coingecko | −0.173% |
| XLM/USD | coingecko | −0.166% |

All within the historical "<0.25% typical, <1% clear" band; no
observation in `warning` or `breach`.

**What this does NOT prove:** the campaign-B1 *broadened* ask (top-50
assets vs CoinGecko bulk) — the tracked cross-reference set is the
majors + oracle-covered pairs (22 observations). Broadening needs the
CoinGecko Pro key ([OP], feed dead since 2026-06-19) for bulk reference
coverage. Also proves nothing about assets with no external reference
(the long tail is exactly where no oracle exists).
