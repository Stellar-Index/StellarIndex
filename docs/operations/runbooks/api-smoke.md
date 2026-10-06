---
title: Runbook — api-smoke alerts
last_verified: 2026-10-05
status: draft
---
# API smoke alerts

Alerts from `configs/prometheus/rules.r1/api-smoke.yml` (group `stellarindex.api_smoke`, `severity: ticket`; multi-host twin `deploy/monitoring/rules/api-smoke.yml`). The smoke (`scripts/dev/r1-smoke.sh`, wrapper `configs/healthchecks/smoke.sh`, unit `stellarindex-smoke.service`) is the only check that reads response bodies; it runs every 5 min and writes `/var/lib/node_exporter/textfile_collector/api_smoke.prom`. `_failing` means it ran and found a problem; `_stale` means it did not report. Work them separately.

## At a glance

- [`stellarindex_api_smoke_failing`](#stellarindex_api_smoke_failing)
- [`stellarindex_api_smoke_stale`](#stellarindex_api_smoke_stale)

## stellarindex_api_smoke_failing

**Severity** P3. MTTR 30 min. Impact: a launch-critical endpoint answers with the wrong shape or status.
**Trigger** `stellarindex_api_smoke_failures > 0` for 30m (six consecutive failing runs). The gauge is the smoke's exit code = failed-check count (34 checks), rewritten each run, so it clears within one cadence of a clean run. The 30m `for` is deliberate: some checks are data-dependent (`/v1/ohlc`, `/v1/oracle/prices` accept a documented 404 on an empty window; cold-cache responses can approach the 10 s per-request budget).

**Diagnose** (5 min)

```sh
ssh root@136.243.90.96

# 1. Which checks are failing? Each failing check prints its own line.
journalctl -u stellarindex-smoke.service --since "1 hour ago" -n 200

# 2. Current verdict as Prometheus sees it.
cat /var/lib/node_exporter/textfile_collector/api_smoke.prom

# 3. Reproduce on demand (read-only, safe to re-run).
API_BASE_URL=http://localhost:3000 \
  bash /opt/stellarindex/healthchecks/r1-smoke.sh; echo "failed: $?"

# 4. Same surface from outside, to separate "API is wrong" from "loopback path is wrong" (Caddy, TLS, host routing).
API_BASE_URL=https://api.stellarindex.io bash scripts/dev/r1-smoke.sh
```

**Root causes**

1. Handler changed shape (serialiser field renamed/dropped; jq assertion fails on a healthy 200). The failing check names the endpoint. Fix forward, or roll the API binary back per `docs/operations/deploy-config-apply.md`; prefer rollback during the window.
2. A documented 4xx regressed to 200 (the `expect_status` pins). Treat as an API contract break, not a smoke bug.
3. Deploy landed the binary without its config (ansible-rendered config missing; endpoint answers pre-feature shape). Check `config-apply-gate` on the deploy run and whether the archival-node role was applied.
4. Deployed `r1-smoke.sh` differs from the repo. The archival-node role copies it (`17-stellarindex-healthchecks.yml`); `diff scripts/dev/r1-smoke.sh` against `/opt/stellarindex/healthchecks/r1-smoke.sh`.
5. Dependency down (Postgres, Redis, ClickHouse): many checks fail at once. Work `api-5xx` / `api-down` first; this alert is downstream.

**False positives** A single slow cold-cache run (`/v1/markets?limit=5` has taken 6-8 s vs 10 s budget); checks accepting `200|404` tolerate a quiet pair, but a 404 outside that set is a real break; a run during a deploy (API restarts mid-run, check the deploy timeline).

**Verify** Re-run manually to confirm the failure is current; after the fix `stellarindex_api_smoke_failures` returns to 0 at the next timer firing (5 min).

Related: `sla-probe.md#stellarindex_sla_probe_unit_failed_alert` (latency/freshness counterpart), `healthcheck-ping.md` (read before trusting a Healthchecks.io "down" email for this check).

## stellarindex_api_smoke_stale

**Severity** P3. MTTR 15 min. Impact: nothing asserts the public API's shapes; the API may be healthy, we cannot tell.
**Trigger** for 5m:
`(time() - stellarindex_api_smoke_last_run_unix) > 30*60 or absent_over_time(stellarindex_api_smoke_last_run_unix[30m])`
30 min = six missed firings. The first branch covers a frozen `api_smoke.prom` still re-served by node_exporter (stamp present but old). The `absent_over_time` branch covers never-scheduled cases (timer stopped, unit failing before it writes, textfile dir not writable, host never deployed): the series is absent, not old, so the first branch cannot fire.

**Diagnose** (5 min)

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

Present-but-old timestamp = frozen file (unit not completing, or directory went read-only after the last write). Absent series = nothing is being scraped.

**Root causes**

1. Timer disabled (`systemctl status` shows `inactive`): `sudo systemctl enable --now stellarindex-smoke.timer`.
2. `ReadWritePaths` missing from the unit: `ProtectSystem=strict` makes `/var` read-only so the textfile write fails (EROFS) while the smoke runs fine. The unit should ship `ReadWritePaths=/var/lib/node_exporter/textfile_collector` and `SupplementaryGroups=stellarindex` (`configs/healthchecks/stellarindex-smoke.service`). Journal shows `smoke: WARN … not writable`. Re-apply the role: `ansible-playbook … archival-node.yml --tags healthchecks`.
3. Collector directory missing (provisioned by archival-node role `10-observability.yml`; fresh host without the role).
4. node_exporter not reading the directory (file present and fresh, series absent). Check `--collector.textfile.directory`; sibling alerts (restore-drill, pgbackrest, sla-probe) would also go quiet.
5. Unit fails before the wrapper runs (bad `ExecStart` path after a partial deploy); journal shows the failure, no textfile written.

**Fix** Identify the branch (frozen vs absent), apply the matching fix, `systemctl start stellarindex-smoke.service` and confirm `last_run_unix` advances. Alert clears ~5 min after the first scrape with a fresh stamp.

**False positives** Fresh deploy: series absent until the first run lands (plus `for: 5m`); force a run, do not silence. One skipped firing (`RandomizedDelaySec=30s`) does not trip the six-miss threshold.

Related: `sla-probe.md#stellarindex_sla_probe_stale`, `data-freshness.md#stellarindex_data_freshness_watchdog_silent` (frozen-textfile pattern), `healthcheck-ping.md` (the other reason Healthchecks.io goes quiet).

## Related

- [api](api.md): API-side alerts.
