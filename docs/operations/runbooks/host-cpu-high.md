---
title: Runbook — host-cpu-high
last_verified: 2026-08-29
status: current
severity: P3
---

# Runbook — `stellarindex_host_cpu_high`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_host_cpu_high` |
| Severity | P3 (informational) |
| Detected by | `configs/prometheus/rules.r1/infra.yml` (group `stellarindex.infra`; `severity: informational`, `for: 10m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/infra.yml`. |
| Typical MTTR | 30 min – days (depends on whether it's fixable code vs scale-up) |
| Impact | Not directly customer-visible. High CPU usually precedes latency degradation — if `api-latency.md` hasn't fired yet, you have lead time. |

## Symptoms

- `100 - (avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100) > 90`
  sustained 10 min (the deployed expr — per-instance average across
  all cores).
- Load avg on the host exceeds CPU count.
- The same host's `iowait` / `softirq` may also be elevated
  (useful for root-causing).

## Quick diagnosis (≤ 5 min)

```sh
# Which process is eating CPU?
ssh root@136.243.90.96 'top -b -n1 -o %CPU | head -20'

# Per-service breakdown (systemd cgroup view)
ssh root@136.243.90.96 'systemd-cgtop --order=cpu --iterations=2 -n 20'

# Is it user-CPU, system-CPU, iowait, or softirq?
ssh root@136.243.90.96 'mpstat 1 5'
```

**Heavy one-shot jobs on r1 are expected consumers.** Ops
one-shots (re-derives, backfills, bulk SQL, census walks) run
under `/usr/local/sbin/run-heavy-job.sh`, a transient systemd
scope with **batch-class CPU/IO weights (CPUWeight=25 /
IOWeight=25) and MemoryMax=20G**, while galexie carries elevated
CPU/IO weight + `MemoryLow=16G`. A `run-heavy-*.scope` dominating
`systemd-cgtop` is a deprioritised batch job doing its work —
expected, not a fault; the scheduler will preempt it for the
service units. The classic real fault is the inverse: **a heavy
binary run raw (unwrapped)**, which is how the 2026-07-05
unwindowed re-derive ballooned and wedged galexie's captive core
for 11 h. If the top CPU consumer is a heavy ops process NOT
inside a `run-heavy-*.scope`, stop it and re-run under the
wrapper.

## Typical root causes

1. **A single process is pegged.** Usually means a hot code path
   we didn't anticipate — regex in a loop, unbounded concurrency
   firing off goroutines, bad SQL plan forcing a full scan
   client-side.

2. **captive-core catchup** on a galexie (or, in Phase-3
   deployments, stellar-rpc / stellar-core) host. Replay is
   CPU-bound; expected during boot + periodic maintenance.
   On r1 today, only galexie embeds a captive-core
   ([r1-deployment-state.md](../r1-deployment-state.md)); the
   stellar-rpc / stellar-core daemons were removed 2026-04-23
   and `core-lag.md` / `rpc-lag.md` are inert there. Galexie's
   own captive-core does not expose a `/info` endpoint, so the
   end-state signal is "fresh objects in `galexie-live` after
   catchup completes" rather than the stellar-core ledger-age
   metric.

3. **Postgres bad plan** — an ANALYZE drifted, now Postgres is
   picking a seq-scan over an index. `pg_stat_statements` shows
   a specific statement with high `mean_exec_time`.

4. **Compression / backup window** on a Postgres host.
   `pgBackRest --process-max=4` or TimescaleDB compression jobs
   are CPU-intensive on purpose. (`pg_repack` is not installed
   anywhere in our fleet — don't go hunting for it.)

5. **Unwrapped heavy one-shot** — see the callout in Quick
   diagnosis: a heavy ops job run raw instead of under
   `run-heavy-job.sh` competes with the service units at full
   weight (the 2026-07-05 galexie wedge).

6. **Noisy neighbor / CPU steal** — **not applicable on r1** (a
   dedicated Hetzner box; `%steal` is structurally 0). Only
   relevant for future shared/virtualised hosts.
   - Signal: `mpstat 1` shows `%steal` > 0.

## Mitigation

- [ ] Step 1 — identify the consumer (above).
- [ ] Step 2 — if one process: is it legitimate work? Then we're
      under-provisioned — schedule a vertical scale-up. If it's
      a bug (runaway goroutine, infinite loop), file an incident.
- [ ] Step 3 — if captive-core catchup: wait. Usually resolves in
      30–120 min.
- [ ] Step 4 — if Postgres plan regression: `ANALYZE` the affected
      tables (`runuser -u postgres -- psql -d stellarindex -c
      'ANALYZE <table>'`); if the plan is still bad, rewrite the
      offending query — `pg_hint_plan` is not installed anywhere.
- [ ] Step 5 — if compression/backup: verify it completes; if it's
      running for hours, tune `--process-max` down.
- [ ] Verification: CPU drops back under 70 % sustained; alert
      clears.

## Known false-positive patterns

- **Multi-core hosts on average**: `avg(rate(...idle...))` = idle
  percentage averaged across cores. A single-core spike (pinned
  goroutine) doesn't cross 90 % on a 16-core host — so the alert
  is tuned to catch real saturation, not per-core pegs. If you
  need the per-core version, use the `max by (cpu)` variant.
- **Burst workloads**: some of our cron'd maintenance (hourly
  aggregator rollup) can burn CPU for a few minutes. The `for:
  10m` should absorb it.

## Related

- `api-latency.md` — downstream effect when CPU saturation slows
  request handlers.
- `pg-conns-saturated.md` — a common CPU-saturating scenario for
  Postgres hosts.
- `all-ingestion-down.md` — when galexie's captive-core stalls
  hard enough to halt fresh-object production.
- `core-lag.md`, `rpc-lag.md` — captive-core variants for
  Phase-3 deployments running stellar-core / stellar-rpc; inert
  on r1 today.

## Changelog

- 2026-04-23 — initial draft.
- 2026-04-30 — captive-core root-cause refers to galexie (the only
  on-host captive on r1 since 2026-04-23) rather than the removed
  stellar-rpc / stellar-core daemons. Related section flags those
  as Phase-3-only.
- 2026-08-29 — re-verified against HEAD. Detected-by converted to
  the dual-tree convention (`rules.r1/infra.yml`, group
  `stellarindex.infra`, `severity: informational`, `for: 10m`) and
  the Symptoms expr now quotes the deployed rule verbatim
  (including `avg by (instance)`). Commands use the r1 ssh shape.
  Added the missing r1 heavy-job paragraph: one-shots run under
  `/usr/local/sbin/run-heavy-job.sh` (batch-class CPU/IO weights,
  MemoryMax=20G) with galexie at elevated weight + MemoryLow=16G —
  a scoped heavy job dominating cgtop is expected; an UNWRAPPED
  one is the classic fault (2026-07-05 galexie wedge). `pg_repack`
  / `pg_hint_plan` removed (installed nowhere) in favour of plain
  ANALYZE + query rewrite; noisy-neighbor/CPU-steal marked
  non-applicable on the dedicated r1 box.
