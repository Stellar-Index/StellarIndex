---
title: Runbook — backup-offsite alerts
last_verified: 2026-10-05
status: draft
---
# Off-site backup alerts

Alerts from `deploy/monitoring/rules/backup-offsite.yml` (R1 overlay: `configs/prometheus/rules.r1/backup-offsite.yml`).

## At a glance

- [`stellarindex_backup_offsite_stale`](#stellarindex_backup_offsite_stale)

## stellarindex_backup_offsite_stale

**Severity** P3 (ticket). MTTR 15 min (credentials / endpoint), up to 4 h (a full backup to repo2).
**Trigger** for 1h:
`up{job="pgbackrest_exporter"} == 1 unless on (instance) (pgbackrest_backup_info{repo_key="2"} unless pgbackrest_backup_info{repo_key="2"} offset 8d)`
The exporter is up but no `pgbackrest_backup_info` series with `repo_key="2"` appeared in 8 days (a new backup is a new series).
**Impact** The pgBackRest copy that survives host or pool loss (repo2, encrypted S3) is older than 8 days or never written. On-host repo1 may be fresh; `stellarindex_timescale_backup_failed` / `_none_24h` look across repos and stay green, so this is the ONLY alert for a dead off-site stream. Until it clears, DR from host loss restores at most an 8-day-old database (ADR-0043).
The status page Backups panel shows **Off-site copy (S3, repo 2)** red ("beyond SLO") with the real age, or grey ("no off-site repository reported") if repo2 was never written (via `/v1/diagnostics/backups`). A grey "stamp from the future" is a different fault this alert does not cover: newest repo2 label parses ahead of the API clock (host clock skew or corrupt label). The rule reads series presence, not label times. Check `timedatectl` / `chronyc tracking` on the archival node and the `backup_name` labels in Diagnose step 4.

**Diagnose** (5 min)

```sh
# 1. What does pgBackRest itself say about repo2?
sudo -u postgres pgbackrest --stanza=stellarindex info --repo=2
sudo -u postgres pgbackrest --stanza=stellarindex info --output=json | jq '.[0].backup[] | {label, type, repo: .database["repo-key"]}'

# 2. Is repo2 configured at all on this host?
sudo grep -n '^repo2' /etc/pgbackrest/pgbackrest.conf
#    No repo2-* lines -> the inventory has pgbackrest_offsite_ack=true
#    (or no pgbackrest_repo2_s3_bucket). That is a DECISION, recorded in
#    docs/operations/off-site-backup-plan.md; the alert is telling you
#    the decision is still costing you a DR copy.

# 3. Did the nightly backup fail writing repo2?
sudo journalctl -u pgbackrest-backup.service -n 100 --no-pager | grep -iE 'repo2|s3|error'

# 4. What does the exporter see? (the alert reads THIS, not pgbackrest directly)
curl -s localhost:9854/metrics | grep 'pgbackrest_backup_info' | grep 'repo_key="2"' | tail -3
```

**Causes and fixes**

- repo2 S3 credentials expired/rotated (`info --repo=2` 403 / `AccessDenied`; journal `S3 ... 403`): re-issue the key, update `pgbackrest_repo2_s3_key` / `_key_secret` in vault, re-apply the role (sourced from the vault env, no_log).
- Bucket quota / lifecycle rule deleted objects (`info --repo=2` lists no backups; provider console shows bucket empty or capped): raise quota / fix the lifecycle rule, then `pgbackrest --stanza=stellarindex --repo=2 --type=full backup`.
- Endpoint DNS / TLS (`unable to resolve` / TLS handshake errors): fix `pgbackrest_repo2_s3_endpoint`; verify with `curl -sI https://<endpoint>`.
- repo2 never configured (no `repo2-*` in `pgbackrest.conf`; inventory `pgbackrest_offsite_ack: true`): provision repo2 (set `pgbackrest_repo2_s3_bucket` + other repo2 vars, `pgbackrest_manage_conf: true`, then `stanza-upgrade` + a full backup; see `docs/operations/off-site-backup-plan.md`). Do NOT silence the alert: the ack records the gap, this alert prices it.
- Backups genuinely stopped (`stellarindex_timescale_backup_none_24h` ALSO firing): follow [backup-failed](infra.md#stellarindex_timescale_backup_none_24h); that is the root cause and this clears once a backup reaches repo2.

**Recover**

1. Fix the cause, then force a full backup to repo2 rather than waiting for Sunday (1-4 h for the r1 database; watch `journalctl -f -u pgbackrest-backup.service` if via the unit, or foreground output):
   ```sh
   sudo -u postgres pgbackrest --stanza=stellarindex --repo=2 --type=full backup
   ```
2. Confirm the exporter picked it up (re-reads `pgbackrest info` every 10 min): `curl -s localhost:9854/metrics | grep 'repo_key="2"'` shows a `backup_name` with today's date.
3. The alert resolves on the next evaluation with a young repo2 series; the status page Off-site row turns green within 60 s (API snapshot cache).

Related: [restore-drill-stale](restore-drill.md#stellarindex_restore_drill_stale) (monthly proof repo1 restores; stale repo2 makes it partial), [restore-drill-offsite-stale](restore-drill.md#stellarindex_restore_drill_offsite_stale) (monthly proof the repo2 copy restores, `restore-drill-offsite.timer`, the 15th), `docs/operations/off-site-backup-plan.md`, `docs/operations/pgbackrest-encryption.md` (repo2 is encrypted by us; a re-created repo needs its cipher pass).

## Related

- [archive-completeness](archive.md): archive completeness alerts.
