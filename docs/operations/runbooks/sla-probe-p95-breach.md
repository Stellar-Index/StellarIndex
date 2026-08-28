---
title: Runbook — sla-probe-p95-breach
last_verified: 2026-08-28
status: ratified
severity: P2
---

# Runbook — `stellarindex_sla_probe_p95_breach`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_sla_probe_p95_breach` |
| Severity | P2 (`severity: page`) |
| Detected by | `configs/prometheus/rules.r1/sla-probe.yml` (group `stellarindex.sla_probe`, `severity: page`, `for: 30m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/sla-probe.yml`. |
| Typical MTTR | 15–60 min |
| Impact | Detail-page consumers see degraded responsiveness on the named endpoint. Direct SLA breach (target: p95 ≤ 200 ms). |

## Symptoms

- `stellarindex_sla_probe_latency_ms{endpoint=…,quantile="0.95"} > 200`
  for ≥ 30 min (2 timer firings).
- The probe's most-recent report (the textfile the alerts scrape, or
  the Healthchecks.io ping body — the JSON never reaches journald)
  carries `failed_reasons: ["<endpoint>: p95=<N>ms > target 200.0ms"]`.
- Direct-API alert `stellarindex_api_latency_p95_high` may also be
  firing — they're complementary signals (probe = synthetic;
  histogram = real traffic).

## Quick diagnosis (≤ 5 min)

```sh
# 1. Get the most-recent probe verdict.
# The wrapper posts the JSON report to Healthchecks.io, NOT journald
# (configs/healthchecks/sla-probe.sh:80-106 captures stdout and only
# POSTs it). Read the last verdict from the textfile the alerts scrape:
ssh root@136.243.90.96 cat /var/lib/node_exporter/textfile_collector/sla_probe.prom
# or the Healthchecks.io check body, or run a one-off report:
/usr/local/bin/stellarindex-sla-probe -base-url http://localhost:3000/v1 \
  -pair native,fiat:USD -duration 30s -concurrency 1 -report-format json | jq .failed_reasons

# 2. Confirm direct-traffic histograms agree (rules out probe-only artefacts).
# (Prometheus listens on localhost on r1.)
curl -s http://localhost:9090/api/v1/query --data-urlencode \
  'query=histogram_quantile(0.95, sum by (route, le) (rate(http_request_duration_seconds_bucket{job=~"stellarindex[_-]api"}[5m])))' | \
  jq -r '.data.result[] | "\(.metric.route): \(.value[1])s"' | sort -k2 -rn | head

# 3. Run a one-off probe locally to see if it's regional / network.
# Keep -concurrency 1: at -concurrency 2 the probe self-saturates the
# API (F-1305: ~2.5k req/s, p95→3.8s) and measures its own load. And
# authenticate — a keyless run hits the anonymous 60/min limit and
# 429-fake-fails (F-1311, the 2026-08-24 GO-stack retirement shape);
# a keyless run reads as an AVAILABILITY fail, not a latency signal.
export STELLARINDEX_PROBE_API_KEY=<key>   # or pass -api-key
stellarindex-sla-probe -base-url https://api.stellarindex.io/v1 \
  -duration 10s -concurrency 1 -report-format text
```

## Typical root causes (roughly in frequency order)

Same as `api-latency.md` — the probe and the direct-traffic
histograms see the same backend. The probe just adds:

1. **Probe-host network path issue.** If only the probe is slow but
   the histogram is fine, the issue is between the probe runner and
   the API edge. Confirm by running the probe from a different
   network.
2. **Endpoint-specific slowness** — if `/v1/price` is fine but
   `/v1/oracle/latest` breaches, the cause is on that endpoint's
   path (e.g. SEP-40 contract read latency).
3. Everything from `api-latency.md` ("Typical root causes").

## Mitigation

- [ ] Step 1 — Confirm the breach is real (not probe-host-only) per
      Quick diagnosis #2.
- [ ] Step 2 — If probe-only: investigate the probe host's network
      path; this is a probe issue, not a real-impact alert.
- [ ] Step 3 — If real: route to `api-latency.md` for the full
      latency-triage flow.
- [ ] Verification: probe p95 returns under 200 ms for 30 min
      (2 consecutive passes — the alert clears with same `for`
      threshold).

## Known false-positive patterns

- **First scrape after a deploy** — fresh process, cold Redis +
  Timescale buffers. The 30-min `for` window absorbs this.
- **Probe host pinned at 100% CPU** during another job. Confirm
  via the probe host's own metrics.

## Related

- `api-latency.md` — the underlying latency-triage flow.
- `sla-probe-stale.md` — when the probe stops running entirely.

## Changelog

- 2026-08-28 — re-verified against HEAD. Diagnosis #1 replaced: the
  probe's JSON never reaches journald — the wrapper captures stdout
  and only POSTs to Healthchecks.io (sla-probe.sh:80-106); read the
  textfile / Healthchecks.io body or run the binary one-off. One-off
  probe guidance: `-concurrency 1` (F-1305 self-saturation) + an API
  key (F-1311 keyless-429 fake-fails). Rule citation →
  `rules.r1/sla-probe.yml`; Prometheus queries use the
  localhost-on-r1 shape.
- 2026-04-30 — initial draft alongside #294 (alert rules).
