---
title: SLA probe — periodic per-endpoint evidence trail
last_verified: 2026-05-03
status: living procedure
---

# SLA probe — periodic per-endpoint evidence trail

Operational companion to the executable SLA-evidence CLI shipped in
#283 (`cmd/stellarindex-sla-probe`). This doc covers:

- What the probe is + why it runs continuously
- Daily cron via `configs/healthchecks/stellarindex-sla-probe.{service,timer}`
- The SLA targets the probe verifies against
- Textfile-collector integration + the four shipped alerts

## Purpose

The API is bound to four SLA targets:

| Metric                   | Target           | Source            |
| ------------------------ | ---------------- | ----------------- |
| p95 latency              | ≤ 200 ms         | service SLA       |
| p99 latency              | ≤ 500 ms         | service SLA       |
| Availability             | ≥ 99.9 %         | service SLA       |
| Price freshness          | ≤ 30 s staleness | service SLA       |

The ≤ 30 s freshness target binds `/v1/price/tip`, the rolling-window
surface. `/v1/price` serves the last **closed** one-minute bucket
(ADR-0015), so its `observed_at` is 30–150 s old by construction; the
probe holds it to a separate structural bound
(`defaultClosedBucketFreshTarget = 150s`,
`cmd/stellarindex-sla-probe/main.go`) whose breach means the
closed-bucket pipeline has fallen behind its design, not that the
service SLA is violated. Measured on R1 2026-09-03:
`stellarindex_sla_probe_freshness_sec{endpoint="price"} 95.2`,
`{endpoint="price-tip"} 18.1`.

The SLA probe drives synthetic load against the deployed API,
measures per-endpoint p50/p95/p99 latency, parses `observed_at`
on the price endpoints to compute freshness, and tallies 2xx vs
non-2xx for availability. Each run emits a JSON report and exits
with code 0 (pass) or 1 (any SLA violated).

The systemd timer runs the probe every 15 minutes — tight enough
to pinpoint a SEV-2 latency-spike window (the SEV-2 detection
requirement is ≤ 30 min after the incident begins) but loose
enough that the probe itself doesn't dominate the anonymous-tier
rate budget.

## Operator wiring

```sh
sudo cp configs/healthchecks/stellarindex-sla-probe.{service,timer} /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now stellarindex-sla-probe.timer
```

Override defaults via `/etc/default/stellarindex-healthchecks`:

```sh
SLA_PROBE_BASE_URL=http://localhost:3000/v1  # default (see the note below)
DURATION=30s                                 # default
CONCURRENCY=4                                # default
PAIRS="-pair native,fiat:USD -pair USDC-G…,fiat:USD"
REPORT_FORMAT=json                           # default; text also valid
STELLARINDEX_PROBE_API_KEY=sip_…              # vault-minted key; required (see below)
EXTRA_FLAGS=""                               # default
```

> **What the default target means.** `SLA_PROBE_BASE_URL` defaults to
> `http://localhost:3000/v1` — both in
> `configs/healthchecks/sla-probe.sh:21` and in the binary's own
> `-base-url` flag — and R1 runs it unset. The probe therefore measures
> the API process's **own listener**, bypassing Caddy, TLS, DNS and the
> network. That is the right scope for latency (it isolates application
> time, which is what a code regression moves) and the **wrong** scope
> for availability: this probe cannot see a reverse-proxy failure, an
> expired certificate, a DNS fault or a DC-network outage. Point it at
> `https://api.stellarindex.io/v1` from a host **outside** the
> deployment to get an edge measurement; running it against the public
> URL from R1 itself still hairpins through the box's own stack and is
> not an independent signal. Until an off-host probe exists, the
> availability row above is an objective, not a measurement — say so
> anywhere it is published.

### Why an API key is required

