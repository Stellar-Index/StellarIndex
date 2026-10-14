---
title: Runbook — backup-offsite alerts
last_verified: 2026-10-05
status: draft
---
# Off-site backup alerts

## At a glance

Rules: `deploy/monitoring/rules/backup-offsite.yml` (R1 overlay: `configs/prometheus/rules.r1/backup-offsite.yml`).

## stellarindex_backup_offsite_stale

P3. The exporter is up but no `pgbackrest_backup_info{repo_key="2"}` series appeared in 8 days. repo2 (encrypted S3) is the copy that survives host loss; it is the ONLY alert for a dead off-site stream (`stellarindex_timescale_backup_failed` / `_none_24h` look across repos). Status page: Off-site copy (S3, repo 2) red or grey. A grey "stamp from the future" is clock skew or a corrupt label, not this alert: check `timedatectl` / `chronyc tracking`.

Check:

```sh
sudo -u postgres pgbackrest --stanza=stellarindex info --repo=2
sudo -u postgres pgbackrest --stanza=stellarindex info --output=json | jq '.[0].backup[] | {label, type, repo: .database["repo-key"]}'
sudo grep -n '^repo2' /etc/pgbackrest/pgbackrest.conf
sudo journalctl -u pgbackrest-backup.service -n 100 --no-pager | grep -iE 'repo2|s3|error'
curl -s localhost:9854/metrics | grep 'pgbackrest_backup_info' | grep 'repo_key="2"' | tail -3
```

No `repo2-*` lines means inventory has `pgbackrest_offsite_ack=true` (a recorded decision, `docs/operations/off-site-backup-plan.md`).

Likely causes:

- S3 credentials expired (403 / `AccessDenied`): re-issue the key, update `pgbackrest_repo2_s3_key` / `_key_secret` in vault, re-apply the role.
- Bucket quota or lifecycle rule deleted objects: raise quota / fix the rule.
- Endpoint DNS / TLS: fix `pgbackrest_repo2_s3_endpoint`; verify with `curl -sI https://<endpoint>`.
- repo2 never configured: set `pgbackrest_repo2_s3_bucket` + other repo2 vars and `pgbackrest_manage_conf: true`, then `stanza-upgrade` and a full backup. Do NOT silence the alert.
- Backups stopped (`stellarindex_timescale_backup_none_24h` also firing): see [backup-failed](infra.md#stellarindex_timescale_backup_none_24h).

Fix:

1. Fix the cause, then force a full backup to repo2 (1-4 h):
   ```sh
   sudo -u postgres pgbackrest --stanza=stellarindex --repo=2 --type=full backup
   ```
2. Confirm: `curl -s localhost:9854/metrics | grep 'repo_key="2"'` shows today's `backup_name` (exporter re-reads every 10 min).
3. The alert clears on the next evaluation; the status page turns green within 60 s.

Escalate: if S3 access cannot be restored in 4 h, page the infra owner.

Related: [restore-drill-stale](restore-drill.md#stellarindex_restore_drill_stale), [restore-drill-offsite-stale](restore-drill.md#stellarindex_restore_drill_offsite_stale), `docs/operations/off-site-backup-plan.md`, `docs/operations/pgbackrest-encryption.md`, [archive-completeness](archive.md).

## Related

- [Alerts catalogue](../alerts-catalog.md)
