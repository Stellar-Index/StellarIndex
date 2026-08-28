---
title: Runbook — node-root-disk-full
last_verified: 2026-08-28
status: current
severity: P1
---

# Runbook — `stellarindex_node_root_disk_full`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_node_root_disk_full` |
| Severity | **P1** (page) |
| Detected by | `configs/prometheus/rules.r1/storage.yml` (the single-host overlay r1 actually loads); multi-host twin in `deploy/monitoring/rules/storage.yml`. NOTE: at HEAD both rule files' `runbook_url` for this alert still point at `redis-write-blocked-disk-full.md`, so the page will NOT link here — come via `docs/operations/alerts-catalog.md`. |
| Typical MTTR | 15–60 min |
| Impact | The host's root filesystem is < 10 % free. Cascading failures can follow within **minutes**, not hours: Redis BGSAVE blocks (every cache write returns MISCONF) → `/v1/price` 404s on every rewritten/triangulated/stablecoin-proxy pair; **Postgres crashes** — its log lives on `/var/log/postgresql` (root FS) and `postgresql@15-main` will not restart until root is freed (2026-06-11); postgres WAL stalls; systemd-journald corrupts. Incidents on this path: 2026-05-10 SEV-2 (`internal/incidents/data/2026-05-10-redis-writes-blocked-disk-full.md`), 2026-06-11 (`internal/incidents/data/2026-06-11-clickhouse-log-channel-wedge-root-full.md`, root filled at ~3.8 GB/min), 2026-08-05 recurrence to 81 % (rsyslog duplicate of the API access log, see `15-log-discipline.yml`). |

## Symptoms

- `(node_filesystem_avail_bytes{mountpoint="/"} / node_filesystem_size_bytes{mountpoint="/"}) * 100 < 10` for ≥ 1 min.
- Root free-space graph is a straight line to zero — `stellarindex_node_root_disk_filling_fast` (predict_linear, `node-root-disk-filling-fast.md`) usually fires first.
- Customer-side: `/v1/price` 404s on rewritten pairs; aggregator log shows repeating `WARN` lines about Redis Set MISCONF errors. Companion P1s: `stellarindex_redis_writes_blocked`, `stellarindex_aggregator_cache_write_errors`.
- Synthetic: `cmd/stellarindex-sla-probe` (15-min timer, `configs/healthchecks/stellarindex-sla-probe.timer`) `/v1/price` sample fails → `stellarindex_sla_probe_*` alerts. (Whether the public status page surfaces this is not verifiable from the repo.)

## Quick diagnosis (≤ 5 min)

```sh
# What's filling the disk? -x = stay on the root FS.
# /var/lib/{clickhouse,postgresql,galexie,minio,loki,prometheus,pgbackrest,stellarindex}
# are ZFS datasets on the multi-TB `data` pool — never du them without -x.
df -h /
sudo du -xsh /var/log/* /tmp /var/cache 2>/dev/null | sort -rh | head -15

# The two biggest known root consumers that du above will NOT explain:
ls -lh /swap_f1209; swapon --show                    # 16 G swap file on the 49 G root
zfs list -o name,mountpoint,mounted data/prometheus data/loki data/clickhouse data/postgres
# an UNMOUNTED dataset silently lands that data (e.g. ~13 G prometheus TSDB) back on root

# Is it the journal?
journalctl --disk-usage

# Is it logs that haven't rotated?
ls -lh /var/log/syslog* /var/log/postgresql/*.log 2>/dev/null

# Who is writing? (names the flooding unit)
journalctl --since '-5min' --no-pager | awk '{print $5}' | sort | uniq -c | sort -rn | head -5
```

If the flooder is `clickhouse-server` (2026-06-11 signature: `Cannot log message in OwnAsyncSplitChannel`), freeing space does NOT unwedge it — `sudo systemctl restart clickhouse-server` first (see `node-root-disk-filling-fast.md`).

Key signals:
- **Multi-GB syslog** → one of: stellarindex-* API access log duplicated into syslog (~2.5 GB/day; guard = `/etc/rsyslog.d/30-stellarindex-journald-only.conf`, 2026-08-05); loki / clickhouse-server flood (guard = `/etc/rsyslog.d/10-suppress-noisy-units.conf`); the `/etc/logrotate.d/rsyslog` override silently skipped because it lacks `su root adm` ("insecure permissions"). Note logrotate.timer is daily, so `maxsize` cannot cap intra-day growth.
- **3 GB+ journal** → `SystemMaxUse=500M` cap missing (`/etc/systemd/journald.conf.d/00-cap.conf`).
- **`/var/log/stellarindex/*.log`** → operator one-shot job logs (logrotate 500M/weekly via `/etc/logrotate.d/stellarindex`); ad-hoc walk outputs land in `/tmp`. Heavy jobs should run under `/usr/local/sbin/run-heavy-job.sh`, which has a root-disk watchdog.
- **postgres logs** → the repo template (`postgresql.conf.j2`) still sets `log_min_duration_statement=1000`, `log_connections=on`, `log_disconnections=on`, while r1 was hand-set to `-1`/`none`/`off` on 2026-06-11. Check the live value: `sudo -u postgres psql -Atc "show log_min_duration_statement; show log_statement; show log_connections;"` — an ansible apply reverts it.