Without `STELLARINDEX_PROBE_API_KEY` set, the probe hits the
anonymous-tier rate limit — `[api].anon_rate_limit_per_min`, whose
shipped default is 60/min (R1 sets 6,000). At the documented 4
workers × 30 s window that's ~1000 requests/sec/worker — every
non-`/healthz` endpoint reads as `availability < 0.1 %` and the
verdict comes back `fail` for reasons unrelated to actual SLA
compliance. Mint a load-test API key from the operator vault (same
class as `STELLARINDEX_LOAD_API_KEY` for the k6 weekly) and set it
in `/etc/default/stellarindex-healthchecks` before enabling the timer. The probe
sends it as `Authorization: Bearer <key>` on every request — the
key never appears on the systemd unit's command line.

The defaults exercise XLM/USD as the smoke-test pair. Add `-pair`
entries to track additional asset/quote combinations the operator
cares about — each repeats the per-endpoint probe across the
chart, price, and oracle-latest surfaces for that pair.

## Which number is the latency SLO

**Read `stellarindex_sla_probe_latency_ms`, not a quantile over
`http_request_duration_seconds`.** They measure different things and
they disagree by orders of magnitude. Both are correct.

The served histogram is the only view of *real customer traffic*, and
its p50/p95 are meaningful. Its **p99 is not**, at current volume.
Production runs around **0.08 rps** — roughly 24 requests in a
5-minute window — so `histogram_quantile(0.99, …)` over that window is
computed from well under a hundred samples. In practice it reports the
single slowest request and calls it a percentile. One cold cache miss
moves it by seconds.

That is not a hypothetical. On 2026-09-01 the served p99 read 2,140 ms
while the probe measured `/price` at **19 ms p95** and `/assets` at
**13 ms p95** — both an order of magnitude inside the 200 ms target,
verdict `pass`, availability 100%. The gap was entirely cold-cache
first-hits landing in a nearly-empty sample window.

| question | query |
|---|---|
| Are we meeting the latency SLA? | `stellarindex_sla_probe_latency_ms{quantile="0.95"}` |
| Did the last probe run pass? | `stellarindex_sla_probe_unit_failed` (0 = pass) |
| What do real users see, typically? | `histogram_quantile(0.5\|0.95, sum by (le) (rate(http_request_duration_seconds_bucket[5m])))` |
| What was the slowest real request? | the served p99 — read it as a **max**, not a percentile |

The probe fixes the sample-size problem by construction: it drives
~150 samples per endpoint over 30s against a fixed basket, so its
percentiles are computed from enough data to mean something. Its
metrics live in their own namespace, so probe traffic never pollutes
the customer-traffic histogram — keep it that way. Synthetic load
emitted into `http_request_duration_seconds` would, at this traffic
level, become 99%+ of the samples and the dashboard would then be
describing the prober rather than the customers.

### A zeroed probe file usually means a restart, not a broken probe

If `sla_probe.prom` shows `0.000` for every latency, check
`stellarindex_sla_probe_availability_pct` **first**:

```sh
grep -E "availability_pct|unit_failed" \
  /var/lib/node_exporter/textfile_collector/sla_probe.prom
stat -c %y /var/lib/node_exporter/textfile_collector/sla_probe.prom
```

Availability `0.000` with `unit_failed 1` means every request failed —
so there were no successful samples to compute a latency from. The
overwhelmingly common cause is that the run landed during a deploy
while the API was restarting. Compare the file's mtime against the
deploy window before investigating the probe itself. The values stay
on the last run's result until the next tick (every 15 min), so a
mid-deploy failure looks current for up to a quarter of an hour.

The alert rules already tolerate this: `for: 30m` against a 15-minute
cadence needs two consecutive bad runs, so a single deploy-window
failure does not page.

## Reading the output

Each run logs its JSON report to the systemd journal:

```sh
sudo journalctl -u stellarindex-sla-probe.service -n 100 --output=cat | jq .
```

Key fields:

