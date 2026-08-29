---
title: Runbook — backup-offsite-stale
last_verified: 2026-08-29
status: ratified
severity: P3
---

# Runbook — `stellarindex_backup_offsite_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_backup_offsite_stale` |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/backup-offsite.yml` (R1 overlay: `configs/prometheus/rules.r1/backup-offsite.yml`) |
| Typical MTTR | 15 min (credentials / endpoint) · up to 4 h (a full backup to repo2) |
| Impact | The pgBackRest copy that survives host or pool loss (repo2, encrypted S3) is older than 8 days or has never been written. The on-host repo1 may be perfectly fresh — `stellarindex_timescale_backup_failed` / `_none_24h` look across repos and stay green — so this is the ONLY alert for a dead off-site stream. Until it clears, DR from a host loss restores at most an 8-day-old database (REL-DR, ADR-0043). |

## Symptoms

- `up{job="pgbackrest_exporter"} == 1 unless on (instance) (pgbackrest_backup_info{repo_key="2"} unless pgbackrest_backup_info{repo_key="2"} offset 8d)`
  for ≥ 1 h: the exporter is up, but no `pgbackrest_backup_info`
  series with `repo_key="2"` appeared in the last 8 days (a new backup
  is a new series — that is the whole signal).
- The public status page's Backups panel shows **Off-site copy (S3,
  repo 2)** in red ("beyond SLO") with the real age, or grey ("no
  off-site repository reported") when repo2 has never been written.
  Same 8-day SLO, same series, read through `/v1/diagnostics/backups`.
- A grey **"stamp from the future"** on that row is a DIFFERENT fault and
  this alert does not fire for it: the newest repo2 backup label parses to
  a time ahead of the API's clock (host clock skew, or a corrupt label),
  so the API reports `age_seconds` negative with verdict `unknown` rather
  than rewarding it with a fresh `0`. The rule reads series PRESENCE, not
  label times, so it stays silent while the real copy may be any age.
  Check `timedatectl` / `chronyc tracking` on the archival node and the
  `backup_name` labels in step 4 below.

## Quick diagnosis (≤ 5 min)

```sh
# 1. What does pgBackRest itself say about repo2?
sudo -u postgres pgbackrest --stanza=stellarindex info --repo=2
sudo -u postgres pgbackrest --stanza=stellarindex info --output=json | jq '.[0].backup[] | {label, type, repo: .database["repo-key"]}'

# 2. Is repo2 configured at all on this host?
sudo grep -n '^repo2' /etc/pgbackrest/pgbackrest.conf
#    No repo2-* lines → the inventory has pgbackrest_offsite_ack=true
#    (or no pgbackrest_repo2_s3_bucket). That is a DECISION, recorded in
#    docs/operations/off-site-backup-plan.md; the alert is telling you
#    the decision is still costing you a DR copy.

# 3. Did the nightly backup fail writing repo2?
sudo journalctl -u pgbackrest-backup.service -n 100 --no-pager | grep -iE 'repo2|s3|error'

# 4. What does the exporter see? (the alert reads THIS, not pgbackrest directly)
curl -s localhost:9854/metrics | grep 'pgbackrest_backup_info' | grep 'repo_key="2"' | tail -3
```

## Likely causes

| Cause | How to confirm | Fix |
| ----- | -------------- | --- |
| repo2 S3 credentials expired / rotated | `pgbackrest info --repo=2` errors with 403 / `AccessDenied`; journal shows `S3 ... 403` | Re-issue the key, update `pgbackrest_repo2_s3_key` / `_key_secret` in vault, re-apply the role (PR #272 sources them from the vault env, no_log) |
| Bucket quota / lifecycle rule deleted objects | `info --repo=2` lists no backups; the provider's console shows the bucket empty or capped | Raise quota / fix the lifecycle rule, then `pgbackrest --stanza=stellarindex --repo=2 --type=full backup` |
| Endpoint DNS / TLS | journal shows `unable to resolve` / TLS handshake errors for the S3 endpoint | Fix `pgbackrest_repo2_s3_endpoint`; verify with `curl -sI https://<endpoint>` |
| repo2 never configured (offsite gap acknowledged) | No `repo2-*` in `pgbackrest.conf`; inventory has `pgbackrest_offsite_ack: true` | Provision repo2 (set `pgbackrest_repo2_s3_bucket` + the other repo2 vars, `pgbackrest_manage_conf: true`, then `stanza-upgrade` + a full backup — see `docs/operations/off-site-backup-plan.md`). Do NOT silence the alert instead: the ack records the gap, this alert prices it. |
| Backups genuinely stopped | `stellarindex_timescale_backup_none_24h` is ALSO firing | Follow [backup-failed](backup-failed.md) — that is the root cause; this alert clears once a backup reaches repo2 |

## Recovery

1. Fix the cause above.
2. Force a full backup to repo2 rather than waiting for Sunday:
   ```sh
   sudo -u postgres pgbackrest --stanza=stellarindex --repo=2 --type=full backup
   ```
   Expect 1–4 h for the r1 database; watch `journalctl -f -u pgbackrest-backup.service`
   if you ran it through the unit, or the foreground output if by hand.
3. Confirm the exporter picked it up (it re-reads `pgbackrest info` every
   10 min): `curl -s localhost:9854/metrics | grep 'repo_key="2"'` shows a
   `backup_name` with today's date.
4. The alert resolves on the next evaluation with a young repo2 series.
   The status page's Off-site row turns green within 60 s (the API caches
   the snapshot for 60 s).

## Related

- [backup-failed](backup-failed.md) — backups stopped across ALL repos (the causal alert when both fire).
- [restore-drill-stale](restore-drill-stale.md) — the monthly proof the chain is restorable; a stale repo2 makes that proof partial.
- `docs/operations/off-site-backup-plan.md` — why repo2 exists and the deferred-offsite decision record.
- `docs/operations/pgbackrest-encryption.md` — repo2 is encrypted by us; a re-created repo needs its cipher pass.
