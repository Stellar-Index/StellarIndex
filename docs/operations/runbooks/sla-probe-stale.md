---
title: Runbook — sla-probe-stale
last_verified: 2026-08-28
status: ratified
severity: P2
---

# Runbook — `stellarindex_sla_probe_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_sla_probe_stale` |
| Severity | P2 (`severity: page`) |
| Detected by | `configs/prometheus/rules.r1/sla-probe.yml` (group `stellarindex.sla_probe`, `severity: page`, `for: 5m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/sla-probe.yml`. |
| Typical MTTR | 15 min |
| Impact | We've lost the SLA-evidence trail. The API itself may be fine — this alert says we can't *prove* it. |

## Symptoms

- The full rule expr — note it has TWO branches:
  `(time() - stellarindex_sla_probe_last_pass_timestamp) > 90*60 or absent_over_time(stellarindex_sla_probe_last_pass_timestamp[90m])`
  for ≥ 5 min.
- OBS-2: the probe writes `last_pass_timestamp` **only on a pass**
  and rewrites the whole textfile each run — so a failing run drops
  the series entirely, and the `absent_over_time` branch is what
  catches every-run-failed.
- Either the systemd timer isn't running, or every recent run has
  failed (which would also fire `_unit_failed_alert`).
- Don't confuse the live `stellarindex-sla-probe.timer` with the
  retired duplicate GO-stack `sla-probe.timer` — ansible actively
  removes the latter.

## Quick diagnosis (≤ 5 min)

```sh
ssh root@136.243.90.96

# 1. Is the timer scheduled?
systemctl status stellarindex-sla-probe.timer
systemctl list-timers stellarindex-sla-probe.timer

# 2. When did the unit last run?
journalctl -u stellarindex-sla-probe.service --since "2 hours ago" -n 50

# 3. Is the textfile being written?
ls -la /var/lib/node_exporter/textfile_collector/sla_probe.prom

# 4. Force a one-off run, then read the verdict. The wrapper POSTs
# the JSON report to Healthchecks.io, NOT journald — read the
# textfile, or run the binary directly for a JSON report:
systemctl start stellarindex-sla-probe.service
cat /var/lib/node_exporter/textfile_collector/sla_probe.prom
/usr/local/bin/stellarindex-sla-probe -base-url http://localhost:3000/v1 \
  -pair native,fiat:USD -duration 30s -concurrency 1 -report-format json | jq .
```

## Typical root causes

1. **Timer disabled** — operator ran `systemctl stop stellarindex-sla-probe.timer`
   for maintenance and forgot to re-enable. `systemctl status` shows
   `inactive`.
   - Mitigation: `sudo systemctl enable --now stellarindex-sla-probe.timer`.

2. **Service unit failing every run** — fires alongside this alert
   in journald.
   - Mitigation: check the journald entries; route to the
     `_unit_failed_alert` runbook.

3. **node_exporter not scraping the textfile_collector dir** — the
   probe runs and writes the file, but Prometheus never sees the
   metric. Under OBS-2 this signal is ambiguous — disambiguate with
   `grep last_pass /var/lib/node_exporter/textfile_collector/sla_probe.prom`:
   - Line **present** in the file but the series **absent** in
     Prometheus → node_exporter / textfile-collector problem;
     confirm node_exporter's `--collector.textfile.directory` flag
     points at the right path.
   - Line **absent** from a FRESH file → the probe is failing every
     run (check `stellarindex_sla_probe_unit_failed=1` in the same
     file) → route to `sla-probe-unit-failed.md`.

4. **`SLA_PROBE_TEXTFILE_OUTPUT` explicitly blanked** — the wrapper
   DEFAULTS the textfile path (`SLA_PROBE_TEXTFILE_OUTPUT`,
   `configs/healthchecks/sla-probe.sh:54`), so an absent env var
   still writes; only an explicitly emptied
   `SLA_PROBE_TEXTFILE_OUTPUT=` in
   `/etc/default/stellarindex-healthchecks` disables the write.
   (`TEXTFILE_OUTPUT` is a different variable belonging to the
   supply-snapshot unit.) The other real file-missing cause is the
   `ReadWritePaths`/EROFS class the unit itself documents — the
   service can't write outside its allowed paths.
   - Signal: file doesn't exist at all.
   - Mitigation: un-blank the var (or fix `ReadWritePaths`); reload
     the service.

## Mitigation

- [ ] Step 1 — Walk the diagnostic commands; identify which stage
      is silent.
- [ ] Step 2 — Apply the matching fix from "Typical root causes."
- [ ] Step 3 — Force a probe run via
      `systemctl start stellarindex-sla-probe.service` and confirm
      `last_pass_timestamp` updates.
- [ ] Verification: alert clears within 5 min after a successful
      probe run lands in node_exporter.

## Known false-positive patterns

- **Fresh deploy** of the probe — the gauge has never been set. The
  series is simply absent, so the first branch can't fire ("time()-0"
  is mechanically wrong: an absent series is an empty vector) — the
  fresh-deploy FP comes from the `absent_over_time` branch, which
  fires until the first PASSING run lands (plus `for: 5m`). Fix by
  forcing a run (`systemctl start stellarindex-sla-probe.service`)
  rather than silencing.

## Related

- `sla-probe-unit-failed.md` — when the probe is running but
  failing.
- `sla-probe-p95-breach.md` / `sla-probe-freshness-breach.md` —
  specific failure modes.

## Changelog

- 2026-08-28 — re-verified against HEAD. Symptom now quotes BOTH
  rule branches and documents OBS-2 (last_pass_timestamp written only
  on pass; whole-file rewrite ⇒ failing runs drop the series — the
  absent branch catches every-run-failed). Root cause 4 rewritten:
  the wrapper defaults `SLA_PROBE_TEXTFILE_OUTPUT`
  (sla-probe.sh:54) — absent env still writes; only an explicit
  blank disables (`TEXTFILE_OUTPUT` belongs to the supply-snapshot
  unit); ReadWritePaths/EROFS added. Root cause 3 disambiguated
  under OBS-2 via a grep of the textfile. False-positive section
  corrected (absent series ≠ time()-0; the never-implemented
  `unless unit_failed` guard claim dropped). Forced-run check reads
  the textfile / runs the binary one-off (the wrapper swallows the
  JSON). Rule citation → `rules.r1/sla-probe.yml`; commands use r1
  shapes; retired GO-stack `sla-probe.timer` disambiguation noted.
- 2026-04-30 — initial draft alongside #294 (alert rules).
