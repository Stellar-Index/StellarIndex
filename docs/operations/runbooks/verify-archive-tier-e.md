---
title: Runbook — verify-archive-tier-e
last_verified: 2026-09-26
status: ratified
severity: P3
---

# Runbook — `stellarindex_verify_archive_tier_e_run_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_verify_archive_tier_e_run_stale` |
| Severity | P3 (ticket — defence-in-depth behind Tier A page / Tier B ticket) |
| Detected by | Prometheus rules in `deploy/monitoring/rules/verify-archive.yml` (`stellarindex_verify_archive_last_success_unix{tier="archivist"}`, a node_exporter textfile) |
| Typical MTTR | 30 min (diagnosis) – hours (a 48h-timeout scan re-run) |
| Impact | None immediate — the API serves correct data from existing bytes. A stale Tier E means the local `/srv/history-archive` mirror hasn't had its own bytes re-hashed recently; bit rot there would still pass Tier B, which anchors against that mirror's manifest instead of re-hashing it. |

## What Tier E is

Tier E (`stellarindex-ops verify-archive -tier archivist`) runs
`stellar-archivist scan --verify` over the local `/srv/history-archive`
mirror — every bucket's sha256, re-hashed from the bytes on disk. It is the
only tier that checks the mirror's own integrity rather than trusting its
manifest: Tier B's checkpoint anchor reads that manifest, so corruption in
it would pass Tier B silently (ADR-0016 §7.4 counts Tier E in R1's
integrity guarantee; R2/R3 delegate it to R1).

Unlike Tier A/B, Tier E is installed as a monthly **cron** entry
(`stellarindex-verify-archive-tier-e`, the 15th at 12:43 UTC —
`configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml`),
not a systemd timer, so there is no `node_systemd_unit_state` series to
alert a "run failed" pair off of. Staleness on
`stellarindex_verify_archive_last_success_unix{tier="archivist"}` is the
only monitored signal, covering both "every recent scan failed" and "the
cron entry was never installed" (gated the same as Tier B —
`verify_archive_tier_b_enabled`, pubnet-only local mirror).

The scan can take hours on r1-class disk; `-archivist-timeout 48h` bounds
a hung run, not a slow one.

## Symptoms

- `stellarindex_verify_archive_tier_e_run_stale`: no clean exit recorded in
  over 35 days (one missed monthly cycle plus slack) — or never recorded
  one.

## Quick diagnosis (≤ 5 min)

```sh
# Is the cron entry installed?
ssh r1 'crontab -l -u root | grep verify-archive-tier-e'

# What did the last run log? (run-heavy-job.sh pipes through
# `logger -t stellarindex-tier-e`)
ssh r1 'journalctl -t stellarindex-tier-e --since "-45 days" --no-pager | tail -80'

# Is the local mirror present at all?
ssh r1 'ls /srv/history-archive/ledger | head; df -h /srv/history-archive'
```

| Pattern | Cause |
| ------- | ----- |
| `archivist scan FAILED` | `stellar-archivist scan --verify` exited non-zero — a bucket's sha256 didn't match. Single-source corruption in the local mirror; escalate per the Tier B RCA (bit rot Tier B cannot see). |
| `archivist scan timed out after 48h0m0s` | Runtime bound hit — investigate disk I/O contention with the nightly heavy-job band (`run-heavy-job.sh` serialises Tier D/E under one lock name so they don't overlap it, but a slow disk can still exceed 48h). |
| no log entry at all in the window | The cron entry isn't installed, or `run-heavy-job.sh`'s lock was held by another heavy job for the entire window. Check `verify_archive_tier_b_enabled` in inventory — Tier E is gated the same as Tier B (pubnet-only local mirror). |

## Mitigation

- [ ] **`archivist scan FAILED`**: STOP — this is single-source corruption
  in `/srv/history-archive`, the same escalation as a Tier B anchor
  MISMATCH. Do not auto-recover; compare against a peer archive and see
  [verify-archive-tier-b](verify-archive-tier-b.md).
- [ ] **Cron entry missing**: re-run the archival-node role's
  `14-stellarindex-services.yml` tasks (tag `ops-jobs`) with
  `verify_archive_tier_b_enabled: true` in inventory.
- [ ] **Timeout**: re-run by hand outside the heavy-job window:
  ```sh
  ssh r1
  set -a; source /etc/default/stellarindex-ops; set +a
  /usr/local/bin/stellarindex-ops verify-archive \
    -config /etc/stellarindex.toml \
    -tier archivist -archive-root /srv/history-archive \
    -archivist-timeout 48h
  ```
- [ ] **Verification**: the next scheduled run (or the hand re-run above)
  completes cleanly and advances
  `stellarindex_verify_archive_last_success_unix{tier="archivist"}`.

## Related

- [verify-archive-tier-b](verify-archive-tier-b.md) — the checkpoint
  anchor Tier E's re-hash backstops; shares the same pubnet-only gating.
- [verify-archive-run-stale](verify-archive-run-stale.md) — the Tier A
  staleness page (the higher-urgency chain-integrity counterpart).
- ADR-0016 §7.4 — Tier E in R1's periodic integrity guarantee.
- GH-726 — scheduling and alerting gap this runbook closes.

## Changelog

- 2026-09-26 — initial draft alongside the Tier E staleness alert.
