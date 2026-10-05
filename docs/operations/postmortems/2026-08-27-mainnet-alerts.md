---
title: Post-mortem — mainnet "Degraded performance" alert cluster
date: 2026-08-27
status: in-progress
severity: P3 (all tickets — zero customer impact; the API served correct data throughout)
author: ops
---

# Post-mortem — 2026-08-27 mainnet alert cluster

## Summary

The public status banner showed **"Degraded performance · 5 active alerts"** for an extended period. All `ticket`/P3; the API served correct data throughout, but a persistent banner
is itself a reliability signal and had to be driven to zero.

| # | Alert | Nature | Status |
| - | ----- | ------ | ------ |
| 1 | `stellarindex_api_latency_p99_high` | intermittent p99 spikes (24ms to 3.4s) | root-caused; fix needs API deploy |
| 2 | `stellarindex_assets_popular_priceless` | 1 popular SEP-41 token priceless | root-caused; pricing work |
| 3 | `stellarindex_onchain_usd_volume_coverage_low` | on-chain usd_volume coverage 94.9% vs 99.5% bar | root-caused; pricing work |
| 4 | `stellarindex_verify_archive_unit_failed` (Tier A) | nightly chain-link verify failing | **RESOLVED** |
| 5 | `stellarindex_verify_archive_tier_b_unit_failed` | nightly checkpoint-anchor verify failing | **RESOLVED** |

## Alerts 4 and 5 — archive verification (resolved)

**Root cause (shared):** `verify-archive-tier-b.service` on r1 had drifted from its ansible template
(`configs/ansible/roles/archival-node/templates/systemd/verify-archive-tier-b.service.j2`: `User=stellarindex`, `run-heavy-job.sh` singleton lock,
`VERIFY_ARCHIVE_FROM={{ hot_floor }}` = 49984000). The live unit (hand-edited 2026-08-25 04:06) ran `User=root`, `VERIFY_ARCHIVE_FROM=2`, no lock.

- Alert 5: `-from 2` walks the anchor from genesis, but after the archive-to-cold-S3 capacity migration the local `galexie-archive` bucket holds only
  [0-63999] + [49984000-tip]; the walk hits a missing LCM at ledger ~16M and exits 1.
- Alert 4: Tier B as root wrote the shared `/var/lib/stellarindex/verify-archive-state.json` root-owned `0600`; Tier A (`User=stellarindex`, CS-118) could not read it ("permission denied" in 2 ms).
- Latent second bug: 6,034 dirs under `/srv/history-archive` were `root:root 0750` (a fill under umask 027), blocking `stellarindex` traversal. Probably why Tier B had been switched to root.

**Fix:** restored r1's unit to the template, reset the stale from-genesis `in_progress` state (chain high-water 64112025 kept), chowned the state file back to `stellarindex`;
`find … -type d ! -perm -o+rx -exec chmod o+rx` on the 6,034 dirs (public ledger data); codified the mirror-perm sweep idempotently in the archival-node role (`04-users.yml`).
Verified: Tier A green (`chain-link integrity OK`, exit 0); Tier B verified 957 checkpoints, 0 missed, 0 permission errors (capped test); timers (05:27 / 06:44 CEST) run the corrected config.
**Follow-ups:** nothing re-asserts these units between deploys; a periodic `ansible-playbook --check` drift alarm on r1's units would have caught it. That alarm shipped as `.github/workflows/ansible-drift.yml` (INV-0994). Tier B's first run is a
full ~2.5 h pass from 49984000, later runs incremental, deprioritised (CPUWeight/IOWeight=20).

## Alert 1 — API p99 latency (root-caused; needs deliberate deploy)

Not an outage: a periodic cold-cache stampede. Real latency is healthy (p50 ~3 ms, p95 ~117 ms, per-route p99 <200 ms). The alert (>2s for 2m) trips on non-stream requests >=2s:
`/v1/assets` (~3.6 s, always two concurrent, every ~10 min) and `/v1/pairs` (~2.5 s then measured ~10 s over 90 min, every ~5 min), all status 200. SSE streams
(`/v1/ledger/stream`, `/v1/price/tip/stream`, 60-90 s) are excluded from the histogram by `isStreamingRoute`.

