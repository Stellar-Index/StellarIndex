---
title: galexie-catchup-refused
last_verified: 2026-08-29
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

## Applying galexie config with ansible — the restart ack

A galexie restart is never casual on r1: the captive core cold-catches-up
for ~9 minutes on mainnet, and every re-restart starts that clock again
(the 2026-08-27 postmortem). On 2026-08-29 06:04Z an apply with
`--tags users,minio,galexie` restarted a healthy galexie for a change to
the `galexie-append.sh` wrapper — a file systemd exec's once per service
start and the running process never reads — and the preceding
`--check --diff` had shown no handler, because the old effective-change
gate only compared before/after a real write. The archival-node role now
behaves as follows (`tasks/galexie-effective-checksum.yml`):

- **Only inputs the running process has loaded at start can restart it:**
  `/etc/stellar/captive-core-galexie.cfg`, `/etc/galexie/galexie.toml`,
  `/etc/default/galexie` (systemd `EnvironmentFile`), and
  `/etc/systemd/system/galexie.service`. A galexie binary rebuild
  (`galexie_version` bump) restarts too. Nothing else does — not the
  `galexie-append.sh` wrapper, not the archive-fill / tip-lag /
  contiguity scripts and timers, not the SDF apt key. A wrapper edit is
  picked up at the next (re)start; if it is needed sooner, restart by
  hand in a maintenance window.
- **Only an effective change restarts:** the on-disk file is compared with
  what the run would render, comments and blank lines stripped. A comment
  re-wrap applies silently; a value change is a restart.
- **The dry-run tells you first:** `ansible-playbook --check --diff` reaches
  the same verdict and prints `RUNNING HANDLER [archival-node : Restart
  galexie]` when a real apply would restart. (The weekly ansible-drift job
  runs `--check --diff` too, so a pending restart-class change shows up
  there as drift — apply it, with the ack, in a window.)
- **Fail-closed without an ack:** when galexie is active and a restart is
  required, a real apply FAILS before writing anything:

  ```text
  A real galexie restart is required by an effective config change in
  /etc/galexie/galexie.toml and galexie is active … re-run in a
  maintenance window with `-e galexie_restart_ack=true`
  ```

  Nothing is left on disk unapplied (a rotated S3 key pair is either
  fully applied — rendered and loaded — or not at all). Re-run in a
  maintenance window:

  ```bash
  ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
    --tags galexie -e galexie_restart_ack=true
  ```

  then watch the tip for ~10 minutes (§Remedy) and do not re-restart
  mid-catchup. `galexie_restart_ack` defaults to `false` in
  `roles/archival-node/defaults/main.yml`; never set it in inventory.
  A stopped galexie (bootstrap, or already down) needs no ack — the
  "restart" is just a start.

## Related

- [galexie-archive-tip-lag](galexie-archive-tip-lag.md) — the archive-
  side lag alert (same subsystem, different failure mode).
- [docs/operations/archival-node-bringup.md](../archival-node-bringup.md)
  — the disaster-recovery triage tree if a restart does NOT recover
  (corrupt history archive path).
- `stellarindex_host_swap_activity` shares this runbook: swap is the
  early warning of the pressure class that causes the wedge.
