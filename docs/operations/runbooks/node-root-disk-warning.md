---
title: Runbook — node-root-disk-warning
last_verified: 2026-05-12
status: draft
severity: P2
---

# Runbook — `stellaratlas_node_root_disk_warning`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellaratlas_node_root_disk_warning` |
| Severity | P2 (ticket) |
| Detected by | `deploy/monitoring/rules/storage.yml` |
| Typical MTTR | 30–60 min |
| Impact | The host's root filesystem is < 20 % free. No customer impact yet, but ~12 hours of headroom before the **P1** `node_root_disk_full` page fires (which has cascading effects per its runbook). Act now to avoid waking someone at 3 AM. |

## Symptoms

- `(node_filesystem_avail_bytes{mountpoint="/"} / node_filesystem_size_bytes{mountpoint="/"}) * 100 < 20` for ≥ 10 min.
- Trend graph in Grafana shows steady downward slope over recent days.

## Quick diagnosis (≤ 5 min)

Same as `node-root-disk-full.md` § "Quick diagnosis" — what's filling the disk?

```sh
df -h /
sudo du -sh /var/log/* /var/cache/* /var/lib/* 2>/dev/null | sort -hr | head -20
journalctl --disk-usage
```

## Mitigation (≤ 30 min)

This is a warning, not an emergency. Plan the cleanup, don't rush:

- [ ] Step 1 — identify the dominant consumer per the diagnosis above.
- [ ] Step 2 — apply the appropriate cleanup:
  - **Logs** → confirm `15-log-discipline.yml` is current; re-apply if drifted.
  - **Galexie cache** → if galexie-archive bucket has been mounted into root by mistake (it should be on `/var/lib/galexie` per ADR-0016), unmount and remount correctly.
  - **Postgres** → reduce `log_min_duration_statement` if logs are dominant; vacuum old chunks if the data volume is on the same FS (it shouldn't be on R1).
- [ ] Step 3 — schedule a follow-up review in 24 h to confirm the trend reversed.
- [ ] Verification: `node_filesystem_avail_bytes{mountpoint="/"} / node_filesystem_size_bytes{mountpoint="/"} > 0.40` (40% free) sustained for 1 hour.

## Root cause analysis

If this fires more than once a quarter, the disk-usage trend has a leak. Capture for a planning ticket:
- 30-day Grafana trend of `node_filesystem_avail_bytes{mountpoint="/"}`.
- Per-directory growth rate via two `du -sh /var/*` snapshots 7 days apart.

## Known false-positive patterns

- **One-time large captures**: WASM history runs, manual debug captures, and one-shot operator log dumps can take 5–10 GB transiently. If the trigger is identifiable and the data is needed, leave it; otherwise clean up.

## Related

- `node-root-disk-full.md` — the **P1** that fires next if you don't act.
- `db-disk-full.md` — sibling for the postgres data volume (separate FS on R1 per ADR-0016).
- ADR-0008 — HA topology + DR posture.

## Changelog

- 2026-05-12 — initial draft (audit-2026-05-12 F-1237 closure).
