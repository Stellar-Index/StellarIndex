---
title: Runbook — divergence-refresh-error-dominant
last_verified: 2026-08-29
status: living
severity: P3
---

# Runbook — `stellarindex_divergence_refresh_error_dominant`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_divergence_refresh_error_dominant` |
| Severity | P3 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/divergence.yml` (group `stellarindex.divergence`, `severity: ticket`, `for: 30m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/divergence.yml`. |
| Typical MTTR | 5–60 min (usually upstream-reference recovery) |
| Impact | The API's `flags.divergence_warning` stops updating. After the 5-min cache TTL elapses, consumers see no warning even when prices DO diverge — false-negative window. Aggregate price endpoints continue serving; only the divergence flag is degraded. |

## Symptoms

- The real rule expr (note the cold-start guard):

  ```promql
  sum(rate(stellarindex_divergence_refresh_total{outcome="refresh_error"}[5m]))
    >
  sum(rate(stellarindex_divergence_refresh_total{outcome="ok"}[5m]))
  and
  sum(rate(stellarindex_divergence_refresh_total{outcome=~"refresh_error|ok"}[5m])) > 0
  ```

  sustained 30+ min — the `and` clause avoids firing during cold
  start when both rates are 0.
- Aggregator log lines repeat `divergence refresh failed` with the underlying error (CoinGecko 429, Chainlink RPC timeout, Redis cache write failure).
- After 5 min sustained, `/v1/price` consumers reading `flags.divergence_warning` see whatever the last successful refresh wrote — eventually nothing once entries TTL out.

## Background — why this fires

The aggregator's orchestrator calls `divergence.Service.RefreshPair`
per configured pair, rate-limited by
`[aggregate] divergence_min_interval_seconds` (default 300s =
`cachekeys.DivergenceTTL`): the Tick still fires every 30s, but the
divergence pass is skipped while elapsed < the min interval, so
refreshes run roughly 5-minutely (F-0030 follow-up — ~10× less
external quota while keeping the `div:<asset>` cache continuously
populated). Each pass queries the configured references
concurrently, computes the median, and writes `div:<asset>` in
Redis with a 5-min TTL.

The reference set is broader than the original CoinGecko +
Chainlink pair: since the oracle-reference wiring, the on-chain
oracle references — Reflector (`reflector-dex` / `reflector-cex` /
`reflector-fx`), Redstone, and Band — are **default ON**, reading
our own ingested `oracle_updates` rows (zero external quota;
`internal/divergence/oracle.go`), and the supply cross-check family
runs alongside. The aggregator's startup log line
`divergence refresher wired` lists exactly which references are
live — read it before assuming which upstream is in the blast
radius.

`refresh_error` outcomes mean the call reached the refresher but
something downstream broke. The common patterns:

1. **CoinGecko rate-limit.** The anonymous no-key tier was
   tightened in late 2024 to roughly ~30 calls/DAY before 429s.
   The divergence price reference batches ALL configured pairs
   into ONE `/simple/price` call per pass (cached 25s,
   `internal/divergence/coingecko.go`), so at the 300s
   min-interval it makes ~288 calls/day — over the anonymous
   ceiling but well inside a demo key's ~30/min / 10K-day quota.
   **However: the divergence PRICE reference has NO API-key
   plumbing at all** — only the supply cross-check reference takes
   `COINGECKO_API_KEY`, and the CEX-class poller takes
   `COINGECKO_DEMO_API_KEY`; neither reaches
   `divergence.CoinGeckoReference`. "Get a paid key" is therefore
   NOT actionable for this alert — the working knob is
   `divergence_min_interval_seconds` (lengthen the pass interval)
   or disabling the CG reference.
2. **CoinGecko 5xx** — their free-tier endpoint occasionally
   degrades. Self-recovers in 5–30 min.
3. **Chainlink RPC unreachable.** The TOML default `rpc_url` is
   `cloudflare-eth.com`, but on r1 the env var `CHAINLINK_RPC_URL`
   in `/etc/default/stellarindex` injects an Alchemy URL (the key
   is embedded in the URL path — treat as secret), and **env
   overrides TOML** — read the env file first when diagnosing on
   r1.
4. **Redis cache write failed.** The known production class is
   BGSAVE-on-full-disk (the 2026-05-10 SEV-2) — route to
   [redis-write-blocked-disk-full.md](redis-write-blocked-disk-full.md)
   (the same runbook the `cache_write_errors` alert cites). r1 runs
   a single local `redis-server` — there is no Sentinel and no
   failover to wait out.

## Quick diagnosis (≤ 5 min)

```sh
ssh root@136.243.90.96

# 1) Confirm the alert is real and which outcome dominates.
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_divergence_refresh_total'

# 2) Which references are even wired? (logged once at startup)
journalctl -u stellarindex-aggregator --no-pager \
  | grep 'divergence refresher wired' | tail -1

# 3) Recent aggregator logs for the underlying error.
journalctl -u stellarindex-aggregator -n 200 \
  | grep 'divergence refresh failed'

