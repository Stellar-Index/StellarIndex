---
title: Runbook — Galexie archive off-site mirror (off-site-backup-plan.md §1)
last_verified: 2026-09-28
status: draft
severity: P3
---

# Runbook — `stellarindex_galexie_archive_mirror_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_galexie_archive_mirror_stale` (no successful, verified mirror run in 48 h — including a host with no off-site target configured; per host, carries `instance`) |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/storage.yml` and `configs/prometheus/rules.r1/storage.yml` |
| Producer | `scripts/ops/galexie-archive-mirror.sh` via `galexie-archive-mirror.timer` (hourly) |
| Typical MTTR | 15 min to fix a credential; a first full sync of a multi-TiB archive can run for hours |
| Impact | No immediate customer impact. While it fires, the archive has no off-site copy: a pool or box loss recovers only via re-ingest of the public Stellar history archives (days–weeks). |

## Symptoms

- The ticket names a host. `stellarindex_galexie_archive_mirror_configured{instance="<host>:9100"}`
  is `0` (no off-site target configured) or `1` (configured, but no verified
  run has succeeded in 48 h).
- `systemctl status galexie-archive-mirror.service` shows a failed or
  long-running run. An unconfigured host fails every run on purpose
  (`no DEST_ENDPOINT configured` in the journal): a run that copied nothing
  never reports success.

## Quick diagnosis (≤ 5 min)

```bash
journalctl -u galexie-archive-mirror.service -n 100 --no-pager
cat /etc/default/galexie-archive-mirror        # DEST_ENDPOINT set?
mc alias list | grep r2-archive                # alias configured?
mc mirror --dry-run local/galexie-archive r2-archive/stellarindex-galexie-archive
```

## Mitigation (≤ 15 min)

- **`configured 0`** — no target. Set `galexie_archive_mirror_s3_endpoint`
  in the host inventory and `galexie_archive_mirror_s3_key` /
  `galexie_archive_mirror_s3_key_secret` in the vault, then apply with
  `--tags backup` (or `galexie-archive-mirror`). `systemctl start
  galexie-archive-mirror.service` starts the first sync.
- **mirror fails (network/credential)** — fix the vars/bucket policy and
  re-apply; the next hourly run retries. `mc mirror` is incremental, so a
  retry after a partial failure resumes rather than restarting.
- **dry-run verification fails** — the script treats a non-zero exit from
  the post-mirror `mc mirror --dry-run` as a failure even with no stdout
  (an auth/network failure can print nothing); check `mc alias list` and
  bucket permissions.
- **stdout still lists objects after mirror** — the sync did not
  converge (quota, throttling); check the target bucket's free space.

## Restore

The archive is append-only, so a restore is: point a fresh MinIO at the
off-site bucket read-only, or `mc mirror` from `r2-archive/<bucket>` back to
a rebuilt `local/galexie-archive`. Re-run
`scripts/ci/galexie-archive-contiguity-test.sh`'s underlying probe
(`galexie-archive-contiguity.sh`) against the restored archive before
pointing ingest at it.

## Root cause analysis

`mc mirror` alone can exit 0 having copied nothing useful if the source
listing is empty or truncated; the post-mirror `mc mirror --dry-run`
verification exists to catch that — see the header of
`scripts/ops/galexie-archive-mirror.sh` for why its exit status and its
stdout are checked separately.

## Known false-positive patterns

- A freshly provisioned host fires until its first full sync completes.

## Related

- [Off-site backup plan](../off-site-backup-plan.md) §1
- [ch-lake-backup](ch-lake-backup.md) — the same inert-mechanism-first precedent for the lake's data backup

## Changelog

- 2026-09-28 — created with the mirror job (NS03).
