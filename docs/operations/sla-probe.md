---
title: SLA probe — periodic per-endpoint evidence trail
last_verified: 2026-09-05
status: living procedure
---

# SLA probe — periodic per-endpoint evidence trail

Operator guide for `cmd/stellarindex-sla-probe`, run every 15 min by
`configs/healthchecks/stellarindex-sla-probe.{service,timer}`.

## Purpose

| Metric          | Target           | Source      |
| --------------- | ---------------- | ----------- |
| p95 latency     | ≤ 200 ms         | service SLA |
| p99 latency     | ≤ 500 ms         | service SLA |
| Availability    | ≥ 99.9 %         | service SLA |
| Price freshness | ≤ 30 s staleness | service SLA |

Defaults live in `cmd/stellarindex-sla-probe/main.go::default*Target`;
flags override them for a deployment with a different contract.

- The 30 s freshness target binds `/v1/price/tip`. `/v1/price` serves the
  last **closed** one-minute bucket (ADR-0015), 30–150 s old by
  construction, so it is held to `defaultClosedBucketFreshTarget = 150s`;
  a breach there means the closed-bucket pipeline is behind, not an SLA
  violation.
- Freshness is measured when each response is received, and the per-run
  figure is the **stalest** response, not the median.
- A 2xx is a success only if the body carries the measurement: `/price`
  and `/price/tip` need a parseable `data.observed_at`; `/oracle/latest`
  needs non-empty `data`. Otherwise the sample fails (`unit_failed`, then
  `stellarindex_sla_probe_stale`).
- Latency percentiles (p50/p95/p99) use successful responses only; every
  failure counts against availability. The run deadline stops new
  requests but never cancels one in flight: each runs to completion or
  its own timeout (10 s, or the run duration if shorter), so a hung API
  reads as failures.
- Exit 0 = pass, 1 = any SLA violated. The 15-min cadence meets the SEV-2
  detection requirement (≤ 30 min) without eating the rate budget.

## Operator wiring

```sh
sudo cp configs/healthchecks/stellarindex-sla-probe.{service,timer} /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now stellarindex-sla-probe.timer
```

Override defaults via `/etc/default/stellarindex-healthchecks`. These are
the only variables `configs/healthchecks/sla-probe.sh` reads; it always
requests a JSON report and passes no other flags:

```sh
SLA_PROBE_BASE_URL=http://localhost:3000/v1  # default (see below)
SLA_PROBE_DURATION=30s                       # default; 120s smooths percentiles on a memory-pressured single-instance host
SLA_PROBE_CONCURRENCY=1                      # default
SLA_PROBE_PAIR=native,fiat:USD               # default; exactly one asset,quote pair
SLA_PROBE_TEXTFILE_OUTPUT=/var/lib/node_exporter/textfile_collector/sla_probe.prom  # default; empty disables the metrics
STELLARINDEX_PROBE_API_KEY=sip_…              # vault-minted key; required (see below)
HEALTHCHECKS_URL_SLA_PROBE=…                 # ping URL
```

> **The default target is the API's own listener.** `localhost:3000/v1`
> (wrapper and binary `-base-url` default; R1 runs it unset) bypasses
> Caddy, TLS, DNS and the network. Right for latency (isolates
> application time); **wrong** for availability: it cannot see a proxy
> failure, expired certificate, DNS fault or DC-network outage. Pointing
> it at the public URL from R1 still hairpins through the box. Until an
> off-host probe exists, the availability row is an objective, not a
> measurement — say so anywhere it is published.

To probe more pairs, run the binary directly with repeated `-pair` flags;
each repeats the chart, price and oracle-latest probes for that pair.

### Why an API key is required

Without `STELLARINDEX_PROBE_API_KEY`, the probe hits
`[api].anon_rate_limit_per_min` (shipped default 60/min; R1 sets 6,000),
every non-`/healthz` endpoint reads as unavailable, and the verdict fails
for reasons unrelated to the SLA. Mint a load-test key from the operator
vault (same class as `STELLARINDEX_LOAD_API_KEY` for the k6 weekly) and
set it before enabling the timer. It is sent as
`Authorization: Bearer <key>`, never on the unit's command line.

The key does not lift the limit where `[api].key_rate_limit_per_min`
equals the anonymous one (R1: both 6,000). The binary's `-max-rps`
pacing (default 100 req/s, shared by all workers; the wrapper does not
pass it) keeps a 30 s run at 3,000 requests, half the budget, leaving
room for the smoke runner. `-max-rps 0` removes the cap; unpaced, a 30 s
run crosses 6,000 and the 429s fail availability.

Each endpoint's `failed_by_status` counts failures by `429`, `4xx`,
`5xx`, `timeout`, `conn` or `body` (2xx that broke the contract); the
availability reason names the dominant one, e.g.
`price: availability=51.80% < target 99.90% (429 x 848)`.

## Which number is the latency SLO

**Read `stellarindex_sla_probe_latency_ms`, not a quantile over
`http_request_duration_seconds`.** At ~0.08 rps of real traffic
(~24 requests per 5 min), the served p99 is effectively the single
slowest request; one cold cache miss moves it by seconds. The probe
drives ~150 samples per endpoint per 30 s run against a fixed basket.

| question | query |
|---|---|
| Are we meeting the latency SLA? | `stellarindex_sla_probe_latency_ms{quantile="0.95"}` |
| Did the last probe run pass? | `stellarindex_sla_probe_unit_failed` (0 = pass) |
| What do real users see, typically? | `histogram_quantile(0.5\|0.95, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))` |
| What was the slowest real request? | the served p99 — read it as a **max**, not a percentile |

