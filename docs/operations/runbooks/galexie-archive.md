---
title: Runbook — galexie-archive
last_verified: 2026-10-06
status: current
severity: P1 | P3
---

# Runbook — galexie-archive family

Alerts for the R1 durable ledger mirror (`galexie-archive` bucket on MinIO, ADR-0016), the galexie captive core that feeds the lake, and host swap pressure. Rules live in `configs/prometheus/rules.r1/galexie-archive.yml` (what r1 loads) and `deploy/monitoring/rules/galexie-archive.yml`; the two files are identical. Rule `severity: page` = P1, `severity: ticket` = P3 (routed by `configs/alertmanager/alertmanager.r1.yml`).

## At a glance

- [`stellarindex_galexie_archive_tip_lag_high`](#stellarindex_galexie_archive_tip_lag_high)
- [`stellarindex_galexie_archive_tip_lag_severe`](#stellarindex_galexie_archive_tip_lag_severe)
- [`stellarindex_galexie_archive_tip_lag_metric_stale`](#stellarindex_galexie_archive_tip_lag_metric_stale)
- [`stellarindex_galexie_archive_gap`](#stellarindex_galexie_archive_gap)
- [`stellarindex_galexie_archive_contiguity_silent`](#stellarindex_galexie_archive_contiguity_silent)
- [`stellarindex_galexie_archive_scan_degraded`](#stellarindex_galexie_archive_scan_degraded)
- [`stellarindex_galexie_archive_upstream_divergence`](#stellarindex_galexie_archive_upstream_divergence)
- [`stellarindex_galexie_archive_upstream_check_stale`](#stellarindex_galexie_archive_upstream_check_stale)
- [`stellarindex_galexie_catchup_refused`](#stellarindex_galexie_catchup_refused)
- [`stellarindex_galexie_catchup_probe_degraded`](#stellarindex_galexie_catchup_probe_degraded)
- [`stellarindex_host_swap_activity`](#stellarindex_host_swap_activity)

## Shared context

- Archive alerts: no customer impact (serving unaffected; `aws-public-blockchain` backstops); the mirror and the off-site DR copy (`docs/architecture/multi-region-ha.md` §5) degrade.
- Scope: r1 / pubnet. `galexie-archive-fill` is installed only when `stellar_network == pubnet` (aws-public has no testnet/futurenet dataset) and removed elsewhere; the tip-lag updater is on every network, so fill remediation does not apply off pubnet.
- Partition model: galexie writes 64,000 ledgers per partition (`files_per_partition=64000`, `ledgers_per_file=1`); the fill mirrors only COMPLETE partitions. Archive tip lag is a sawtooth 0 → 64,000 (~4 days per partition, back to ~0 on the first hourly fill after a partition completes).
- Declared archive shape: genesis partition `[0, 63999]` plus `[ARCHIVE_FROM = 49,984,000 → tip]`; the middle is a deliberate capacity trim (recoverable only from `aws-public-blockchain`).
- Every mc-based guard reads through root's `local` mc alias; a rotated MinIO credential breaks all of them at once. The `aws-public` alias lives only in root's `~/.mc/config.json` and is not ansible-managed.
- Probe-blindness alerts (`_metric_stale`, `_contiguity_silent`, `_scan_degraded`, `_upstream_check_stale`, `_catchup_probe_degraded`) are `ticket`: node_exporter serves the LAST textfile forever, so a dead producer freezes the guarded alert. Fix the producer first.
- Timers and units: `galexie-archive-fill.timer` (hourly, :17 + jitter), `galexie-archive-tip-lag.timer` (5 min), `galexie-archive-contiguity.timer` (hourly), `galexie-mirror-verify.timer` (weekly), `galexie-archive-trim.timer`, `galexie-catchup-probe.timer`. Sources: `configs/ansible/roles/archival-node/templates/systemd/*.j2`, installed by `tasks/07-galexie.yml` (the `deploy/systemd/` copies have drifted and are not what runs on r1).
- Read a oneshot's `Result` WITH its timestamp (`Type=oneshot RemainAfterExit=no` units keep `Result` from the last run; empty `InactiveEnterTimestamp` = never ran):

  ```sh
  ssh r1 'systemctl show galexie-archive-fill.service -p Result,InactiveEnterTimestamp,ExecMainStartTimestamp'
  ```

## stellarindex_galexie_archive_tip_lag_high

- Trips: `galexie_archive_tip_lag_ledgers > 64000`, `for: 90m`, `severity: ticket`.
- Means: one completed partition stayed un-mirrored across two hourly fill cycles; the fill timer is slow or partially failing. Do not wait further cycles.
- Diagnose: `systemctl status galexie-archive-fill.service`, last run in `/var/log/galexie-mirror.log` (`needs work (total): 0` = healthy; also read `missing entirely:`, `incomplete (queued by Phase 1b):`, `below hot floor (...)`), `mc admin info local` for MinIO load, aws-public listing latency.
- Fix: see Remediation below.

## stellarindex_galexie_archive_tip_lag_severe

- Trips: `galexie_archive_tip_lag_ledgers > 128000`, `for: 30m`, `severity: page`.
- Means: >= 2 completed partitions (~8 days) behind; the fill timer has broken, the 23-day-stall failure class. Backfills and WASM-walks past the gap fail for ledgers above the hot floor (below it the ADR-0027 cold tier serves reads).
- Fix: `sudo systemctl start galexie-archive-fill.service`, then `journalctl -u galexie-archive-fill.service -n 100` and tail `/var/log/galexie-mirror.log` for the live error; then root-cause the systemd timer/service.

Remediation (both lag alerts):

```sh
# Manual catch-up (idempotent; same unit the timer runs, via run-heavy-job.sh:
# singleton flock, MemoryMax=20G, disk watchdog, 6h TimeoutStartSec):
ssh r1 'sudo systemctl start galexie-archive-fill.service'
# Foreground run with the same guards (never invoke the script bare: it races
# the :17 timer on the shared /tmp/galexie-fill.*.txt scratch files, uncapped):
ssh r1 'sudo /usr/local/sbin/run-heavy-job.sh galexie-archive-fill /usr/local/bin/galexie-archive-fill'
# Force the tip-lag updater to re-read:
ssh r1 'sudo systemctl start galexie-archive-tip-lag.service'
# Re-enable timers if disabled:
ssh r1 'sudo systemctl enable --now galexie-archive-fill.timer galexie-archive-tip-lag.timer'
```

If the fill itself fails, follow `/var/log/galexie-mirror.log`. Common causes: bucket permission denied (rotate the mc alias creds), aws-public listing 503s (retry in an hour), MinIO local out of space (`zpool list -o name,size,alloc,free,cap data`). Partitions ending below the hot floor are intentionally not mirrored (ADR-0027 cold tier serves them; `galexie-archive-trim.timer` removes them), so a "missing" partition below the floor is not a fill failure. The floor is the larger of `ARCHIVE_HOT_FLOOR` in `/etc/default/galexie-archive-fill` (49,984,000 on r1) and the value `compute-trim-cutoff.sh` persists to `/var/lib/galexie-archive/hot-floor`.

## stellarindex_galexie_archive_tip_lag_metric_stale

- Trips: `galexie_archive_tip_lag_probe_success == 0 or time() - galexie_archive_tip_lag_updated_seconds > 1800`, `for: 15m`, `severity: ticket`.
- Means: either the last run could not read a bucket tip (failed `mc ls`, no partition, or no parseable object; the script then OMITS the lag rather than publishing 0, so the two lag alerts are blind), or the timer has not updated the textfile for >30 min (it fires every 5 min). Restore this metric first so the lag alerts can fire honestly.
- Diagnose:

  ```sh
  ssh r1 'journalctl -u galexie-archive-tip-lag.service -n 50'   # mc-alias misconfig, permissions, MinIO down
  ssh r1 'ls -la /var/lib/node_exporter/textfile_collector/galexie_archive_tip_lag.prom'
  ssh r1 'cat /var/lib/node_exporter/textfile_collector/galexie_archive_tip_lag.prom'
  ssh r1 'systemctl list-timers galexie-archive-fill.timer galexie-archive-tip-lag.timer'
  ssh r1 'sudo /usr/local/bin/galexie-archive-tip-lag --self-test'   # pins the object-name parser (single-ledger and range forms, .zst/.zstd); no mc alias needed
  # Manual gap check (partition names are reverse-hex prefixed, first row = newest):
  ssh r1 'mc ls local/galexie-live | head -1; mc ls local/galexie-archive | head -1'
  ```

- Metrics: `galexie_archive_tip_ledger`, `galexie_live_tip_ledger`, `galexie_archive_tip_lag_ledgers`, `galexie_archive_tip_lag_updated_seconds`, `galexie_archive_tip_lag_probe_success`, from `/usr/local/bin/galexie-archive-tip-lag` via `galexie_archive_tip_lag.prom`.

## stellarindex_galexie_archive_gap

- Trips: `galexie_archive_unexpected_gaps > 0`, `for: 1h`, `severity: page`.
- Means: the mirror's top-level partition coverage no longer matches its declared shape (a partition deleted or corrupted in the middle by a bad trim cutoff, manual `mc rm` or MinIO heal failure, or an overlap from a botched re-fill); only the declared trim hole is excluded. Producer: hourly `galexie-archive-contiguity.timer` -> `/usr/local/bin/galexie-archive-contiguity` -> `/var/lib/node_exporter/textfile_collector/galexie_archive_contiguity.prom`. Steady state: gauge 0, `scan_ok == 1`, `scan_last_run_unix` within the hour. Chunk-level holes inside a partition are restore-drill territory (`deploy/monitoring/rules/restore-drill.yml`; see [archive-files-missing](archive-completeness.md#stellarindex_archive_files_missing)).
- Fix:
  1. Freeze the trim: hold `compute-trim-cutoff` / any trim job until diagnosed (a mis-computed cutoff deleting live partitions is the likeliest cause).
  2. Enumerate the shape and walk it for holes/overlaps against the declared trim (`EXPECTED_TRIM` in the service env, default `64000-49983999`):

     ```sh
     mc ls local/galexie-archive/ \
       | grep -oE -- '--[0-9]+-[0-9]+' | sed 's/^--//' | sort -t- -k1,1n
     ```

  3. Overlap: compare both dirs' object counts/sizes; the newer partial one is usually a botched re-fill. Remove the incomplete one only after confirming the other is whole.
  4. Hole inside declared coverage: recover from `aws-public-blockchain` (same pull path as the off-site DR middle-range copy) with `galexie-archive-fill` (it copies absent partitions; incomplete ones go in via the `PARTIALS` env), then re-run the scan service and confirm the gauge returns to 0. A hole below the hot floor is skipped by the fill (see above).
  5. If the declared shape legitimately changed (e.g. the middle was backfilled), update `EXPECTED_TRIM` in the service env via ansible (empty = strict full-history contiguity) and the HA plan doc in the same change.

## stellarindex_galexie_archive_contiguity_silent

- Trips: `absent_over_time(galexie_archive_unexpected_gaps[3h])`, `for: 15m`, `severity: ticket`.
- Means: the scan stopped emitting (timer fires hourly), so the gap guard is blind.
- Diagnose: `systemctl status galexie-archive-contiguity.timer`, the service journal, and `mc ls local/galexie-archive/ | head` as root (the `local` alias must list the bucket). Correlate with `_tip_lag_metric_stale` (a rotated credential breaks both).

## stellarindex_galexie_archive_scan_degraded

- Trips:

  ```
  galexie_archive_scan_ok == 0
  or
  (time() - galexie_archive_scan_last_run_unix) > 10800
  or
  absent_over_time(galexie_archive_scan_last_run_unix[6h])
  ```

  `for: 15m`, `severity: ticket`.
- Means: the scan runs but gives no usable verdict, so `stellarindex_galexie_archive_gap` evaluates over an absent or frozen series; its silence means nothing. The scan keeps the exit status of its single bucket read and publishes no partition verdict for a run that could not look (a partial read would otherwise certify a clean mirror from a truncated listing).

  | Series | Reading | Meaning |
  | ------ | ------- | ------- |
  | `galexie_archive_scan_ok` | 0 | Bucket listing errored: `local` mc alias, MinIO reachability, credentials. |
  | `galexie_archive_scan_last_run_unix` | older than 3 h, or absent | Scan or timer not running; node_exporter keeps serving the last file, only this stamp ages. |
  | `galexie_archive_scan_listing_lines` | (diagnostic, not an arm) | When a SUCCESSFUL read yields zero partitions the gap alert still pages; 0 = bucket really empty, > 0 = listing shape the parser no longer matches. |

- Diagnose: `systemctl status galexie-archive-contiguity.timer` and the file's mtime, then run `/usr/local/bin/galexie-archive-contiguity` by hand and read `/var/lib/node_exporter/textfile_collector/galexie_archive_contiguity.prom`. Test: `scripts/ci/galexie-archive-contiguity-test.sh` (runs the shipped script against a stubbed `mc`).

## stellarindex_galexie_archive_upstream_divergence

- Trips: `sum(galexie_archive_upstream_objects{result=~"upstream-rewritten|local-differs|local-only"}) > 0`, `for: 15m`, `severity: ticket`.
- Means: the last `stellarindex-ops galexie-mirror-verify` run (weekly `galexie-mirror-verify.timer`, Sunday 03:41 UTC, under the heavy-job lock; installed only where `storage.s3_cold_bucket_archive` is configured) found mirrored objects that differ from the same key upstream, or that upstream no longer lists. The fill copies only absent objects and `verify-archive` checks header hashes, so neither sees an upstream re-export; this compares ETag and size (Last-Modified only classifies). Anything decoded from a differing ledger may differ from a fresh mirror.
- Diagnose (MTTR 30 min to classify; re-mirror plus lake rebuild can take hours):

  ```bash
  systemctl status galexie-mirror-verify.timer galexie-mirror-verify.service
  journalctl -u galexie-mirror-verify.service --no-pager | grep -E 'MISMATCH|galexie-mirror-verify:'
  cat /var/lib/node_exporter/textfile_collector/galexie_archive_upstream.prom
  ```

  Each journal line: `MISMATCH partition=<p> ledger=<n> cause=<c> local_etag=… upstream_etag=… local_size=… upstream_size=… local_modified=… upstream_modified=… key=…`. Causes:
  - `upstream-rewritten`: upstream's copy is newer (re-exported after we mirrored); ours is stale.
  - `local-differs`: our copy is newer than upstream's and still differs; something rewrote our object (re-mirror from another source, manual copy, write fault). Treat ours as suspect.
  - `local-only`: upstream no longer lists a key we hold in a partition both sides have; usually an upstream deletion, confirm before acting.

  `missing-local` and `unverifiable` (multipart ETag, equal size) are informational and never fire the alert.
- Fix: re-run one range by hand (read-only):

  ```bash
  sudo -u stellarindex /usr/local/bin/stellarindex-ops galexie-mirror-verify \
    -config /etc/stellarindex.toml -from 58973375 -to 58973375
  ```

  To replace a stale or suspect object with the upstream version, keep the old bytes, then let the rehydrate path copy it back from the cold tier (it skips keys that already exist, so the old object must go):

  ```bash
  mc cp local/galexie-archive/<key> /var/tmp/<ledger>.xdr.zst.local
  mc rm local/galexie-archive/<key>
  sudo -u stellarindex /usr/local/bin/stellarindex-ops rehydrate-galexie-archive \
    -config /etc/stellarindex.toml -from <ledger> -to <ledger> -write
  ```

  Re-run the range check; it must report `matched`. Then decode both versions and compare: if decoded events or operations differ, rebuild the lake rows for that range and replay the affected projected domains per [the replay decision rule](../../architecture/ingest-pipeline.md#the-replay-decision-rule); a meta change that decodes identically needs no replay.

## stellarindex_galexie_archive_upstream_check_stale

- Trips:

  ```
  (time() - galexie_archive_upstream_last_success_unix) > 1296000
  or
  absent_over_time(galexie_archive_upstream_last_success_unix[1h]) == 1
  ```

  `for: 1h`, `severity: ticket`. 1296000 s = 15 days (two missed weekly runs plus slack).
- Means: the textfile is written only when a comparison completes, so runs keep failing (listing error against MinIO or the upstream bucket; the journal names it), the timer is not firing, or (absent arm) no run has ever completed on this host. While it fires an upstream re-export goes unnoticed. A fresh deploy fires it until the first run completes (a full walk can take hours); start it by hand: `systemctl start galexie-mirror-verify.service`.
- Diagnose: `systemctl status galexie-mirror-verify.timer` and the service journal.

## stellarindex_galexie_catchup_refused

- Trips: `stellarindex_galexie_catchup_refusals_5m > 0`, `for: 10m`, `severity: page`.
- Means: the captive stellar-core inside galexie logs `History: Skipping catchup: incompatible core version or invalid local state`. It is on consensus (buffering new ledgers) but refuses to close the gap back to the last ledger it delivered, so the lake tip is FROZEN: served-data staleness within ~10 min, verdict/completeness alerts within the hour. The usual cause is host swap pressure (memory-hungry co-located batch work) corrupting the core's local state mid-write.
- Triage:
  1. Rule out a version mismatch (almost never the cause): `stellar-core version` vs the network protocol on a public explorer. Mismatch means an upgrade task, not a restart.
  2. Check the gap: lake tip (`SELECT max(ledger_seq) FROM stellar.ledgers` on :8123) vs the `seq=` in recent galexie journal lines.
  3. Find WHY the state went bad: swap activity ([stellarindex_host_swap_activity](#stellarindex_host_swap_activity), `free -g`), OOM kills, disk errors. Stop any unwrapped heavy job; heavy one-shots MUST run under `/usr/local/sbin/run-heavy-job.sh`.
- Fix: `systemctl restart galexie`. The core discards its wedged state, does a clean bucket catchup from the history archive (~5-10 min for buckets), replays the gap and resumes; the indexer drains automatically. No data is lost (archive and MinIO are append-only, replay is deterministic). If a restart does not recover, see [archival-node-bringup](../archival-node-bringup.md).
- Prevention in place: `galexie.service.d/resources.conf` (MemoryLow=16G plus elevated CPU/IO weight, so the kernel reclaims galexie LAST); `run-heavy-job.sh` (MemoryMax=20G, MemorySwapMax=0, batch-class weights); `ch-rebuild` refuses unwindowed buffering ranges >2M ledgers; the lake-tip freshness rule (data-freshness family) catches the symptom independently.

### Applying galexie config with ansible (restart ack)

A galexie restart is never casual on r1: the captive core cold-catches-up ~9 min on mainnet and every re-restart restarts that clock. Role behaviour (`tasks/galexie-effective-checksum.yml`):

- Only inputs the running process loaded at start restart it: `/etc/stellar/captive-core-galexie.cfg`, `/etc/galexie/galexie.toml`, `/etc/default/galexie`, `/etc/systemd/system/galexie.service`, and a galexie binary rebuild (`galexie_version` bump). Not the `galexie-append.sh` wrapper (exec'd once per start; restart by hand in a window to pick up an edit), the archive-fill / tip-lag / contiguity scripts and timers, or the SDF apt key.
- Only an effective change restarts: the on-disk file is compared with what the run would render, comments and blank lines stripped.
- `ansible-playbook --check --diff` prints `RUNNING HANDLER [archival-node : Restart galexie]` when a real apply would restart (the weekly ansible-drift job shows it as drift).
- With galexie active and a restart required, a real apply FAILS before writing anything (`… re-run in a maintenance window with -e galexie_restart_ack=true`). Re-run in a window, then watch the tip ~10 min and do not re-restart mid-catchup:

  ```bash
  ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
    --tags galexie -e galexie_restart_ack=true
  ```

  `galexie_restart_ack` defaults to `false` in `roles/archival-node/defaults/main.yml`; never set it in inventory. A stopped galexie needs no ack.

## stellarindex_galexie_catchup_probe_degraded

- Trips:

  ```
  stellarindex_galexie_catchup_probe_read_ok == 0
  or
  stellarindex_galexie_catchup_probe_journal_lines == 0
  or
  (time() - stellarindex_galexie_catchup_probe_last_run_unix) > 600
  or
  absent_over_time(stellarindex_galexie_catchup_probe_last_run_unix[30m])
  ```

  `for: 15m`, `severity: ticket`. 600 s = ten missed ticks of the 60 s timer.
- Means: the probe behind `stellarindex_galexie_catchup_refused` (counts `Skipping catchup` lines in the last 5 min of galexie's journal, writing `/var/lib/node_exporter/textfile_collector/galexie_catchup.prom`) is not producing usable metrics, so the page evaluates over an absent or frozen series. `grep -c` prints 0 and exits 1 on no match (the healthy outcome), so the probe keeps the journal read's own status and publishes three series about itself:

  | Series | Reading | Meaning |
  | ------ | ------- | ------- |
  | `stellarindex_galexie_catchup_probe_read_ok` | 0 | Journal read errored (`journalctl` missing, unreadable journal); no refusal count is published for that run (a fabricated 0 would assert "no refusals"). |
  | `stellarindex_galexie_catchup_probe_journal_lines` | 0 | Read succeeded but returned nothing: unit name no longer matches, or galexie is not running. Galexie logs 364-409 lines per 5 min on r1, so 0 is never healthy while it is up. |
  | `stellarindex_galexie_catchup_probe_last_run_unix` | older than 10 min, or absent | Probe or timer not running; node_exporter serves the last file, only this stamp ages. |

- Diagnose: `systemctl status galexie-catchup-probe.timer` and the file's mtime, then run `/usr/local/sbin/galexie-catchup-probe.sh` by hand and read the `.prom` above. Test: `scripts/ci/galexie-catchup-probe-test.sh` (stubbed `journalctl`).
- While firing, detection of a wedged core is not lost, only the alert naming it: a frozen lake stalls `stellarindex_cursor_last_ledger` and pages via `stellarindex_ingestion_ledger_stalled` in ~10 min. Remedy is then the `_catchup_refused` Fix.

## stellarindex_host_swap_activity

- Trips: `rate(node_vmstat_pswpout[10m]) > 100`, `for: 15m`, `severity: ticket` (`component: infra`, group `stellarindex.galexie_archive_tip_lag`).
- Means: pages are being written OUT to swap (anonymous memory evicted, not just page cache) for 15+ min. r1 has 16 G swap at `vm.swappiness=1` (`configs/ansible/roles/archival-node/defaults/main.yml`, `sysctl_tunings`), so any sustained pswpout is a violated memory budget. Every heavy one-shot must run under `/usr/local/sbin/run-heavy-job.sh` (`MemoryMax=20G` with `MemorySwapMax=0`: killed, never swapped) and galexie carries `MemoryLow=16G` (both in `configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml`); swapping means something OUTSIDE those fences. Not customer-visible alone, but it is the early warning for the pressure that wedges the captive core ([_catchup_refused](#stellarindex_galexie_catchup_refused)). Often arrives with `host-memory-high` and `host-cpu-high`. MTTR 15 min (stop the job) to hours (re-plan).
- Diagnose:

  ```sh
  ssh root@136.243.90.96 'vmstat 1 5; free -g'
  # processes holding swap, largest last
  ssh root@136.243.90.96 'for p in /proc/[0-9]*; do s=$(awk "/VmSwap/{print \$2}" $p/status 2>/dev/null); [ -n "$s" ] && [ "$s" -gt 0 ] && echo "$s KB $(cat $p/comm)"; done | sort -n | tail -20'
  # a scoped job cannot swap, so a swapper is outside the fence
  ssh root@136.243.90.96 'systemctl status "heavy-*.scope" --no-pager; systemd-cgtop --order=memory --iterations=2 -n 20'
  ```

- Causes and fixes:
  1. Heavy ops job run raw: stop it, re-run under `run-heavy-job.sh`.
  2. Service leaking heap (RSS climbs monotonically over hours/days): pprof heap dump, restart the unit, file an incident.
  3. Postgres `work_mem` x concurrent backends: see [host-memory-high](infra.md#stellarindex_host_memory_high) (lower `work_mem` / cap `max_connections` via the ansible role; brief served-tier outage).
  4. Genuine undersize: a capacity decision, not an incident. See also [host-cpu-high](infra.md#stellarindex_host_cpu_high).
- Before closing: the lake tip must still be advancing; if the journal shows `Skipping catchup: incompatible core version or invalid local state`, go to [_catchup_refused](#stellarindex_galexie_catchup_refused). Verify `rate(node_vmstat_pswpout[10m])` back to 0 and `free -g` swap used flat (used swap does not fall on its own; flat is the signal, not zero).
- Not a trigger: swap-in without swap-out (alert reads `pswpout` only); bursts under 15 min (`for: 15m`).

## Related

- ADR-0016 (R1 = full mirror), ADR-0027 (hot floor + trim, `docs/operations/lcm-cache-tiering.md`), [archive-files-missing](archive-completeness.md#stellarindex_archive_files_missing), [bootstrap-archival-node](bootstrap-archival-node.md), [galexie-archive-mirror](galexie-archive-mirror.md) (the off-site copy).
