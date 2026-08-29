---
title: Runbook — cache-miss-rate-high
last_verified: 2026-08-29
status: current
severity: P2
---

# Runbook — `stellarindex_api_cache_miss_rate_high`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_api_cache_miss_rate_high` |
| Severity | P2 (ticket) |
| Detected by | `configs/prometheus/rules.r1/api.yml` (group `stellarindex.api`, `severity: ticket`, `for: 10m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/api.yml` (same expr/for/labels). |
| Typical MTTR | 30–60 min (mostly diff-and-deploy time once the drift dimension is identified) |
| Impact | One in-memory cache (e.g. `markets`/`all_pools`) is serving more than half its requests cold, which means a prewarm-key drift OR genuine load spike against an un-prewarmed surface. Cold-cache requests pay the 5–10s underlying SQL scan; users see slow page loads on whatever surface uses that cache. |

## Symptoms

- `(rate(stellarindex_api_cache_ops_total{result="miss"}[5m]) / rate(stellarindex_api_cache_ops_total[5m])) > 0.5` sustained 10 min
- The alert fires per `(cache, op)` so the label tells you which cache.
- Likely correlated: the underlying API surface gets noticeably slower (the explorer page that hits this cache feels sluggish) and `http_request_duration_seconds` p95 climbs.
- NOT correlated: total request volume — we threshold on ratio, so a low-traffic cache with 100% miss won't fire (the `> 0.1 req/s` floor in the expression prevents flapping on quiet caches).

## Quick diagnosis (≤ 10 min)

1. **Identify which cache + op fired.** The alert label is `{cache="<name>", op="<method>"}`. Today's SEVEN `cache` label values (grep `APICacheOpsTotal.WithLabelValues` under `internal/api/v1/`): `coins`, `issuers`, `markets`, `network_stats`, `observations`, `oracle`, `sources_stats`. The `markets` cache's ops:
   - `markets` / `distinct_pairs` — backs `/v1/markets` (no source filter)
   - `markets` / `source_markets` — backs `/v1/markets?source=<x>`
   - `markets` / `asset_markets` — backs `/v1/markets?asset=<x>`
   - `markets` / `all_pools` — backs `/v1/pools`
2. **Check the prewarm code.** Open `cmd/stellarindex-api/main.go`, function `prewarmCaches` (main.go:4429) — it dispatches two tiers: `prewarmHeavy` (the `sources_stats` family, 5-min cadence) and `prewarmLight` (markets/pools/coins/native, 60 s cadence). Find the call corresponding to the alerted op. Compare every argument against what the handler at `internal/api/v1/markets.go` passes.
3. **Diff the cache keys.** The cache key is a `fmt.Sprintf` of the args (see `internal/api/v1/markets_cache.go` `fetchPairs` / `fetchPools`). If the prewarm passes `Order=0` and the handler passes `Order=1`, the keys differ. We've shipped 3 of these bugs in 24h (#1185 Order, #1194 Sources, #1195 Limit) — same family.
4. **Sanity check the cache TTL vs prewarm cadence.** `v1.NewCachedMarketsReader(...)` is constructed with `2*time.Minute`; `prewarmCaches` runs the heavy tier every 5 min and the light tier every 60 s. If a tier's cadence ever exceeds its caches' TTL, the cache expires before the next refresh and looks like a miss-storm.

## Mitigation

- [ ] Step 1 — Fix the drifted dimension in the prewarm. If the prewarm and handler share a value-derivation function (like `v1.DexSourceNames()`), use it instead of independently re-computing. Pattern: prefer one source-of-truth over duplicated logic.
- [ ] Step 2 — Cut a release + deploy. Cache observability fixes don't backport — operator needs to deploy a binary that ships both the fixed prewarm AND the metric.
- [ ] Verification: `(rate(...{result="miss"}[5m]) / rate(...[5m])) < 0.1` within 1 cycle — ≤ 60 s post-deploy on r1 for the LIGHT tier only (markets/pools/coins/native); the `sources_stats` family warms on the 5-min heavy tier, so allow up to 5 min there.

## Known false-positive patterns

- **Cold start.** Right after a binary restart, the cache is empty for the first prewarm cycle. Both prewarm and user requests miss. The alert's `for: 10m` window covers this, but a long boot delay can trip it.
- **TTL > prewarm cadence.** If someone bumps the cache TTL without bumping the prewarm cadence, the alert fires legitimately — but the fix is the cadence, not the prewarm code.
- **Op the prewarm doesn't cover.** Only `asset_markets` remains un-prewarmed today. `source_markets` IS prewarmed per registered CEX (`v1.CexSourceNames()`, limit=200, volume-desc — the explorer's `/exchanges/{name}` shape), and `all_pools` is prewarmed per-limit `{5,25,100,200}` on the DEX-source filter PLUS one pass per DEX (`soroswap`/`phoenix`/`aquarius`/`sdex`/`comet`, limit=100). High miss rate on `asset_markets` is expected — it's cached on first user request and served from cache thereafter. Suppress with a per-op exception or extend the prewarm.

## Related

- Worked examples of the pattern: PR #1185 (Order dimension drift),
  #1194 (Sources dimension), #1195 (Limit dimension via implicit
  handler-side subtraction). Read these for fix exemplars; all
  three follow the same diff-and-fix shape this runbook describes.
- [api-latency.md](api-latency.md) — what the user sees when this
  fires (cache miss → cold-cache SQL → 5–10s response).
- The metric itself: `stellarindex_api_cache_ops_total` documented
  in [docs/reference/metrics/README.md](../../reference/metrics/README.md).

## Changelog

- 2026-08-29 — re-verified against HEAD (Wave I). Cache inventory
  expanded from 4 markets-ops to the real seven `cache` label
  values (`coins`/`issuers`/`markets`/`network_stats`/`observations`/
  `oracle`/`sources_stats`); `prewarmOnce` corrected to
  `prewarmCaches` (main.go:4429) with its two tiers (heavy 5 min /
  light 60 s); TTL corrected 30 s → `2*time.Minute` and the stale
  25 s cadence claim dropped; prewarm coverage updated —
  `source_markets` is now prewarmed per-CEX and `all_pools`
  per-limit + per-DEX, leaving only `asset_markets` cold; the
  "≤ 60 s post-deploy" verification scoped to the light tier.
  Status promoted draft → current.
- 2026-05-09 — initial draft, motivated by PRs #1185 / #1194 / #1195 / #1196.