```json
{
  "base_url": "http://localhost:3000/v1",
  "started_at": "2026-04-30T12:00:00Z",
  "duration_sec": 30.0,
  "concurrency": 4,
  "sla": {
    "p95_ms": 200,
    "p99_ms": 500,
    "freshness_sec": 30,
    "availability_pct": 99.9
  },
  "per_endpoint": [
    {
      "endpoint": "price",
      "path": "/price",
      "samples": 120,
      "successes": 120,
      "availability_pct": 100.0,
      "latency_ms": {
        "p50": 12.0, "p95": 45.0, "p99": 78.0,
        "max": 102.0, "mean": 18.0
      },
      "observed_at_fresh_sec": 1.5
    }
    // … one entry per endpoint
  ],
  "verdict": "pass",
  "failed_reasons": []
}
```

A `verdict` of `fail` carries the reasons in `failed_reasons` —
e.g. `["price: p95=215.3ms > target 200.0ms"]`. The Healthchecks
wrapper at `/opt/stellarindex/healthchecks/sla-probe.sh` reports the
breach through three channels (F-1313, codex audit-2026-05-13):
1. POSTs the full JSON report body to `${HEALTHCHECKS_URL_SLA_PROBE}/fail`.
2. Writes `stellarindex_sla_probe_unit_failed 1` to the textfile-collector,
   which Prometheus surfaces as the `stellarindex_sla_probe_unit_failed_alert`.
3. The probe binary's stdout JSON lands in journald (`journalctl -u
   stellarindex-sla-probe.service`).

The wrapper itself **exits 0** even on probe failure so the timer's
"completed successfully" path stays clean for systemd; the breach is
detected via Healthchecks/Prometheus/journald, not systemd unit state.
`systemctl is-failed` will NOT report the breach — use the three channels
above.

## Pre-flight: spot-check from the operator's laptop

Before enabling the timer, run a single probe directly:

```sh
stellarindex-sla-probe \
  -base-url https://api.stellarindex.io/v1 \
  -duration 10s \
  -concurrency 2 \
  -report-format text
```

The text-format output is easier to scan during ad-hoc triage.
A clean dry-run with `verdict: pass` confirms the endpoint set,
the rate-limit headroom, and the freshness path all work end-to-
end before the cron starts hitting them. Run from a laptop this
*is* an edge measurement — TLS, DNS, Caddy and the network are all
in the path — which the scheduled on-host run is not.

## Textfile-collector integration

`-textfile-output PATH` writes a Prometheus textfile after each
run so node_exporter can scrape per-endpoint p50/p95/p99 latency,
availability, freshness, and a pass/fail gauge. Operator wiring:

```sh
# /etc/default/stellarindex-healthchecks
TEXTFILE_OUTPUT=/var/lib/node_exporter/textfile_collector/sla_probe.prom
```

The systemd service writes to that path via the
`<path>.tmp`-then-rename atomic protocol; node_exporter skips
files whose name ends in `.tmp` so a partial write never appears
in a scrape.

### Metric set

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

Four alerts in `deploy/monitoring/rules/sla-probe.yml`, each with a
runbook under `docs/operations/runbooks/sla-probe-*.md`:

| Alert | Condition | Severity |
|-------|-----------|----------|
| `stellarindex_sla_probe_p95_breach` | per-endpoint p95 > 200 ms sustained 30 min | **P2** page |
| `stellarindex_sla_probe_freshness_breach` | `price-tip` freshness > 30 s, or any other endpoint > 180 s, sustained 30 min | **P2** page |
| `stellarindex_sla_probe_unit_failed_alert` | overall verdict gauge = 1 sustained 30 min | P3 ticket |
| `stellarindex_sla_probe_stale` | `last_pass_timestamp` older than 90 min (6× 15-min cadence) | **P2** page |

## SLA targets in code

The probe's `slaTargets` struct mirrors the table at the top of
this doc. Defaults are baked in
(`cmd/stellarindex-sla-probe/main.go::default*Target`); operators
can tune them via flags if their deployment carries a different
contract (e.g. an internal staging environment with looser bars).
