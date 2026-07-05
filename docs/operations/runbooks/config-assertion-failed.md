---
title: Runbook — config-assertion-failed
last_verified: 2026-07-05
status: current
severity: P3
---

# Runbook — `stellarindex_config_assertion_failed`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_config_assertion_failed` (+ `stellarindex_config_assertions_stale` for a silent checker) |
| Severity | **P3** (ticket) |
| Detected by | `deploy/monitoring/rules/storage.yml` + `configs/prometheus/rules.r1/storage.yml`; producer is the hourly `config-assertions.timer` running `/usr/local/bin/config-assertions.sh` |
| Typical MTTR | 10–30 min |
| Impact | A load-bearing guard config is missing or content-reverted on the host. Nothing is broken *yet* — the alert exists because these gaps historically surfaced as incidents months later (the 2026-06-11 rsyslog suppression was never applied; live-only fixes were one playbook run from erasure). |

## What each assertion protects

| Assertion | Fix it guards | Restore |
|---|---|---|
| `rsyslog_ch_suppress` / `rsyslog_loki_suppress` | 2026-06-11 root-fill loop (journald flood → syslog) | re-apply `/etc/rsyslog.d/10-suppress-noisy-units.conf` from ansible role 15-log-discipline.yml; `systemctl restart rsyslog` |
| `journald_cap` | journald bounded on root | `journald.conf.d/00-cap.conf` (`SystemMaxUse=500M`); `systemctl restart systemd-journald` |
| `ch_logs_on_zfs` | CH log channel can't wedge on a full root | `config.d/zzz-logpath.xml`; restart clickhouse-server |
| `syslog_maxsize` | syslog rotation caps burst growth | `/etc/logrotate.d/rsyslog` from the same role |
| `nft_https_open` | Caddy public edge (80/443) — the API/explorer | check `nft list ruleset`; a firewall re-render dropped the `public_allow_ports_base` rules — reload nftables from a config containing them |
| `redis_maxmemory` | 2026-06-16 uncapped-Redis fix | `maxmemory 1gb` in `/etc/redis/redis.conf`; restart redis-server |
| `supply_reserve_accounts` / `_nonempty` | CS-010 circulating-supply config | restore `[supply]` from `inventory/r1.yml` vars (16 accounts + balances); restart indexer + aggregator |
| `galexie_writer_creds_valid` | MinIO credential-rotation drift (BACKLOG #66, 2026-07-03 follow-up) — `/etc/default/galexie`'s creds must still authenticate against the live MinIO galexie-writer user | see [credential-rotation.md](../credential-rotation.md#minio) — regenerate the galexie-writer secret in the vault AND `--tags minio` re-apply so both sides move together; then restart `galexie` |

## Diagnosis

```sh
/usr/local/bin/config-assertions.sh   # prints FAIL <assertion> per gap
```

Then restore per the table above. If the assertion fails because the
config was *deliberately* changed, update the assertion in
`scripts/ops/config-assertions.sh` in the same PR as the change.

## Why this exists

See [r1-ansible-drift-2026-07-03](../r1-ansible-drift-2026-07-03.md):
ansible does not auto-run against r1, so codified≠live in either
direction and neither self-heals. This check is the backstop until
ansible becomes the actual deployment path.

## Related

- [node-root-disk-filling-fast](node-root-disk-filling-fast.md) — what several of these guards prevent.
- [redis-write-blocked-disk-full](redis-write-blocked-disk-full.md)
