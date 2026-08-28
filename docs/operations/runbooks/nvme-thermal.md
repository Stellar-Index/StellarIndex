---
title: Runbook — nvme-thermal
last_verified: 2026-08-28
status: current
severity: P2
---

# Runbook — `stellarindex_nvme_thermal_throttle`

> **INERT ALERT (re-verified 2026-08-28).** This alert **cannot fire
> today**: its expr watches `node_nvme_temperature_celsius`, a metric
> **nothing produces** — the rule carries a `NO PRODUCER` / `INERT`
> comment in `deploy/monitoring/rules/infra.yml` and the metric is
> listed in `scripts/ci/lint-metric-refs.sh`'s `KNOWN_INERT` set.
> Thermal problems currently surface only via the manual
> `smartctl`/`sensors` checks below, or downstream as latency.
> Un-inerting: the packaged node-exporter-collectors nvme probe
> already emits real `nvme_*` series on r1
> (`nvme_percentage_used_ratio` etc. feed the live wear alerts in
> `rules.r1/storage.yml`); if it also emits
> `nvme_temperature_celsius` on r1, repoint the expr at that series
> and drop the `KNOWN_INERT` entry.
> TODO(ash): verify on r1 whether the nvme collector emits
> `nvme_temperature_celsius`; if yes, repoint the expr in BOTH rule
> trees and remove the `KNOWN_INERT` entry, then delete this TODO.

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_nvme_thermal_throttle` — **inert, see banner** |
| Severity | P2 (`severity: ticket`) — if it could fire |
| Detected by | `configs/prometheus/rules.r1/infra.yml` (group `stellarindex.infra`, `severity: ticket`, `for: 5m`); multi-host twin in `deploy/monitoring/rules/infra.yml`. Both carry the `INERT: node_nvme_temperature_celsius has no producer` comment. |
| Typical MTTR | 15 min – days (airflow fix vs waiting for cooler weather) |
| Impact | NVMe IO throttles to ~50 % of rated when over thermal limit. Postgres WAL flush slows, captive-core catchup slows, aggregator write throughput drops. Sustained thermal stress also accelerates drive wear — not immediate but not free. |

## Symptoms

- (Would be) `node_nvme_temperature_celsius > 70` sustained 5 min on
  some device — but see the banner: no series exists, so in practice
  you arrive here from manual checks or downstream symptoms.
- Write IOPS / throughput drop visibly when temperature crosses
  threshold (compare IO panels with the manual temps below).
- Latency alerts may follow (`api-latency.md`) if the throttle is
  heavy.

## Quick diagnosis (≤ 5 min)

```sh
# Per-drive temps (r1 = 4× NVMe in the `data` pool)
ssh root@136.243.90.96 'for d in /dev/nvme?n1; do
  echo -n "$d: "; smartctl -A "$d" | grep -i temperature
done'

# Is it all drives (chassis issue) or one (drive issue)?
# If all, check ambient temperature + fans.
ssh root@136.243.90.96 'sensors | grep -E "Composite|fan"'

# IPMI fan speeds (if the BMC is reachable)
ssh root@136.243.90.96 'ipmitool sdr list | grep -iE "fan|temp"'
```

## Typical root causes

1. **Chassis airflow issue.** Clogged intake filter, failed fan,
   misrouted cables blocking flow. Usually affects multiple drives
   simultaneously.

2. **Ambient datacentre temperature.** Summer, HVAC issue, door
   propped open. All drives on affected hosts climb together.

3. **Single drive issue.** Heatsink came loose, drive in a
   poorly-ventilated slot. Only one drive on a host climbs while
   neighbours stay cool.

4. **Sustained heavy workload** — scrub, resilver, backup, mass
   compaction. Expected during these operations; the throttle
   does its job.

## Mitigation

- [ ] Step 1 — is it all drives on the host or one drive? That
      identifies the scale.
- [ ] Step 2 — immediate: reduce the workload. Pause pgBackRest,
      cancel any running scrubs, let the drive cool.
- [ ] Step 3 — medium-term: open a Hetzner support ticket for
      remote-hands to check fans / airflow; replace failed fans;
      clean dust filters (r1 is a Hetzner FSN1 dedicated server —
      there is no "our NOC").
- [ ] Step 4 — if ambient is the cause: escalate to Hetzner support;
      nothing you can do at 3 AM to change a DC's cooling.
- [ ] Verification: temperature drops below 65 °C sustained for
      15 min; write throughput returns to baseline.

## Known false-positive patterns

- **Brief spikes during scrub / resilver** — expected, should
  stay under threshold if the chassis is cooled correctly.
- **Sensor reporting glitches** — a single drive showing 85 °C
  while its neighbours are 45 °C can be a sensor read error.
  `smartctl -x` shows historical temperatures; cross-check.

## Related

- `nvme-smart.md` — thermal stress accelerates the SMART wear
  indicators. Note that the three **live** NVMe alerts —
  `stellarindex_nvme_wear_high`, `stellarindex_nvme_spare_low`,
  `stellarindex_nvme_media_errors` (`rules.r1/storage.yml`, fed by
  the packaged nvme collector) — are the working early-warning
  substitutes while this thermal alert is inert.
- `db-disk-full.md`, `compression-lag.md` — downstream effects
  when thermal throttle slows writes.

## Changelog

- 2026-08-28 — re-verified against HEAD. Added the INERT-ALERT banner:
  `node_nvme_temperature_celsius` has no producer (KNOWN_INERT in
  `scripts/ci/lint-metric-refs.sh`; NO PRODUCER comment in the rule
  files), with the un-inerting path left as TODO(ash). Commands
  rehosted to `ssh root@136.243.90.96`; "colo's NOC" → Hetzner
  support (r1 is Hetzner FSN1). Related: swapped `replica-lag.md`
  (no replica) for `db-disk-full.md` and named the three live NVMe
  wear alerts as the working substitutes. Rule citation →
  `rules.r1/infra.yml`.
- 2026-04-23 — initial draft.
