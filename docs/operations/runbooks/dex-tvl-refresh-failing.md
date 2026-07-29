---
title: Runbook — dex-tvl-refresh-failing
last_verified: 2026-07-29
status: draft
severity: P3
---

# Runbook — `stellarindex_dex_tvl_refresh_failing`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_dex_tvl_refresh_failing` (ticket) |
| Detected by | Prometheus rules in `deploy/monitoring/rules/api.yml` + `configs/prometheus/rules.r1/api.yml` |
| Typical MTTR | 5–20 min (almost always ClickHouse or served-tier reachability, shared with louder alerts) |
| Impact | `/v1/dexes`, `/v1/protocols` and the per-protocol pages keep serving TVL — but it is a carried-forward snapshot growing stale behind a healthy-looking page. No 5xx, no error flag beyond the snapshot's own `as_of`. |

## Symptoms

- `stellarindex_dex_tvl_refresh_total{outcome="error"}` increasing
  with `outcome="ok"` flat for 30+ min.
- `journalctl -u stellarindex-api | grep "dex tvl"` shows the joined
  per-protocol error every ~10 min (`soroswap tvl: …`,
  `phoenix tvl: …` — the message names which protocol's read died).
- The explorer's DEX pages show a TVL whose underlying `as_of`
  timestamp (DEXTVLCache snapshot time) has stopped advancing.

## Quick diagnosis (≤ 5 min)

```sh
# Which protocol is failing? The refresh error is per-protocol:
journalctl -u stellarindex-api --since -30min | grep "dex tvl cache refresh" | tail -3

# Lake-side reads (soroswap/phoenix/comet reserves) → ClickHouse:
clickhouse-client --port 9300 -q "SELECT 1"

# Served-tier reads (aquarius reserve snapshots, prices_1m USD legs) → Postgres:
sudo -u postgres psql stellarindex -c "SELECT max(bucket) FROM prices_1m"
```

## Mitigation (≤ 15 min)

1. The refresher is self-healing: fix the failing backend and the
   next 10-min tick repopulates every protocol. No restart needed.
2. If only ONE protocol errors while the rest succeed (the alert
   won't fire for this — it requires zero `ok` outcomes), the failed
   protocol carries its previous entry forward; treat it as a signal
   of that protocol's reader breaking (e.g. a lake schema change) and
   file it rather than restarting anything.
3. A restart of `stellarindex-api` clears the snapshot entirely —
   the TVL field is then OMITTED until the first successful refresh
   (honest degradation). Only restart if the process itself is
   wedged.

## Root cause analysis

The refresh is one lake reserve lookup per protocol + a bounded set
of `prices_1m` point reads, under a 3-minute timeout. Sustained
failure has so far only ever meant backend reachability. If backends
are healthy and the refresh still errors, chart
`stellarindex_dex_tvl_refresh_duration_seconds` — an `error`
histogram pinned at 180 s means the reads are timing out, which is
the 40× read-amplification class (a scan lost its
`max_threads`/`max_memory_usage` pin; see the 2026-07-29
investigation in `docs/operations/v1-launch-plan.md`).

## Known false-positive patterns

- A ClickHouse restart or heavy merge window can fail one or two
  consecutive refreshes; the alert's 30-min `for:` absorbs this.
  If it fires during a known heavy one-shot job on r1, verify the
  job finished and the next tick succeeded, then close.

## Related

- Metric docs: `docs/reference/metrics/README.md` —
  `stellarindex_dex_tvl_refresh_total` /
  `stellarindex_dex_tvl_refresh_duration_seconds`.
- Sibling worker alert:
  [`sdex-orderbook-maintain-failing`](sdex-orderbook-maintain-failing.md)
  (same binary, same lake dependency).
- Cache design: `internal/api/v1/dex_tvl_cache.go` (CoverageCache
  pattern — carried-forward per-protocol entries on failure).

## Changelog

- 2026-07-29: created with the v0.21.4 background-worker metrics.
