---
title: Runbook — api-smoke-stale
last_verified: 2026-09-03
status: ratified
severity: P3
---

# Runbook — `stellarindex_api_smoke_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_api_smoke_stale` |
| Severity | P3 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/api-smoke.yml` (group `stellarindex.api_smoke`, `severity: ticket`, `for: 5m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/api-smoke.yml`. |
| Typical MTTR | 15 min |
| Impact | Nothing is asserting the public API's response shapes. The API may be perfectly healthy — this alert says we cannot tell. |

## Why this exists

A monitoring check that stops running looks exactly like a monitoring
check that keeps passing, unless something watches for its silence. The
smoke spent months in that state: `HEALTHCHECKS_URL_SMOKE` empty on r1,
no node_exporter textfile, no alert rule in either tree. It ran 34 GETs
with jq shape assertions every five minutes and the result reached no
system that could be queried.

`smoke.sh` now stamps
`stellarindex_api_smoke_last_run_unix` on every run, pass or fail, and
this alert fires when that stamp stops advancing — or was never written.

## Symptoms

- The full rule expr — note it has TWO branches:
  `(time() - stellarindex_api_smoke_last_run_unix) > 30*60 or absent_over_time(stellarindex_api_smoke_last_run_unix[30m])`
  for ≥ 5 min. 30 min is six missed firings at the 5-minute cadence.
- **OBS-2**: the never-scheduled cases — timer stopped, unit failing
  before it can write, textfile directory not writable, host never
  deployed — make the series **absent**, not old. An absent series is an
  empty vector, so the first branch cannot fire on any of them; the
  `absent_over_time` branch is what covers them.
- The first branch covers the opposite shape: node_exporter still
  re-serving a **frozen** `api_smoke.prom` whose timestamp never
  advances.

## Quick diagnosis (≤ 5 min)

```sh
ssh root@136.243.90.96

# 1. Is the timer scheduled, and when did it last fire?
systemctl list-timers stellarindex-smoke.timer
systemctl status stellarindex-smoke.timer

# 2. Is the unit running, or failing before it can write?
journalctl -u stellarindex-smoke.service --since "2 hours ago" -n 100

# 3. Is the textfile there, and how old is it?
ls -la /var/lib/node_exporter/textfile_collector/api_smoke.prom
cat /var/lib/node_exporter/textfile_collector/api_smoke.prom

# 4. Force one run and confirm the stamp advances.
systemctl start stellarindex-smoke.service
cat /var/lib/node_exporter/textfile_collector/api_smoke.prom
```

Which branch fired tells you where to look: a **present but old**
timestamp means the file is frozen (the unit is not completing, or the
directory went read-only after the last good write); an **absent series**
means nothing is being scraped at all.

## Typical root causes

1. **Timer disabled** — stopped for maintenance and not re-enabled.
   `systemctl status` shows `inactive`.
   - Mitigation: `sudo systemctl enable --now stellarindex-smoke.timer`.

2. **`ReadWritePaths` missing from the unit** — `ProtectSystem=strict`
   makes `/var` read-only, so the textfile write fails with EROFS and
   the series never appears while the smoke itself runs fine. The unit
   ships `ReadWritePaths=/var/lib/node_exporter/textfile_collector` plus
   `SupplementaryGroups=stellarindex`; a host that predates that, or one
   the archival-node role has not been applied to, has neither.
   - Signal: the journal shows the smoke output plus
     `smoke: WARN … not writable`.
   - Mitigation: re-apply the role
     (`ansible-playbook … archival-node.yml --tags healthchecks`).

3. **The collector directory does not exist** — provisioned by the
   archival-node role's `10-observability.yml`. On a fresh host that has
   not had the role applied, nothing under it is scraped.

4. **node_exporter not reading the textfile directory** — the file is
   present and fresh but the series is absent in Prometheus. Confirm
   node_exporter's `--collector.textfile.directory` flag. Every other
   `.prom` in that directory would be missing too, so check whether the
   sibling alerts (restore-drill, pgbackrest, sla-probe) also went quiet.

5. **The unit fails before the wrapper runs** — a bad `ExecStart` path
   after a partial deploy. The journal shows the unit failing, and no
   textfile is written at all.

## Mitigation

- [ ] Step 1 — Establish which branch fired (frozen file vs absent
      series) from step 3 of the diagnosis.
- [ ] Step 2 — Apply the matching fix from "Typical root causes".
- [ ] Step 3 — `systemctl start stellarindex-smoke.service` and confirm
      `last_run_unix` advances in the textfile.
- [ ] Verification: the alert clears within ~5 min of the first scrape
      that carries a fresh stamp.

## Known false-positive patterns

- **Fresh deploy** — on a host where the smoke has never run, the series
  is simply absent, so the `absent_over_time` branch fires until the
  first run lands (plus `for: 5m`). Fix by letting the timer fire or
  forcing a run, not by silencing: on a brand-new host that alert is
  telling the literal truth.
- **A single skipped firing** — `RandomizedDelaySec=30s` plus a busy host
  can stretch one interval. Six missed firings is the threshold, so this
  does not trip on jitter.

## Related

- `api-smoke-failing.md` — the companion alert for a smoke that IS
  running and found a problem. This one is about silence; that one is
  about a verdict.
- `configs/healthchecks/smoke.sh` — the wrapper that writes the metric.
- `configs/healthchecks/stellarindex-smoke.service` — the sandbox grants
  that let it write.
- `sla-probe-stale.md` — the same OBS-2 shape for the SLA probe; read it
  for the general "is the check itself alive" pattern.
- `data-freshness-watchdog-silent.md` — the frozen-textfile failure mode
  in a different emitter.

## Changelog

- 2026-09-03 — initial version, alongside the textfile metric that gave
  the smoke a queryable heartbeat.
