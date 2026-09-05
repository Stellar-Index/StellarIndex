---
title: Runbook — timescale-probe-degraded
last_verified: 2026-09-05
status: current
severity: P2
---

# Runbook — `stellarindex_timescale_probe_degraded`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_timescale_probe_degraded` |
| Severity | P2 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/storage.yml` (group `stellarindex.storage`, `severity: ticket`, `for: 15m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/storage.yml`. **Producer:** `timescale-jobs-probe.timer` (every 60 s), installed by `configs/ansible/roles/archival-node/tasks/10-observability.yml`, publishes its own state alongside its data: `stellarindex_timescale_probe_query_ok{query}`, `stellarindex_timescale_probe_rows{query}` and `stellarindex_timescale_probe_last_run_unix`. |
| Typical MTTR | 15–30 min |
| Impact | No direct customer impact. Three storage alerts — [cagg-stale](cagg-stale.md), [compression-lag](compression-lag.md) and [timescale-job-failures-climbing](timescale-job-failures-climbing.md) — read one textfile from this probe, and while it is degraded their silence means nothing either way. |

## Why this exists

Everything downstream of `/var/lib/node_exporter/textfile_collector/timescale_jobs.prom`
fails quiet. `>` and `time() - x` over an empty vector are empty, so an
alert whose series went absent does not fire — it goes blind, and blind
looks exactly like healthy on every dashboard.

Three ways the producer could reach that state without saying a word:

- its psql helper ended in `2>/dev/null || true`, so a failing query
  returned an empty result **and** exit 0. The file was rewritten
  without that query's families and node_exporter served it;
- a query that exits 0 and returns **no rows** drops the same series
  without raising a status at all — a renamed `timescaledb_information`
  view, or a filter (`proc_name`, a `config` key) that stopped matching.
  The compression family emits one row per policy *including zeros*
  precisely so that absence means "probe stopped", which makes zero rows
  the blind state rather than a quiet one;
- the script not running leaves node_exporter serving the **last file it
  saw**, indefinitely. The series stay present and get a fresh sample
  timestamp on every scrape while their values freeze — the same shape
  [`stellarindex_config_assertions_stale`](config-assertion-failed.md)
  was written for.

So the probe now reports its own state on every run and this alert reads
it. Each arm maps to one of those shapes.

## Symptoms

- `stellarindex_timescale_probe_query_ok{query=…} == 0` — that query
  errored. The alert names it in the summary.
- `stellarindex_timescale_probe_rows{query=…} == 0` — that query
  succeeded and returned nothing.
- `time() - stellarindex_timescale_probe_last_run_unix > 600` — the file
  has not been rewritten for ten ticks. The per-query metrics beside it
  will look perfectly healthy; they are frozen.
- The series are absent entirely — the probe has never written the file
  on this host. The summary carries no `query` label in this case.

## Quick diagnosis (≤ 5 min)

```sh
ssh root@136.243.90.96

# Is the timer alive, and did the last run succeed?
systemctl status timescale-jobs-probe.timer --no-pager
systemctl status timescale-jobs-probe.service --no-pager
journalctl -u timescale-jobs-probe.service --since -30min --no-pager

# Is the file fresh? mtime should be < 2 min old.
ls -l --time-style=full-iso /var/lib/node_exporter/textfile_collector/timescale_jobs.prom

# What does the probe say about itself?
grep stellarindex_timescale_probe_ \
  /var/lib/node_exporter/textfile_collector/timescale_jobs.prom

