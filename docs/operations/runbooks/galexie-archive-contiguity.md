---
title: Runbook — galexie-archive contiguity
last_verified: 2026-08-22
status: ratified
severity: P1 | P3
---

# Runbook — `stellarindex_galexie_archive_gap`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_galexie_archive_gap` (P1, page) · `stellarindex_galexie_archive_contiguity_silent` (P3, ticket) |
| Severity | P1 (an integrity hole in the DR asset) / P3 (the scan itself dark) |
| Detected by | Prometheus rules in `deploy/monitoring/rules/galexie-archive.yml` + `configs/prometheus/rules.r1/galexie-archive.yml` |
| Metric source | `node_exporter` textfile_collector reads `/var/lib/node_exporter/textfile_collector/galexie_archive_contiguity.prom`, refreshed hourly by `galexie-archive-contiguity.timer` → `/usr/local/bin/galexie-archive-contiguity` |
| Steady-state | `galexie_archive_unexpected_gaps == 0`; 221 partitions (2026-08), first_ledger 0, one declared trim hole |
| Customer impact | None while alerting — serving unaffected. The R1 durable mirror (the source the off-site DR copy pulls from) has an integrity hole; a restore in this state would be incomplete. |
| Companions | [galexie-archive-tip-lag](galexie-archive-tip-lag.md), [archive-files-missing](archive-files-missing.md) |

## Why this exists

`galexie-archive` on R1's MinIO is the ADR-0016 durable ledger mirror
and the source the multi-region off-site DR copy is pulled from
(`docs/architecture/multi-region-ha.md` §5). Its **declared shape** is
the genesis partition `[0, 63999]` plus `[ARCHIVE_FROM = 49,984,000 →
tip]`; the middle is a **deliberate capacity trim** (recoverable only
from `aws-public-blockchain`).

The 2026-08-21 DR review found the ratified HA plan claiming a
full-history archive while the bucket held the trimmed shape — drift
nothing recurring could catch: tip-lag proves the newest edge advances,
archive-fill proves the pipe runs, but a partition deleted or corrupted
in the **middle** (bad trim cutoff, manual `mc rm`, MinIO heal failure)
or an overlap from a botched re-fill would surface only at restore
time. The hourly scan lists the ~220 top-level 64k-ledger partition
dirs and counts every discontinuity or overlap that is **not** the
declared trim. Chunk-level holes *inside* a partition are restore-drill
territory (`deploy/monitoring/rules/restore-drill.yml`).

## When `stellarindex_galexie_archive_gap` fires

1. **Freeze the trim.** If `compute-trim-cutoff` / any trim job is
   scheduled, hold it until diagnosed — a mis-computed cutoff deleting
   live partitions is the most likely cause and letting it run again
   widens the damage.
2. **Enumerate the actual shape:**

   ```sh
   mc ls local/galexie-archive/ \
     | grep -oE -- '--[0-9]+-[0-9]+' | sed 's/^--//' | sort -t- -k1,1n
   ```

   Walk for holes/overlaps; compare against the declared trim
   (`EXPECTED_TRIM` in the service env, default `64000-49983999`).
3. **Overlap** (two partitions covering the same ledgers): inspect both
   dirs' object counts/sizes; the newer partial one is usually a botched
   re-fill — remove the incomplete one only after confirming the other
   is whole.
4. **Hole** (a partition missing inside declared coverage): recover from
   `aws-public-blockchain` (the same pull path as the off-site DR plan's
   middle-range copy) via `galexie-archive-fill`'s backfill mode, then
   re-run the scan service and confirm the gauge returns to 0.
5. If the *declared* shape legitimately changed (e.g. the off-site DR
   work backfilled the middle), update `EXPECTED_TRIM` in the service
   env via ansible — set it empty for strict full-history contiguity —
   and update the HA plan doc in the same change.

## When `…_contiguity_silent` fires

The scan stopped emitting (timer fires hourly; absent >3h). Check
`systemctl status galexie-archive-contiguity.timer` and the service
journal; verify root's `local` mc alias can list the bucket
(`mc ls local/galexie-archive/ | head`). A rotated MinIO credential
breaks every mc-based guard at once — check
[galexie-archive-tip-lag](galexie-archive-tip-lag.md)'s staleness alert
for correlation.

## Related

- [galexie-archive-tip-lag](galexie-archive-tip-lag.md) — the newest
  edge advancing; this runbook covers the middle staying intact.
- [archive-files-missing](archive-files-missing.md) — chunk-level
  verification inside partitions.
- `docs/architecture/multi-region-ha.md` §5 — the off-site copy this
  mirror feeds.