Mechanism: cache-expiry thundering herd. `/v1/assets` has an SWR cache + prewarmer (`asset_catalogue_cache.go`) but the key is the full arg-tuple; tuples clients send (`limit=1` status probes,
`limit=50`, `limit=10&order_by=…&include=sparkline`) differ from the warmed ones, so they stay cold and concurrent misses each recompute with no single-flight. The per-pair `/v1/pairs`
MarketsReader query is the worst (~10 s cold). This is the "phantom warmed slot" failure `stellarindex_api_cache_miss_rate_high` warns of.

**Fix (code, deliberate deploy):** align prewarmer tuples with real requests or coarsen the key; add single-flight coalescing; make the SWR refresh truly async (never block a served request);
optimise the cold `/v1/pairs` and `/v1/assets` queries; optionally make `/v1/assets?limit=1` cheap. An external r1-local cache-warmer timer was **rejected**: the recompute is a blocking,
histogram-counted request whoever triggers it, so it would not lower p99 and would add DB load.

## Alerts 2 and 3 — pricing coverage (structurally unpriceable SEP-41 tokens)

Both fire on SEP-41 tokens trading only against other unpriced SEP-41 tokens, closed clusters with no XLM/USDC anchor, which the usd-volume runbook calls an "expected residual".
Alert 2: `CAUP7NFA…772J` (~$72k/7d, 6.4k trades, not wash) trades only against `CBIJBDNZ…`, which touches `native` 0 times; it has volume via a bridge but no derivable spot price, so it
clears neither the price path nor the auto-withheld escape (`Volume24hUSD < $1000`). Alert 3 (94.9% vs 99.5%): dominated by the BLTA/BLTB/BLTC soroswap triangle
(`CCUYL75…`/`CAANCS…`/`CB2J5F…`, trading since April, ~7k trades/6h), SEP-41 to SEP-41 with no USD leg.
**Fix (pricing work, branch with tests + fix-verifier before deploy):** a quote-path/USD-proxy bridge for such clusters, or exclude structurally-unpriceable SEP-41 pairs from the coverage denominator and priceless tripwire.

## Adjacent alerts seen

- `stellarindex_stellar_stack_lagging` (ticket): stellar-core installed 27.1.0 vs candidate 28.0.1 (archivist 484 to 486): the known Protocol 28 upgrade, due before 2026-09-16; tracked in `docs/operations/protocol-upgrades.md`.
- `stellarindex_deadmansswitch` (informational): fires by design; excluded from the "degraded" verdict.
- `stellarindex_anomaly_freeze_engaged` (P3) and `_active` (informational): a recurring single-source thin-fiat pattern. The mechanism works (API serves last-known-good VWAP with `flags.frozen=true`), but cycles:
  ~40 freezes / 6h on class=default, one per ~9 min, so `rate(engaged_total[5m])` is never zero and the ticket never clears; `_active` flaps as each 30-min hold auto-releases.
  Driver: `crypto:XLM/fiat:GBP` and `crypto:XLM/fiat:EUR`, single thin venue. Sample: `freeze engaged pair=crypto:XLM/fiat:GBP class=default reason="phase2:3_signal_AND confidence=0.350 z=7.65 sources=1" hold_until=+30m corroborated=true`.
  The tell is `sources=1`. Candidate fixes (money-path, branch + tests + fix-verifier, deploys on hold): (1) derive thin fiat cross-rates as XLM/USD x fiat rate when the direct market is single-source;
  (2) require >=2 sources for a z-score freeze; (3) stopgap: label the freeze counter with `sources`/confidence and keep single-source auto-releasing cycles off the ticket (the `sustained`/`escalated` page path already ignores them per the 2026-08-06 reshape), which needs an aggregator metric-label change.

## Net effect

The two broken recurring alerts (archive Tier A/B, a config-drift bug) are fixed at root cause and codified. The rest are a caching artifact, two expected pricing residuals, a freeze-cycling pattern and one planned upgrade, each with a correct fix identified and none safe to hot-patch on the production money box.