# Run it by hand — this is the fastest way to see the psql error the
# probe suppresses.
bash -x /usr/local/sbin/timescale-jobs-probe.sh
```

If a query is the problem, run it directly and read the error:

```sh
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT count(*) FROM timescaledb_information.jobs WHERE proc_name = 'policy_compression';"
```

## Typical root causes

1. **The role was never applied.** The rule files auto-deploy
   (`configs/prometheus/apply-rules.sh`, run by the deploy workflow);
   the ansible-managed probe does not. A rule that reads a metric a
   newer producer emits will alert until the archival-node role is
   applied — `ansible-playbook … --tags observability --check --diff`
   first. This is the `codified is not applied` class, and it is the
   most likely cause the first time this alert appears.
2. **Postgres unreachable or peer auth broken.** All three queries fail
   together, all three `query_ok` read 0, and the stamp stays fresh.
   `stellarindex_timescale_primary_down` should be firing beside this.
3. **A renamed or dropped `timescaledb_information` view or column**
   after a TimescaleDB major upgrade. One query fails or returns
   nothing; the others are fine. Fix the SQL in the role, not on the
   host — the file under `/usr/local/sbin` is overwritten on every
   apply.
4. **A filter that stopped matching.** `query_ok` is 1 and `rows` is 0:
   the SQL is valid, the data it selects no longer exists. Check that
   the policies really are there before assuming the probe is wrong —
   a hypertable that genuinely lost its compression policy is
   `compression_policies_applied` in `scripts/ops/config-assertions.sh`.
5. **The timer stopped or the unit is masked.** `last_run_unix` ages
   while everything else looks healthy.
6. **A full disk.** `mv` fails, the previous file stays in place, and
   the stamp ages. `stellarindex_timescale_disk_full` fires alongside.

## Mitigation

- [ ] Step 1 — read the probe's own metrics to pick the arm (above).
- [ ] Step 2 — for a query failure, run that query as `postgres` and
      read the error text; the probe discards stderr by design.
- [ ] Step 3 — for a stale stamp, `systemctl start
      timescale-jobs-probe.service` and re-check the file's mtime.
- [ ] Step 4 — fix the cause in
      `configs/ansible/roles/archival-node/tasks/10-observability.yml`
      and apply the role; never hand-edit `/usr/local/sbin`.
- [ ] Verification: every `stellarindex_timescale_probe_query_ok` reads
      1, every `stellarindex_timescale_probe_rows` is non-zero, and
      `time() - stellarindex_timescale_probe_last_run_unix` stays under
      60. The alert clears within 15 min.

## Root cause analysis

- `journalctl -u timescale-jobs-probe.service` for the whole degraded
  window. The unit exits 0 on a partial run by design, so a failure
  leaves no unit-level trace — the metrics are the record.
- Which of the three downstream alerts were blind, and for how long.
  `stellarindex_timescale_probe_rows` per query is the evidence: a
  family with zero rows over a window is a family no alert could read.
- If a TimescaleDB upgrade preceded it, the release notes for the
  `timescaledb_information` schema.

## Known false-positive patterns

- **A host with no TimescaleDB.** The probe is installed with the rest
  of observability (`run_observability`), not with Postgres
  (`run_postgres`), so a host configured without a database would
  report all three queries failing. No inventory does that today; if one
  ever should, remove the unit rather than muting the alert.
- **A brand-new host between database creation and migration.** The
  queries succeed and return nothing until the caggs and compression
  policies exist. `for: 15m` does not cover a long bring-up; this is
  expected noise during a build and clears with the first migration.
- **Not a false positive: a partial file.** One failing query still
  writes the other two families on purpose — dying instead would leave
  the previous file on disk to be re-scraped forever. A partial file
  with an honest `query_ok 0` is the designed outcome, not a bug.

## Related

- [cagg-stale](cagg-stale.md), [compression-lag](compression-lag.md) and
  [timescale-job-failures-climbing](timescale-job-failures-climbing.md) —
  the three alerts that read this producer. This runbook is the
  producer side of each of their "is the probe alive?" checks.
- [config-assertion-failed](config-assertion-failed.md) — the same
  monitoring-of-monitoring shape one layer out, and the alert that
  catches codified-but-not-applied config.
- Producer:
  `configs/ansible/roles/archival-node/tasks/10-observability.yml`
  (task `TimescaleDB job/CAGG health probe (script)`).
- Fixtures: `scripts/ci/timescale-jobs-probe-test.sh`;
  alert cases in `deploy/monitoring/rule-tests/storage_test.yml`.

## Changelog

- 2026-09-05 — initial version, with the alert.
