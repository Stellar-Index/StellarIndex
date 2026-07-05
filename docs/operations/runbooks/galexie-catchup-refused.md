---
title: galexie-catchup-refused
last_verified: 2026-07-05
status: current
---

# stellarindex_galexie_catchup_refused (+ stellarindex_host_swap_activity)

## At a glance

- **Severity**: page (the lake tip is frozen; every freshness SLA
  degrades from here).
- **First move**: `systemctl restart galexie` after ruling out a
  version mismatch (§Triage 1) and stopping any unwrapped heavy job.
- **Time to impact**: served data staleness within ~10 minutes;
  verdict/completeness alerts within the hour.

## What fired

The captive stellar-core inside galexie is logging `History: Skipping
catchup: incompatible core version or invalid local state` — it is on
network consensus (buffering new ledgers) but refuses to close the gap
back to the last ledger it delivered, so the lake tip is FROZEN and
every downstream freshness signal will degrade within minutes.

## Why this exists (2026-07-05 incident)

Co-located batch work (a ClickHouse allocator wedge + an unwindowed
ops re-derive that ballooned until the kernel killed it) pushed the
host into swap; the captive core took 1.8G of swap mid-write and its
local state went invalid. It then refused catchup, silently, for 11
hours — the lake stalled ~9,200 ledgers while the alert channel was
flooded by an unrelated deadlock storm.

## Triage

1. Confirm the versions are NOT the problem (they almost never are):
   `stellar-core version` vs the network protocol on any public
   explorer. Mismatch → this is an upgrade task, not a restart.
2. Check the gap: lake tip (`SELECT max(ledger_seq) FROM
   stellar.ledgers` on :8123) vs the `seq=` in recent galexie journal
   lines.
3. Check WHY the state went bad before fixing it: swap activity
   (`stellarindex_host_swap_activity`, `free -g`), OOM kills, disk
   errors. If a heavy job is still running unwrapped, stop it — and
   note that heavy one-shots MUST run under
   `/usr/local/sbin/run-heavy-job.sh` (hard memory cap, no swap).

## Remedy

`systemctl restart galexie` — the captive core discards its wedged
local state and performs a clean bucket catchup from the history
archive (verified integrity; ~5-10 min for buckets), then replays the
gap to consensus and resumes streaming. The lake tip advances again;
the indexer drains automatically. No data is lost — the archive and
MinIO are append-only and the replay is deterministic.

## Prevention already in place

- `galexie.service.d/resources.conf`: MemoryLow=16G + elevated
  CPU/IO weight — the kernel reclaims galexie LAST.
- `run-heavy-job.sh`: mandatory wrapper for ops one-shots —
  MemoryMax=20G, MemorySwapMax=0, batch-class weights.
- `ch-rebuild` refuses unwindowed buffering ranges >2M ledgers.
- The lake-tip freshness rule (data-freshness family) catches the
  symptom independently of this signature.

## Related

- [galexie-archive-tip-lag](galexie-archive-tip-lag.md) — the archive-
  side lag alert (same subsystem, different failure mode).
- [docs/operations/archival-node-bringup.md](../archival-node-bringup.md)
  — the disaster-recovery triage tree if a restart does NOT recover
  (corrupt history archive path).
- `stellarindex_host_swap_activity` shares this runbook: swap is the
  early warning of the pressure class that causes the wedge.
