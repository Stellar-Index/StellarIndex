---
title: Runbook — aggregator-silent
last_verified: 2026-08-28
status: current
severity: P1
---

# Runbook — `stellarindex_aggregator_silent`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_aggregator_silent` |
| Severity | **P1** (`severity: page`) |
| Detected by | `configs/prometheus/rules.r1/aggregator.yml` (group `stellarindex.aggregator`, `severity: page`, `for: 5m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/aggregator.yml`. |
| Typical MTTR | 5–20 min |
| Impact | Zero VWAP cache writes for 5+ min. **`/v1/price`** serves progressively staler cached VWAPs — the orchestrator publishes `cachekeys.VWAP` keys that `price.go` serves — so `flags.stale` flips, and rewritten/triangulated pairs start 404ing as their key TTLs lapse (the May-10 SEV-2 shape). **`/v1/vwap` is unaffected**: it ALWAYS scans raw trades on-query (`internal/api/v1/vwap.go:114-118`) — it neither degrades nor lags when the aggregator goes silent. |

## Symptoms

- The full rule expr:
  `sum(rate(stellarindex_aggregator_vwap_writes_total[5m])) == 0 or absent_over_time(stellarindex_aggregator_vwap_writes_total[10m]) == 1`,
  held for `for: 5m`, `severity: page`.
- `/metrics` on the aggregator binary shows the counter not advancing
  between scrapes.
- `/v1/price` responses carry an aging `observed_at` /
  `flags.stale=true`.

## Quick diagnosis (≤ 5 min)

```sh
ssh root@136.243.90.96

# 1) Is the binary alive at all?
systemctl status stellarindex-aggregator
journalctl -u stellarindex-aggregator -n 50 --no-pager

# 2) What does the orchestrator say about its last tick?
# F-1301 (codex audit-2026-05-13): aggregator binary auto-shifts
# its metrics listener from :9464 to :9465 when it would collide
# with the indexer. R1's prometheus.r1.yml scrapes :9465 for the
# aggregator; :9464 is the INDEXER's port. Use :9465 when probing
# the aggregator. The auto-shift keys off `obs.metrics_listen` being
# left at its default (cmd/stellarindex-aggregator/main.go:1689-1694,
# shifts to 127.0.0.1:9465); to pin a different port, set
# `obs.metrics_listen` in the aggregator's TOML.
curl -fs http://localhost:9465/metrics | grep -E '^stellarindex_aggregator_(ticks_total|empty_windows|dropped_trades|vwap_writes)'

# 3) Is Redis reachable and accepting writes? (single local
# redis-server on localhost — no auth, no replica)
redis-cli PING
redis-cli SET _aggregator_probe "$(date -Iseconds)" EX 60

# 4) Is Timescale serving trades for the configured pair set?
runuser -u postgres -- psql -d stellarindex -c "SELECT pair, COUNT(*) FROM trades WHERE timestamp > now() - interval '5 minutes' GROUP BY pair ORDER BY 2 DESC LIMIT 10;"
```

Three signal-pairs to read off the metrics dump:

| What you see | What it means | Where to look next |
| ------------ | ------------- | ------------------ |
| `ticks_total{outcome="ok"}` advancing, `vwap_writes_total` flat | Ticks running but every (pair, window) lands in the empty-window branch | `empty_windows_total` should match the (pairs × windows) rate; if so → check Timescale for trade volume |
| `ticks_total{outcome="error"}` only advancing | Refresh-loop failures. Check journalctl for `refresh failed` lines | Likely Timescale or Redis side; section "Mitigation" below |
| `ticks_total` flat, no advance at all | Orchestrator tick loop not running | Process is alive but stuck — `pprof` goroutines, or restart |

## Mitigation (≤ 15 min)

- [ ] **If Redis is the proximate cause** (PING fails / writes error):
      fix the local redis-server — there is NO standby to fail over to
      (r1 runs a single Debian-packaged redis-server on localhost, no
      sentinel/replica/auth). Restart the unit, or if writes fail with
      `MISCONF` (BGSAVE blocked on a full root FS — the May-10 SEV-2
      class) free root-FS disk per
      [`redis-write-blocked-disk-full.md`](redis-write-blocked-disk-full.md).
      The aggregator retries on the next tick, no restart needed.
- [ ] **If the tick loop is wedged**: `systemctl restart stellarindex-aggregator`.
      The orchestrator's first-tick-immediate behaviour means VWAP writes
      resume within `interval_seconds` (default 30 s) of restart.
- [ ] **If empty windows are the cause** (Timescale has trades but every
      configured pair returns zero rows): check whether
      `enable_stablecoin_fiat_proxy` is needed — a fiat-quote pair
      (`XLM/fiat:USD`) without expansion only matches direct FX-feed
      trades, which may be sparse if FX connectors aren't enabled.
- [ ] **Verification**: `vwap_writes_total` resumes advancing within
      one tick interval; alert clears within `for: 5m` of recovery.

## Root cause analysis

Capture for the postmortem:

- `journalctl -u stellarindex-aggregator --since='15 minutes ago'`
- A `/metrics` snapshot before and after recovery (paste both
  `ticks_total` and `dropped_trades_total` series).
- Any concurrent Timescale or Redis incidents from the same window.
- The current `cfg.Aggregate.*` state and the configured `Pairs` /
  `Windows` set — if expansion is off but the operator wanted it on,
  that's a config drift item.

## Known false-positive patterns

- **Aggregator binary not deployed in this environment** — dev /
  staging stacks where only the indexer + API are running. The
  rule's `absent_over_time` branch now fires in such an environment
  (no series at all). Silencing via AlertManager routing is the
  right fix. (There is no `up{job=...}` precondition on the rule —
  and the r1 job name is `stellarindex-aggregator` anyway.)
- **First 60 s after a fresh aggregator boot** — the immediate
  initial tick lands inside the alert's 5 min window, so a clean
  start should never trigger. If it does, suspect a config or
  storage bring-up issue rather than the orchestrator itself.

## Related

- ADR-0007 (cache strategy) — explains why VWAP is pre-computed
  rather than on-query.
- `stellarindex_aggregator_outlier_storm` — post-redesign
  (2026-08-28) it measures per-venue VWAP dispersion
  (`stellarindex_aggregator_venue_vwap`), not trim flood; the
  time-local filter (`internal/aggregate/outliers_local.go`) no
  longer trims agreed regime shifts, so a market event should NOT
  drive VWAP writes to zero. The trim-flood siblings are
  `stellarindex_aggregator_outlier_trim_fraction` +
  `_trim_rate_legacy` (legacy retires 2026-09-04).
- `aggregator-outlier-storm.md` — sister runbook; check its
  symptoms before assuming this is a pure orchestrator failure.

## Changelog

- 2026-08-28 — re-verified against HEAD. Impact rewritten: `/v1/vwap`
  always scans raw trades on-query (`internal/api/v1/vwap.go:114-118`)
  and is UNAFFECTED — the aggregator's cache writes feed `/v1/price`
  (May-10 SEV-2 staleness/404 shape). Full rule expr quoted (incl. the
  `absent_over_time` branch). Redis failover advice removed — no
  standby exists on r1; routed to `redis-write-blocked-disk-full.md`
  instead. F-1301 port note corrected: the auto-shift keys off
  `obs.metrics_listen` (the `AGGREGATOR_METRICS_PORT` env was
  fiction). Outlier-storm bullet updated for the 2026-08-28
  venue-dispersion redesign. Rule citation → `rules.r1/aggregator.yml`;
  commands use r1 shapes.
- 2026-04-25 — initial draft alongside the aggregator metrics
  PR #26 wire-up.