Probe metrics stay in their own namespace. Never emit synthetic load into
`http_request_duration_seconds`: at this volume it would be 99%+ of the
samples.

### A latency-less probe file usually means a restart, not a broken probe

If `sla_probe.prom` has no `stellarindex_sla_probe_latency_ms` line for
an endpoint, check availability first:

```sh
grep -E "availability_pct|unit_failed" \
  /var/lib/node_exporter/textfile_collector/sla_probe.prom
stat -c %y /var/lib/node_exporter/textfile_collector/sla_probe.prom
```

Availability `0.000` with `unit_failed 1` means every request failed, so
no latency is emitted (rather than a misleading `0.000`). Usually the run
landed in a deploy's API restart: compare the mtime with the deploy
window. Values persist until the next tick (≤ 15 min). The alerts'
`for: 30m` needs two consecutive bad runs, so one deploy-window failure
does not page.

## Reading the output

```sh
sudo journalctl -u stellarindex-sla-probe.service -n 100 --output=cat | jq .
```

```json
{
  "base_url": "http://localhost:3000/v1",
  "duration_sec": 30.0, "concurrency": 1, "max_rps": 100,
  "sla": {"p95_ms": 200, "p99_ms": 500, "freshness_sec": 30, "availability_pct": 99.9},
  "per_endpoint": [
    {"endpoint": "price", "path": "/price", "samples": 120, "successes": 120,
     "availability_pct": 100.0,
     "latency_ms": {"p50": 12.0, "p95": 45.0, "p99": 78.0, "max": 102.0, "mean": 18.0},
     "observed_at_fresh_sec": 1.5}
  ],
  "verdict": "pass",
  "failed_reasons": []
}
```

A `fail` verdict lists reasons, e.g. `["price: p95=215.3ms > target 200.0ms"]`.
The wrapper (`/opt/stellarindex/healthchecks/sla-probe.sh`) reports a
breach on three channels:

1. POSTs the JSON report to `${HEALTHCHECKS_URL_SLA_PROBE}/fail`.
2. Writes `stellarindex_sla_probe_unit_failed 1` to the textfile
   (→ `stellarindex_sla_probe_unit_failed_alert`).
3. The report lands in journald.

The wrapper **always exits 0**, so `systemctl is-failed` will NOT show a
breach; a missing or non-executable probe binary is also pinged as a fail.

## Pre-flight: spot-check from the operator's laptop

```sh
stellarindex-sla-probe \
  -base-url https://api.stellarindex.io/v1 \
  -duration 10s \
  -concurrency 1 \
  -report-format text
```

`verdict: pass` confirms the endpoint set, rate-limit headroom and
freshness path before enabling the timer. From a laptop this *is* an
edge measurement (TLS, DNS, Caddy, network), which the on-host run is not.

## Diagnostic: per-endpoint latency sweep

To find *which* endpoint is slow (rather than prove the SLA), run
`scripts/dev/api-latency-sweep.sh`. It hits every anonymous GET endpoint
`ITERS` times, ranks slowest-first by p50/p95/p99 and flags anything over
the 200 ms p95 SLO or the 1 s ceiling. `CACHE_BUST=1` exposes uncached
cost, `JSON=1` emits a diffable array, `--spec-check` lists OpenAPI GET
paths the sweep misses. Run it on the host (`ssh root@<host> 'bash -s' <
scripts/dev/api-latency-sweep.sh`) for pure server compute, or with
`API_BASE_URL` set for an edge measurement. It is a diagnostic, not a
contract test: shape is pinned by `scripts/dev/r1-smoke.sh`.

## Textfile-collector integration

`-textfile-output PATH` (wrapper: `SLA_PROBE_TEXTFILE_OUTPUT`) writes via
`<path>.tmp`-then-rename; node_exporter skips `.tmp` files, so a partial
write never scrapes.

```
stellarindex_sla_probe_latency_ms{endpoint=,quantile=}      gauge   ms
stellarindex_sla_probe_availability_pct{endpoint=}          gauge   percent
stellarindex_sla_probe_freshness_sec{endpoint=}             gauge   seconds (only when present)
stellarindex_sla_probe_samples{endpoint=}                   gauge   count
stellarindex_sla_probe_run_duration_seconds                 gauge   seconds
stellarindex_sla_probe_unit_failed                          gauge   1 on fail, 0 on pass
stellarindex_sla_probe_last_pass_timestamp                  gauge   unix; only on pass
```

### Alerts

`deploy/monitoring/rules/sla-probe.yml`; runbooks under
`docs/operations/runbooks/sla-probe-*.md`.

| Alert | Condition | Severity |
|-------|-----------|----------|
| `stellarindex_sla_probe_p95_breach` | per-endpoint p95 > 200 ms for 30 min | **P2** page |
| `stellarindex_sla_probe_p99_breach` | per-endpoint p99 > 500 ms for 30 min | **P2** page |
| `stellarindex_sla_probe_availability_breach` | per-endpoint availability < 99.9 % for 30 min | **P2** page |
| `stellarindex_sla_probe_freshness_breach` | `price` > 150 s (ADR-0015 closed-bucket bound), every other endpoint > 30 s, for 30 min | **P2** page |
| `stellarindex_sla_probe_unit_failed_alert` | verdict gauge = 1 for 30 min | P3 ticket |
| `stellarindex_sla_probe_stale` | `last_pass_timestamp` older than 90 min (6× cadence) or absent | **P2** page |
