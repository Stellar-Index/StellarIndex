# Runbook: galexie-archive contiguity

**Alerts:** `stellarindex_galexie_archive_gap` (page) ·
`stellarindex_galexie_archive_contiguity_silent` (ticket)

## What this guards

`galexie-archive` on R1's MinIO is the ADR-0016 durable ledger mirror and
the source the multi-region off-site DR copy is pulled from
(`docs/architecture/multi-region-ha.md` §5). Its **declared shape** is:

- the genesis partition `[0, 63999]`, plus
- `[ARCHIVE_FROM = 49,984,000 → tip]`.

The middle `[64,000 – 49,983,999]` is a **deliberate capacity trim**
(recoverable only from `aws-public-blockchain`). The hourly
`galexie-archive-contiguity.timer` runs
`/usr/local/bin/galexie-archive-contiguity`, which lists the ~220
top-level 64k-ledger partition dirs and counts every discontinuity or
overlap that is **not** the declared trim, emitting
`galexie_archive_unexpected_gaps` (plus `partition_count`,
`first_ledger`, `last_ledger`) via the node_exporter textfile collector.

Complementary guards: `galexie-archive-tip-lag` (#31) proves the newest
edge advances; `galexie-archive-fill` runs the pipe. Neither can see a
partition deleted or corrupted in the middle — that is what this scan
exists for. Chunk-level holes *inside* a partition are restore-drill
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
breaks every mc-based guard at once — check tip-lag's staleness alert
for correlation.
