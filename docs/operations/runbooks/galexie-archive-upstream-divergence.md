---
title: Runbook — Galexie mirror differs from the upstream dataset
last_verified: 2026-10-03
status: draft
severity: P3
---

# Runbook — `stellarindex_galexie_archive_upstream_divergence` / `_upstream_check_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alerts | `stellarindex_galexie_archive_upstream_divergence` (at least one mirrored object differs from the same key upstream, or upstream no longer lists it); `stellarindex_galexie_archive_upstream_check_stale` (no comparison has completed in 15 days, or ever on this host) |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/galexie-archive.yml` and `configs/prometheus/rules.r1/galexie-archive.yml` |
| Producer | `stellarindex-ops galexie-mirror-verify` via `galexie-mirror-verify.timer` (weekly, Sunday 03:41 UTC, under the heavy-job lock) |
| Typical MTTR | 30 min to classify; a re-mirror plus lake rebuild for the affected ledgers can take hours |
| Impact | None immediately. Our copy of a ledger no longer matches the public dataset, so anything decoded from it may differ from what a fresh mirror would produce. |

## Why this exists

`galexie-archive-fill` copies only objects that are absent, and
`verify-archive` checks ledger header hashes. Neither sees an upstream
re-export that rewrites a ledger's meta under the same key after we
mirrored it. `galexie-mirror-verify` compares every object's ETag and
size with upstream; Last-Modified only classifies a difference.

## Symptoms

- `galexie_archive_upstream_objects{result="…"}` is non-zero for
  `upstream-rewritten`, `local-differs` or `local-only`.
- The service journal has one `MISMATCH` line per object, naming its
  partition and ledger:

```text
MISMATCH partition=FC7C95FF--58944000-59007999 ledger=58973375 cause=upstream-rewritten local_etag=… upstream_etag=… local_size=… upstream_size=… local_modified=… upstream_modified=… key=…
```

## Quick diagnosis (≤ 5 min)

```bash
systemctl status galexie-mirror-verify.timer galexie-mirror-verify.service
journalctl -u galexie-mirror-verify.service --no-pager | grep -E 'MISMATCH|galexie-mirror-verify:'
cat /var/lib/node_exporter/textfile_collector/galexie_archive_upstream.prom
```

Read the `cause` of each line:

- **`upstream-rewritten`** — upstream's copy is newer than ours: the
  dataset was re-exported after we mirrored. Our copy is the stale one.
- **`local-differs`** — our copy is newer than upstream's and still
  differs: something rewrote our object (a re-mirror from another
  source, a manual copy, a write fault). Treat ours as suspect.
- **`local-only`** — upstream no longer lists a key we hold in a
  partition both sides have. Usually an upstream deletion; confirm
  before acting.

`missing-local` and `unverifiable` (multipart ETag, equal size) are
informational and never fire the alert.

## Mitigation

To re-run one range by hand (read-only):

```bash
sudo -u stellarindex /usr/local/bin/stellarindex-ops galexie-mirror-verify \
  -config /etc/stellarindex.toml -from 58973375 -to 58973375
```

To replace a stale or suspect object with the upstream version, keep the
old bytes first, then let the rehydrate path copy it back from the cold
tier (it skips keys that already exist, so the old object has to go):

```bash
mc cp local/galexie-archive/<key> /var/tmp/<ledger>.xdr.zst.local
mc rm local/galexie-archive/<key>
sudo -u stellarindex /usr/local/bin/stellarindex-ops rehydrate-galexie-archive \
  -config /etc/stellarindex.toml -from <ledger> -to <ledger> -write
```

Re-run the range check above; it must report `matched`.

Then decide whether the served data needs rebuilding. Decode both
versions of the ledger and compare what changed. If decoded events or
operations differ, rebuild the lake rows for that ledger range and replay
the affected projected domains, following
[the replay decision rule](../../architecture/ingest-pipeline.md#the-replay-decision-rule).
A meta change that decodes identically needs no replay.

## `_upstream_check_stale`

The textfile is written only when a comparison completes, so this fires
when runs keep failing (a listing error against MinIO or the upstream
bucket; the journal names it), when the timer is not firing, or on a host
where no run has completed yet. On a fresh deploy, start the first run by
hand: `systemctl start galexie-mirror-verify.service`.

## Known false-positive patterns

- A freshly deployed host fires `_upstream_check_stale` until its first
  run completes (a full walk can take hours).
- The unit is installed only where `storage.s3_cold_bucket_archive` is
  configured; a host without an upstream has nothing to compare against.

## Related

- [galexie-archive-contiguity](galexie-archive-contiguity.md) — partition-level presence
- [galexie-archive-mirror](galexie-archive-mirror.md) — the off-site copy