# 4) Probe the external references manually:
#    CoinGecko
curl -fs 'https://api.coingecko.com/api/v3/simple/price?ids=stellar&vs_currencies=usd'
#    Chainlink — on r1 the live URL is CHAINLINK_RPC_URL in
#    /etc/default/stellarindex (env overrides the TOML rpc_url);
#    curl it with a JSON-RPC eth_chainId payload.
grep CHAINLINK_RPC_URL /etc/default/stellarindex
```

## Decision tree

| Underlying error | Likely cause | Mitigation |
| ---------------- | ------------ | ---------- |
| HTTP 429 from CoinGecko | Anonymous-tier daily cap (~30/day) vs our ~288 calls/day | Lengthen `[aggregate].divergence_min_interval_seconds` or disable the CG reference — the price reference has no key plumbing, so a paid key does NOT help here |
| HTTP 5xx from CoinGecko | Upstream degraded | Wait for upstream recovery (typically ≤30 min); the alert auto-resolves |
| Chainlink RPC timeout | RPC provider issue | Check/rotate `CHAINLINK_RPC_URL` in `/etc/default/stellarindex` (r1's Alchemy URL; env overrides TOML) OR disable Chainlink temporarily |
| Redis cache write failed | BGSAVE blocked on full disk (May-10 SEV-2 class) or Redis OOM | [redis-write-blocked-disk-full.md](redis-write-blocked-disk-full.md) |
| All-references failed | Network egress is blocked (oracle references excepted — they read local Postgres) | Check egress firewall + DNS for the aggregator host |

## Mitigation (≤ 60 min)

- [ ] **Identify the reference** failing via aggregator logs (and
      the `divergence refresher wired` line for what's live).
- [ ] **Probe the reference manually** (commands above) to confirm
      it's the upstream and not us.
- [ ] **Disable the failing reference** if recovery is taking
      longer than your tolerance window. Edit `[divergence]` in
      operator config, set the relevant `enabled = false`, restart
      the aggregator. The remaining references — including the
      zero-quota on-chain oracle references — continue feeding
      the divergence comparison.
- [ ] **Verify** `rate(stellarindex_divergence_refresh_total{outcome="ok"}[5m])`
      recovers above the `refresh_error` rate; the alert
      auto-resolves after 30 min sustained.

## Root cause analysis

Capture for the postmortem:

- The reference that was failing (CoinGecko / Chainlink / an
  oracle reference / Redis).
- The aggregator log line with the error class (HTTP status,
  network error, etc.).
- Duration of the outage (alert FIRING → RESOLVED).
- Whether `flags.divergence_warning` actually went stale on
  consumer-facing endpoints during the window.

## Known false-positive patterns

- **Cold start**: the rule's `and ... > 0` guard means a freshly
  booted aggregator with zero outcomes doesn't fire. If it fires
  shortly after a restart, refreshes ARE running and failing —
  treat as real.
- **Operator config change**: editing `[divergence]` while the
  aggregator is running causes one pass of refresh_error before
  the new config takes effect. Self-recovers; ignore short blips.

## Related

- [`docs/architecture/aggregation-plan.md`](../../architecture/aggregation-plan.md)
  — divergence service architecture.
- [ADR-0019](../../adr/0019-anomaly-response-and-confidence-scoring.md)
  — divergence's role in the confidence score.
- `internal/divergence/` package code.
- `divergence-no-reference.md` — the `outcome="no_reference"`
  sibling (CS-088): every reference went dark for a pair. That
  outcome is neither `refresh_error` nor `ok`, so the
  all-references-dark case is **invisible to THIS alert** — the
  `_no_reference` rule exists precisely to see it.
- `redis-write-blocked-disk-full.md` — the Redis cache-write
  failure class.
- Sibling alerts `stellarindex_price_divergence_warning` /
  `_critical` are **INERT** — nothing produces
  `stellarindex_our_price` / `stellarindex_reference_price` as
  Prometheus metrics (values live in Postgres + Redis; tracked in
  `scripts/ci/lint-metric-refs.sh`'s `KNOWN_INERT` list). This
  alert is the LIVE Prometheus signal for the divergence worker.

## Changelog

- 2026-08-29 — re-verified against HEAD. Redis root cause
  corrected: no Sentinel exists (single local redis-server) and
  `redis-conns-saturated.md` does not exist — the real production
  class is BGSAVE-on-full-disk (May-10 SEV-2) →
  `redis-write-blocked-disk-full.md`. Cadence corrected: the
  divergence pass is rate-limited by
  `[aggregate] divergence_min_interval_seconds` (default 300s =
  DivergenceTTL) — tick stays 30s, refresh ~5-minutely. Reference
  set updated: on-chain oracle references (Reflector dex/cex/fx,
  Redstone, Band) are default ON reading our own oracle_updates
  rows, plus the supply cross-check family; the
  `divergence refresher wired` startup line lists what's live.
  CoinGecko math rewritten: anonymous tier ~30 calls/DAY
  post-2024; the reference batches all pairs into ONE
  /simple/price call per pass (25s cache) ≈ 288 calls/day at the
  300s min-interval; the divergence PRICE reference has NO API-key
  plumbing (only the supply reference takes COINGECKO_API_KEY) —
  "get a paid key" removed, knob is divergence_min_interval_seconds.
  Chainlink: r1 injects an Alchemy URL via CHAINLINK_RPC_URL env
  (env overrides TOML). Symptom expr now carries the rule's
  cold-start `and` guard. Rule citation → `rules.r1/divergence.yml`;
  commands use r1 shapes. Related: price-divergence siblings marked
  INERT (KNOWN_INERT, no producer); `divergence-no-reference.md`
  added (no_reference outcome invisible to this alert).
- 2026-05-02 — initial draft alongside the divergence-refresh
  wiring (#429).
