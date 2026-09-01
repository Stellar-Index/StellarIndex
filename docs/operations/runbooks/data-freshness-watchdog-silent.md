---
title: Runbook — data-freshness watchdog silent
last_verified: 2026-08-29
status: current
severity: P3
---

# Runbook — `stellarindex_data_freshness_watchdog_silent`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_data_freshness_watchdog_silent` |
| Severity | **P3** (ticket) |
| Detected by | `absent_over_time(stellarindex_data_freshness_stale[45m])` for > 15m |
| Emitted by | the absence of `data-freshness.sh`'s textfile output (`configs/ansible/roles/archival-node/files/data-freshness.sh` → `/usr/local/sbin/data-freshness.sh`) |
| Typical MTTR | 5–15 min |
| Impact | The watchdog that backstops every other source's staleness (coingecko, sep1, the completeness verdict) is itself blind — drift could now go unnoticed. Meta-alert. |

> **This alert can only see ABSENCE, not staleness.** The script builds the
> whole textfile in a temp file and swaps it in atomically at the very end. If a
> run dies before that swap, the PREVIOUS `data_freshness.prom` stays on disk
> and node_exporter keeps re-serving it verbatim — the series are present, just
> frozen, so `absent_over_time(...)` never trips. When you are triaging a
> "everything looks suspiciously green" report, check the file's mtime, not
> only the series:
>
> ```sh
> ls -la --time-style=full-iso /var/lib/node_exporter/textfile_collector/data_freshness.prom
> curl -s localhost:9100/metrics | grep -E 'node_textfile_(mtime_seconds|scrape_error)'
> ```

## Symptoms

The `stellarindex_data_freshness_*` series stop updating / go absent. The timer
runs every 15 min; absence > 45 min means it failed to fire or the script errors.

## Quick diagnosis (≤ 5 min)

```sh
systemctl status data-freshness.service data-freshness.timer
journalctl -u data-freshness.service --since '1 hour ago' | tail -30
ls -la --time-style=+%H:%M /var/lib/node_exporter/textfile_collector/data_freshness.prom
# Run it by hand to see the error:
/usr/local/sbin/data-freshness.sh
```

## Mitigation (≤ 15 min)

- **Timer disabled/not loaded:** `systemctl enable --now data-freshness.timer`.
- **Script errors (psql/DSN):** the script reads `/etc/default/stellarindex`
  **verbatim** for `STELLARINDEX_POSTGRES_DSN` — it deliberately never `.`/
  `source`s it, because the file is a systemd EnvironmentFile with unquoted
  values and the shell parser would expand `$`, split on `;` and eat quotes
  inside the password (deploy-ansible-secrets-5, pinned by
  `scripts/ci/envfile-loader-test.sh`). **Do not "fix" it by sourcing the
  file.** A Postgres outage or DSN drift breaks the run — fix the DSN/DB, then
  `systemctl start data-freshness.service`.
- **`psql` not found / wrong cluster:** the script calls the versioned binary
  directly, `/usr/lib/postgresql/${PG_VERSION:-15}/bin/psql`, to bypass
  Debian's `pg_wrapper` (which stats the cluster data dir and aborts for the
  unprivileged `stellarindex` user). After a Postgres MAJOR upgrade that path
  no longer exists: set `PG_VERSION=<major>` in `/etc/default/data-freshness`
  and restart the unit.
- **Textfile unreadable (0600):** node_exporter is unprivileged; the script
  chmods the file 0644 before the atomic swap — if a stale 0600 file lingers,
  `chmod 0644` it.

## Root cause analysis

A oneshot timer/script that stopped: timer not enabled after a rebuild, a DB
outage failing a query under `set -euo pipefail`, or a permissions regression
on the textfile.

**Known root cause, fixed 2026-08-29 (Wave L, #319).** The script's last
producer is a ClickHouse HTTP probe on `:8123` for `stellar.supply_flows`. It
was written as a bare `SF_AGE=$(curl … | tr …)` assignment, which under
`set -euo pipefail` takes curl's exit status — so a ClickHouse outage aborted
the run at that line, the EXIT trap deleted the temp file, the atomic swap
never happened, and every gauge froze at its last value (see the banner above:
frozen is invisible to this alert). The probe is now best-effort: it runs as an
`if` condition, logs `data-freshness: ClickHouse supply_flows probe failed —
sep41_supply gauges skipped this tick` to the journal, and only the two
`domain="sep41_supply"` gauges are omitted. On a host that has not been
redeployed since, the old behaviour is still live — check the script on disk.

## Known false-positive patterns

- A node_exporter restart briefly drops textfile metrics until the next scrape —
  the `for: 15m` absorbs that.

## Related

- `stellarindex_data_source_stale`, `stellarindex_completeness_incomplete` — the alerts this watchdog feeds ([data-source-stale](data-source-stale.md), [completeness-incomplete](completeness-incomplete.md)).
- `stellarindex_supply_assets_stale`, `stellarindex_twap_history_missing`,
  `stellarindex_recognition_unattributed_shapes`, `stellarindex_recognition_ok`
  — the other gauges this one script emits; they all go dark together.
- `stellarindex_ingest_gap_detector_silent` — the analogous meta-alert for the gap detector.
- `scripts/ci/data-freshness-test.sh` — the fixture test that pins the
  best-effort ClickHouse probe.

## Changelog

- 2026-08-29 — re-verified against HEAD (runbook Wave L, #319): the script
  reads `/etc/default/stellarindex` VERBATIM and must never be "fixed" by
  sourcing it; added the `PG_VERSION` note for the versioned psql path; added
  the frozen-vs-absent banner (this alert cannot see a stale-but-present
  textfile) with the mtime check; recorded the ClickHouse-probe root cause and
  its fix.
- 2026-06-30: created with the data-freshness watchdog.
