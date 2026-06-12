---
title: Runbook — node-root-disk-full
last_verified: 2026-05-12
status: draft
severity: P1
---

# Runbook — `stellarindex_node_root_disk_full`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_node_root_disk_full` |
| Severity | **P1** (page) |
| Detected by | `deploy/monitoring/rules/storage.yml` |
| Typical MTTR | 15–60 min |
| Impact | The host's root filesystem is < 10 % free. Multiple cascading failures follow within 30 min: Redis BGSAVE blocks (every cache write returns MISCONF) → `/v1/price` 404s on every rewritten/triangulated/stablecoin-proxy pair; postgres WAL stalls; systemd-journald corrupts. The 2026-05-10 SEV-2 (`internal/incidents/data/2026-05-10-redis-writes-blocked-disk-full.md`) hit this exact path. |

## Symptoms

- `(node_filesystem_avail_bytes{mountpoint="/"} / node_filesystem_size_bytes{mountpoint="/"}) * 100 < 10` for ≥ 1 min.
- Customer-side: `/v1/price` 404s on rewritten pairs; aggregator log shows repeating `WARN` lines about Redis Set MISCONF errors.
- Status page: synthetic `/v1/price?asset=native&quote=fiat:USD` probe red.

## Quick diagnosis (≤ 5 min)

```sh
# What's filling the disk?
df -h /
sudo du -sh /var/log/* /var/cache/* /var/lib/* 2>/dev/null | sort -hr | head -20

# Is it the journal?
journalctl --disk-usage

# Is it logs that haven't rotated?
ls -lh /var/log/syslog* /var/log/postgresql/*.log 2>/dev/null
```

Key signals (from the May-10 incident):
- **8 GB+ syslog** → logrotate misconfig (the May-10 fix in `15-log-discipline.yml` should have closed this; if it's back, that fix regressed).
- **3 GB+ journal** → systemd-journald cap missing (same `15-log-discipline.yml` fix).
- **5+ GB galexie verify history** → `/var/log/galexie-verify-*.stderr` accumulation; the WASM-audit one-time captures (also called out in May-10 follow-ups).
- **postgres logs** → `log_min_duration_statement` may be too aggressive.

## Mitigation (≤ 15 min)

The May-10 incident's exact recovery sequence:

```sh
# 1. Free immediate space (vacuum the journal first — fast win)
journalctl --vacuum-size=200M

# 2. Truncate any rotated-but-uncompressed syslog
sudo truncate -s 0 /var/log/syslog.1
sudo rm -f /var/log/syslog.[2-9]*

# 3. Remove WASM-audit one-time stderr captures
sudo rm -f /var/log/wasm-history-*.stderr

# 4. Confirm Redis can BGSAVE again
redis-cli BGSAVE
# Wait ~5 s then:
redis-cli INFO persistence | grep rdb_last_bgsave_status
# expect: rdb_last_bgsave_status:ok
```

- [ ] Step 1 — execute the recovery sequence above to drop usage below 80 %.
- [ ] Step 2 — confirm the customer-visible recovery: `curl http://localhost:3000/v1/price?asset=native&quote=fiat:USD` returns 200 with `flags.stale=false`.
- [ ] Step 3 — if logrotate or journald cap missing, re-apply the `15-log-discipline.yml` ansible task from the archival-node role.
- [ ] Step 4 — update the status page if customer-visible time exceeded 5 min (per SEV playbook).
- [ ] Verification: `node_filesystem_avail_bytes{mountpoint="/"} / node_filesystem_size_bytes{mountpoint="/"} > 0.30` (30% free).

## Root cause analysis

For postmortem:
- The full output of `du -sh /var/*` at the moment the alert fired.
- The state of `/etc/logrotate.d/rsyslog` and `/etc/systemd/journald.conf.d/00-cap.conf` (these should match the templates the ansible role provisions).
- The aggregator log around the moment Redis stopped accepting writes.

## Known false-positive patterns

- None known. The 10% threshold is specifically chosen to give 1–2 hours of headroom before customer-visible failures begin. Fire = act.

## Related

- `redis-write-blocked-disk-full.md` — downstream symptom when this alert was missed; the May-10 incident's primary remediation.
- `db-disk-full.md` — sibling for the postgres data volume.
- `node-root-disk-warning.md` — early-warning at 20% free.
- ADR-0008 — HA topology (single-host R1 today; fewer fail-safes than R2/R3 will have).
- 2026-05-10 incident postmortem (`internal/incidents/data/2026-05-10-...md`).

## Changelog

- 2026-05-12 — initial draft (audit-2026-05-12 F-1237 closure).
