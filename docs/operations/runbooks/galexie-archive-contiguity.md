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
| Alerts | `stellarindex_galexie_archive_gap` (P1, page) · `stellarindex_galexie_archive_contiguity_silent` (P3, ticket) · `stellarindex_galexie_archive_scan_degraded` (P3, ticket) |
| Severity | P1 (an integrity hole in the DR asset) / P3 (the scan itself dark, or unable to read the bucket) |
| Detected by | Prometheus rules in `deploy/monitoring/rules/galexie-archive.yml` + `configs/prometheus/rules.r1/galexie-archive.yml` |
| Metric source | `node_exporter` textfile_collector reads `/var/lib/node_exporter/textfile_collector/galexie_archive_contiguity.prom`, refreshed hourly by `galexie-archive-contiguity.timer` → `/usr/local/bin/galexie-archive-contiguity` |
| Steady-state | `galexie_archive_unexpected_gaps == 0`; 225 partitions (2026-09-05), first_ledger 0, one declared trim hole; `galexie_archive_scan_ok == 1` and `galexie_archive_scan_last_run_unix` within the hour |
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

## When `…_scan_degraded` fires

The scan is running but is not producing a verdict you can trust, so
`stellarindex_galexie_archive_gap` above is evaluating over a series
that is absent or frozen. Silence from it means nothing while this is
firing.

The scan reads the bucket once per run and keeps that read's exit
status. Until 2026-09-05 it did not: `mc ls … || true` threw the status
away, so a read that died PARTWAY handed the parser a truncated but
perfectly contiguous prefix and the walk certified a clean mirror from a
read that never finished — `galexie_archive_unexpected_gaps 0`,
byte-identical to the healthy answer. A read that died having emitted
nothing went the other way and paged with a DR-corruption claim about a
bucket nobody managed to list. Neither is published now: a run that
could not look publishes no partition verdict at all, and these three
series say so.

| Series | Reading | What it means |
| ------ | ------- | ------------- |
| `galexie_archive_scan_ok` | 0 | The bucket listing errored. Check root's `local` mc alias, MinIO reachability and credentials — a rotated MinIO credential breaks every mc-based guard at once. |
| `galexie_archive_scan_last_run_unix` | older than 3 h, or absent | The scan or its timer is not running. `node_exporter` keeps serving the last file it saw, so the verdict stays present and stops moving; this stamp is the only thing that ages. |
| `galexie_archive_scan_listing_lines` | — | Diagnostic, not an alert arm. When a SUCCESSFUL read yields zero partitions the scan still pages; this says whether the bucket is really empty (0) or answered in a shape the parser no longer matches (> 0), which is a very different morning. |

Start with `systemctl status galexie-archive-contiguity.timer` and the
file's mtime, then run `/usr/local/bin/galexie-archive-contiguity` by
hand and read
`/var/lib/node_exporter/textfile_collector/galexie_archive_contiguity.prom`.
The scan is proven against these failure shapes by
`scripts/ci/galexie-archive-contiguity-test.sh`, which executes the
shipped script against a stubbed `mc`.

## Related

- [galexie-archive-tip-lag](galexie-archive-tip-lag.md) — the newest
  edge advancing; this runbook covers the middle staying intact.
- [archive-files-missing](archive-files-missing.md) — chunk-level
  verification inside partitions.
- `docs/architecture/multi-region-ha.md` §5 — the off-site copy this
  mirror feeds.
