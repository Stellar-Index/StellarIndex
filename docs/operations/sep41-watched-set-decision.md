# SEP-41 watched-set — decision doc (operator approval needed)

**Status:** awaiting @ash approval of the candidate list below.
**Context:** `watched_sep41_contracts = []` on r1 — the sep41_transfers +
sep41_supply sources are config-disabled (by design: post-CAP-67 the
token-event firehose includes every classic-asset movement; the watched
set is the curated alternative the F-1316 fix locked in).

**What turning it on buys:** Algorithm-3 supply (mint−burn−clawback) for
pure-Soroban tokens → market-cap/FDV on Freighter V2 for those tokens;
per-token transfer history; sep41×2 join the ADR-0033 verdicts (15→17).

**What it does NOT affect:** pricing (trades/oracles), classic + SAC
supply (Algorithms 1+2 — covers XLM, USDC, EURC, BLND, AQUA, …, i.e.
everything that trades meaningfully today).

## Candidate analysis (from trades, 2026-06-12)

Top contract-addressed assets by trade count, classified:

| Contract | Trades | Verdict |
|---|---:|---|
| `CAS3J7…XOWMA` (XLM SAC) | 756k | EXCLUDE — Algorithm 1 |
| `CCW67T…JMI75` (USDC SAC) | 746k | EXCLUDE — Algorithm 2 via sac_wrappers |
| `CAUIKL…6OJPK` | 181k | EXCLUDE — in sac_wrappers (classic-backed) |
| `CDFZUV…RUJFG` | 103k | **CANDIDATE — identify** (not in sac_wrappers) |
| `CBIJBD…FM6VN` | 61k | **CANDIDATE — identify** |
| `CD25MN…VG5JY` (BLND SAC) | 57k | EXCLUDE — sac_wrappers |
| `CCKCKC…DBQIQ` | 50k | **CANDIDATE — identify** |
| `CDTKPW…JBQLV` (EURC?) | 42k | likely SAC — **verify + add to sac_wrappers if so** (possible config gap) |
| `CCCRWH…PHGU2`, `CBZ7M5…DK32`, `CDIKUR…FJKP`, `CAUP7N…772J`, `CBH4M4…OCKF` | 20–38k | **CANDIDATES — identify** |

Plus (regardless of trade volume): the DeFindex vault share tokens and
Blend pool b/d-tokens are pure-SEP-41 mint/burn surfaces if we want
their supplies — defer to phase 2 of this decision.

## Recommendation

1. I identify each CANDIDATE via SEP-1/home-domain + stellar.expert
   cross-reference (30 min) and split them SAC vs pure-SEP-41.
2. Real SACs → `[supply.sac_wrappers]` additions (closes the EURC-class
   config gap; Algorithm 2 covers them).
3. Genuine pure-SEP-41 tokens with volume → `watched_sep41_contracts`.
4. Deploy precondition order: update TOML → restart indexer →
   `projector-replay -source sep41_supply -from 50457424` (replay path
   verified working) → add sep41×2 to the reconcile catalogue →
   compute-completeness → 17/17.

**@ash:** approve the approach and I'll execute end-to-end; or hand me
your preferred watched list directly.