## Mitigation (≤ 15 min)

```sh
# 1. Free immediate space (vacuum the journal first — fast win)
sudo journalctl --vacuum-size=200M

# 2. Truncate any rotated-but-uncompressed syslog
sudo truncate -s 0 /var/log/syslog.1
sudo rm -f /var/log/syslog.[2-9]*

# 3. Postgres log — ONLY if PG is down or the file is > 1 GB
sudo truncate -s 0 /var/log/postgresql/postgresql-15-main.log

# 4. Confirm Redis can BGSAVE again (Debian redis-server, 127.0.0.1:6379, no auth)
redis-cli BGSAVE
# Wait ~5 s then:
redis-cli INFO persistence | grep rdb_last_bgsave_status
# expect: rdb_last_bgsave_status:ok

# 5. Did Postgres / ClickHouse survive?
systemctl is-active postgresql@15-main clickhouse-server redis-server stellarindex-api stellarindex-aggregator stellarindex-indexer
sudo systemctl start postgresql@15-main        # if it crashed on the full root
journalctl -u stellarindex-indexer --since -5min | grep -c 'pool may be wedged'   # indexer PG pool
```

- [ ] Step 1 — execute the recovery sequence above to drop usage below 80 %.
- [ ] Step 2 — confirm the customer-visible recovery: `curl http://localhost:3000/v1/price?asset=native&quote=fiat:USD` returns 200 with `flags.stale=false`.
- [ ] Step 3 — find which guard regressed: `curl -s 'http://localhost:9090/api/v1/query?query=stellarindex_config_assertion_ok' | jq '.data.result[] | {a:.metric.assertion, v:.value[1]}'` (hourly `config-assertions.timer`; assertions `rsyslog_ch_suppress`, `rsyslog_loki_suppress`, `journald_cap`, `ch_logs_on_zfs`, `syslog_maxsize`). Then re-apply ONLY the relevant tags: `ansible-playbook ... --tags logrotate,journald,rsyslog --check --diff` before a real run. Do NOT apply all of `15-log-discipline.yml` mid-incident — its handlers restart `clickhouse-server` and `redis`. Ansible does not auto-run against r1; 2026-06-11 rules were codified-but-never-applied until 2026-07-03.
- [ ] Step 4 — update the status page if customer-visible time exceeded 5 min (per SEV playbook).
- [ ] Verification: `node_filesystem_avail_bytes{mountpoint="/"} / node_filesystem_size_bytes{mountpoint="/"} > 0.30` (30% free); `stellarindex_node_root_disk_filling_fast` not firing; `stellarindex_config_assertion_ok == 1` for the five assertions above.

## Root cause analysis

For postmortem:
- The full output of `sudo du -xsh /var/log/* /tmp /var/cache` and `swapon --show` at the moment the alert fired.
- The flooding unit from the `journalctl | awk` histogram.
- The state of `/etc/logrotate.d/rsyslog` (incl. `su root adm` + `maxsize`), `/etc/systemd/journald.conf.d/00-cap.conf`, `/etc/rsyslog.d/10-suppress-noisy-units.conf`, `/etc/rsyslog.d/30-stellarindex-journald-only.conf`, `/etc/clickhouse-server/config.d/zzz-logpath.xml` — and the `stellarindex_config_assertion_ok` series over the preceding day.
- Live PG logging settings vs the repo template.
- The aggregator log around the moment Redis stopped accepting writes.

## Known false-positive patterns

- None known. Headroom can be under 5 min in a log-flood (3.8 GB/min on 2026-06-11), and the 49 G root carries a 16 G swap file (`/swap_f1209`; dropping it is an open operator decision). Fire = act immediately.

## Related

- `node-root-disk-filling-fast.md` — the predict_linear page that usually fires first; the clickhouse-server wedge procedure.
- `redis-write-blocked-disk-full.md` — downstream symptom when this alert was missed; the May-10 incident's primary remediation.
- `db-disk-full.md` — sibling for the postgres data volume.
- `node-root-disk-warning.md` — early-warning at 20% free.
- `docs/operations/r1-ansible-drift-2026-07-03.md` — why hand-applied guards and the ansible role disagree.
- ADR-0008 — HA topology (single-host R1 today; fewer fail-safes than R2/R3 will have).
- 2026-05-10 incident postmortem (`internal/incidents/data/2026-05-10-redis-writes-blocked-disk-full.md`).
- 2026-06-11 incident postmortem (`internal/incidents/data/2026-06-11-clickhouse-log-channel-wedge-root-full.md`).

## Changelog

- 2026-08-28 — re-verified against HEAD: rule file path, `du -x`, swap/ZFS checks, PG-crash impact, 2026-06-11/08-05 signals, config-assertions gate, tag-scoped ansible re-apply; dropped nonexistent `galexie-verify-*.stderr` and obsolete `wasm-history-*.stderr` paths. TODO(ash): both rule files' `runbook_url` for this alert still point at `redis-write-blocked-disk-full.md`.
- 2026-05-12 — initial draft (audit-2026-05-12 F-1237 closure).
