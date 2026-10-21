---
title: Runbook — config-assertion-failed
last_verified: 2026-10-21
status: current
severity: P3
---

# Runbook — `stellarindex_config_assertion_failed`

P3 (ticket). Alerts: `stellarindex_config_assertion_failed` and `stellarindex_config_assertions_stale` (checker silent). Rules: `deploy/monitoring/rules/storage.yml` + `configs/prometheus/rules.r1/storage.yml`; producer is the hourly `config-assertions.timer` running `/usr/local/bin/config-assertions.sh`. A load-bearing guard config is missing or reverted on the host; nothing is broken yet.

## At a glance

P3 ticket, MTTR 10-30 min. Nothing is broken yet; a guard config is missing or reverted on the host.

## What each assertion protects

| Assertion | Fix it guards | Restore |
|---|---|---|
| `rsyslog_ch_suppress` / `rsyslog_loki_suppress` | root-fill loop (journald flood to syslog) | re-apply `/etc/rsyslog.d/10-suppress-noisy-units.conf` from ansible role 15-log-discipline.yml; `systemctl restart rsyslog` |
| `journald_cap` | journald bounded on root | `journald.conf.d/00-cap.conf` (`SystemMaxUse=500M`); `systemctl restart systemd-journald` |
| `ch_logs_on_zfs` | CH log channel can't wedge on a full root | `config.d/zzz-logpath.xml`; restart clickhouse-server |
| `syslog_maxsize` | syslog rotation caps burst growth | `/etc/logrotate.d/rsyslog` from the same role |
| `nft_https_open` | Caddy public edge (80/443) — the API/explorer | check `nft list ruleset`; a firewall re-render dropped the `public_allow_ports_base` rules — reload nftables from a config containing them |
| `redis_maxmemory` | uncapped-Redis fix | `maxmemory 1gb` in `/etc/redis/redis.conf`; restart redis-server |
| `supply_reserve_accounts` / `_nonempty` | CS-010 circulating-supply config | restore `[supply].sdf_reserve_accounts` from `stellarindex_sdf_reserve_accounts` (role defaults, 16 accounts); restart indexer + aggregator |
| `galexie_writer_creds_valid` | MinIO credential-rotation drift — `/etc/default/galexie`'s creds must still authenticate against the live MinIO galexie-writer user | see [credential-rotation.md](../credential-rotation.md#minio) — regenerate the galexie-writer secret in the vault AND `--tags minio` re-apply so both sides move together; then restart `galexie` |
| `ch_drop_guard_live` | ClickHouse destructive-DDL size guard . FAIL = live `max_table_size_to_drop` or `max_partition_size_to_drop` in `system.server_settings` is `0`, above the pinned 50 GB (`53687091200`), missing, or ClickHouse did not answer on `:8123` | find the carrier: `sudo grep -rn 'size_to_drop' /etc/clickhouse-server/config.xml /etc/clickhouse-server/config.d/`; remove any file other than `si-drop-guard.xml`, then re-apply `--tags clickhouse-drop-guard` (hot-reloads, no restart). A planned big drop uses the `force_drop_table` flag, never a raised limit — [clickhouse-destructive-ddl.md](../clickhouse-destructive-ddl.md) |
| `trades_compression_policy_scheduled` | `trades` keeps compressing after a `usd-volume-restamp -chunks -write` run. The run pauses the policy and re-enables it on exit; a SIGKILL skips the re-enable. FAIL = the policy is unscheduled while no session holds the run's advisory lock `hashtext('usd-volume-restamp:trades')`, or `trades` has no compression policy | confirm no restamp run is alive, then `SELECT alter_job(job_id, scheduled => true) FROM timescaledb_information.jobs WHERE proc_name = 'trades_compression_policy';`. The killed run's last trace names any chunk it left decompressed; `compress_chunk` it |
| `zfs_userspace_works` / `zfs_module_on_disk` / `zfs_packages_held` | a broken ZFS userspace or a deleted dkms module leaves the pool one reboot from unimportable | `zpool status data` must work; reinstall `zfs-dkms` for the running kernel so `/lib/modules/$(uname -r)/updates/dkms/zfs.ko*` exists; `apt-mark hold zfs-dkms`. Do not reboot until all three pass |
| `pg_max_worker_processes_codified` / `_live` | Postgres background-worker headroom (`max_worker_processes = 32`) | codified FAIL: re-render `postgresql.conf` from the role; live FAIL only: the setting is postmaster-level, so it needs a planned Postgres restart |
| `pg_idle_in_transaction_timeout_codified` / `_live` | idle-in-transaction reaper | re-render `postgresql.conf`; live FAIL only: `SELECT pg_reload_conf();` |
| `caggs_have_refresh_policy` | every continuous aggregate has a refresh policy (a CAGG without one emits no staleness series, so its stale alert cannot fire) | find the CAGG with no `policy_refresh_continuous_aggregate` job and re-add the policy from its migration |
| `compression_policies_applied` | the operator script `add-missing-compression-policies.sql` was run (it has no execution tracking) | run that script; if a table was deliberately excluded, update the want-list in `config-assertions.sh` in the same PR |
| `ch_ops_batch_env_pair` / `ch_ops_batch_not_in_service_env` | low-priority ClickHouse `ops_batch` identity: both the CH user (`users.d/ops-batch.xml`) and the env pair in `/etc/default/stellarindex-ops` must exist, and the pair must never reach `/etc/default/stellarindex` (it would demote live services) | re-apply `--tags clickhouse-serving-profile`; remove the pair from the service env if present |
| `archive_trimmer_policy_has_delete` / `archive_trimmer_uses_distinct_identity` | `galexie-archive-trim` deletes as its own MinIO identity; as the read-only reader it gets AccessDenied per object yet exits clean | re-apply `--tags minio` so the trimmer policy grants `s3:DeleteObject` and its env carries its own access key |
| `minio_prometheus_token_present` | MinIO metrics bearer token exists with safe permissions (it goes stale on a root rotation) | re-apply `--tags minio` to re-mint it. Stat only; never read the token |
| `loki_loopback_bound` | Loki has auth disabled, so loopback binding is its only access control | fix the listen addresses in the Loki config and restart Loki (they are read at start) |
| `tx_hash_index_parity` | `GET /v1/tx/{hash}` treats a `stellar.tx_hash_index` miss as an authoritative 404; the index is MV-maintained. This probe samples 500 recent tx hashes hourly and requires all to resolve in the index — a FAIL means the MV was dropped/recreated with a gap, or rows entered `stellar.transactions` without firing MVs (ATTACH-style load) | find the divergence window (compare per-ledger presence around the FAIL's sample hashes), then re-run the windowed backfill: `stellarindex-ops ch-txindex-backfill -ch-addr 127.0.0.1:9300 -from <window start> -to <window end> -window 5000000 -write` (idempotent — ReplacingMergeTree collapses re-inserts); verify `tx_hash_index_mv` exists |

## Diagnosis

```sh
/usr/local/bin/config-assertions.sh   # prints FAIL <assertion> per gap
```

Then restore per the table above. If the assertion fails because the
config was *deliberately* changed, update the assertion in
`scripts/ops/config-assertions.sh` in the same PR as the change.

Ansible does not auto-apply to r1, so codified and live can diverge ([ansible-drift](../ansible-drift.md)); this check is the live-state backstop between the Monday [ansible-drift](../../../.github/workflows/ansible-drift.yml) runs.

## Related

- [node-root-disk-filling-fast](infra.md#stellarindex_node_root_disk_filling_fast) — what several of these guards prevent.
- [redis-write-blocked-disk-full](cache.md#stellarindex_redis_writes_blocked)
