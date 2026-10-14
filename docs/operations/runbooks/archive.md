---
title: Runbook — archive
last_verified: 2026-10-06
status: living
severity: P1
---

# Runbook — archive alerts

## At a glance

P2/P3 archive-integrity and galexie-archive alerts; each section below is pinned by an alert rule.

Archive alerts: archive completeness, SDF archive divergence and publish, Galexie archive and its mirror.

## Shared context: Archive completeness alerts

Rules: `deploy/monitoring/rules/archive-completeness.yml`, `configs/prometheus/rules.r1/archive-completeness.yml`. The daily `archive-completeness.timer` verifies the cross-anchor archive and fills gaps from a nine-source fallback chain. Policy: [ADR-0017](../../adr/0017-archive-completeness-invariants.md); overview: [archive-completeness.md](../archive-completeness.md).

## stellarindex_archive_completeness_stale

P2 (ticket). Fires on `(time() - archive_completeness_last_success_timestamp) > (26 * 3600)` for 5m. Value `0` = no clean verify ever recorded on this host. Impact: `flags.reduced_redundancy` on API responses.

**Check**

```sh
# 1. Is the timer scheduled?
ssh r1 'systemctl list-timers archive-completeness.timer'

# 2. Is the service unit healthy? Look at the last few invocations.
ssh r1 'systemctl status archive-completeness.service'
ssh r1 'journalctl -u archive-completeness.service --since="48 hours ago" -p err'

# 3. Did the last run exit non-zero? Why?
ssh r1 'journalctl -u archive-completeness.service --since="48 hours ago" | tail -50'

# 4. Is the binary working? Take -from/-to from the unit's Environment (-to is required):
ssh r1 'systemctl show -p Environment archive-completeness.service; cat /run/archive-completeness.env'
ssh r1 'stellarindex-ops archive-completeness verify -from <ARCHIVE_FROM> -to <ARCHIVE_TO> -workers 8'
```

**Likely causes**

- Timer dropped on reboot: re-enable.
- Daemon crash mid-run: check the journal tail; next run resumes.
- Disk full: the gap report is written to `/var/lib/galexie/`.
- oom-killed (`dmesg | grep oom`): bump the unit's `MemoryMax=`. Stuck on one file: `kill -9` and re-run.

**Fix**

1. Timer not scheduled: `ssh r1 'systemctl enable --now archive-completeness.timer'`
2. Run the daemon and watch it complete:
   ```sh
   ssh r1 'systemctl start archive-completeness.service'
   ssh r1 'journalctl -u archive-completeness.service -f'
   ```
3. If that fails, run verify by hand and bisect the range (`verify` is a single cross-anchor structural check; no `-checks` modes):
   ```sh
   # ARCHIVE_FROM / ARCHIVE_TO from the unit's Environment (see step 4 above); -to is required.
   ssh r1 'stellarindex-ops archive-completeness verify \
     -from <ARCHIVE_FROM> -to <ARCHIVE_TO> -workers 8 \
     -output-file /tmp/completeness-report.json'

   # Inspect the missing-file list, then re-run scoped to a narrow range (LO/HI from the report).
   ssh r1 'jq ".missing" /tmp/completeness-report.json | head'
   ssh r1 'stellarindex-ops archive-completeness verify -from LO -to HI -workers 8'
   ```
   For missing checkpoints see [archive-completeness.md](../archive-completeness.md).
4. Verify `archive_completeness_last_success_timestamp` updates; the alert clears within 1 min.

R2/R3 alerting while R1 is fine: a broken federation scrape; check R1's local timestamp first.

**Escalate** Repeated failures: attach `journalctl -u archive-completeness.service --since="7 days ago"`.

## stellarindex_archive_completeness_critical_stale

P1 (`severity: page`), R1 only. Fires at `> (48 * 3600)` for 5m. Same metric, timer and steps as [stellarindex_archive_completeness_stale](#stellarindex_archive_completeness_stale): start with the manual verify (Check step 4), then Fix. **Escalate** to the SEV-2 playbook.

## stellarindex_archive_files_missing

P2 (ticket). Fires on `archive_files_missing > 0` for 4h. Only `archive="cross-anchor"` exists; Galexie's own archive is covered by [galexie-archive](archive.md#stellarindex_galexie_archive_gap) and [tip-lag](archive.md#stellarindex_galexie_archive_tip_lag_high). If `archive_completeness_last_success_timestamp` is older than 26 h go to [stale](#stellarindex_archive_completeness_stale). Impact: `flags.reduced_redundancy = true` while the gap persists.

**Check**

```sh
# 1. The gauge + the freshness signal that proves the daily run is alive
ssh r1 'curl -s localhost:9100/metrics | grep -E "^archive_files_missing|^archive_completeness_last_success_timestamp"'

# 2. Which fallback sources are degraded (per-source counters, label `source`)
ssh r1 'curl -s localhost:9100/metrics | grep -E "^archive_completeness_repair_(attempts|failures)_total"'

# 3. The current gap report (post-fix state of the last run)
ssh r1 'jq ".range, (.cross_anchor.missing_count), (.cross_anchor.missing[0:5])" /var/lib/galexie/last-completeness-report.json'

# 4. Is the fallback chain reachable from r1? These nine ARE the chain
#    (internal/archivecompleteness/cross_anchor_fill.go::DefaultCrossAnchorSources); no AWS source.
ssh r1 '
for u in https://history.stellar.org/prd/core-live/core_live_001 \
         https://history.stellar.org/prd/core-live/core_live_002 \
         https://history.stellar.org/prd/core-live/core_live_003 \
         https://bootes-history.publicnode.org \
         https://lyra-history.publicnode.org \
         https://hercules-history.publicnode.org \
         https://archive.v1.stellar.lobstr.co \
         https://archive.v2.stellar.lobstr.co \
         https://archive.v5.stellar.lobstr.co
do
  code=$(curl -s -o /dev/null -m 10 -w "%{http_code}" "$u/.well-known/stellar-history.json")
  echo "$code $u"
done'
```

**Likely causes**

- Most of the chain `200`: per-checkpoint 404 the last pass did not resolve; do Fix 1.
- Whole chain unreachable: egress/DNS on the host; triage [host-down](infra.md#stellarindex_host_down) / [all-ingestion-down](ingestion.md#stellarindex_ingestion_all_sources_stopped) first.
- Fresh R3 bring-up (empty cross-anchor archive), or hand-run with the wrong `-from` on a trimmed host.
- Test-net host: cross-anchor fill is pubnet-only and refuses on `-network testnet|futurenet`.

**Fix**

1. Re-run the daily unit (idempotent; its `ExecStartPre` `compute-archive-to.sh` derives `-to`):
   ```sh
   ssh r1 'systemctl start archive-completeness.service'
   ssh r1 'journalctl -u archive-completeness.service -f --since="5 min ago"'
   ```
2. Else run the range by hand. Modes: `check` / `fix` / `verify` (check, fix, re-check, emit textfile). `-to` and `-write` are REQUIRED (without `-write` it is a dry run). Take `-from` from the unit, never `2`: on a trimmed host the cold range is deliberately empty.
   ```sh
   ssh r1 'systemctl show -p Environment archive-completeness.service; cat /run/archive-completeness.env'
   # -> ARCHIVE_FROM=<hot floor>  ARCHIVE_TO=<cursor-derived head>

   ssh r1 'stellarindex-ops archive-completeness fix \
     -write -from <ARCHIVE_FROM> -to <ARCHIVE_TO> -workers 16 \
     -output-file /var/lib/galexie/last-completeness-report.json'
   ```
   Exit 0 = no residual; 1 = chain exhausted on some.
3. Residual files are unrecoverable from any public archive: log a known incident and escalate.
4. Verify `archive_files_missing` drops to 0 (the gauge is rewritten only by `verify`/the timer tick; `flags.reduced_redundancy` clears ~60 s later).

**Escalate** Attach `/var/lib/galexie/last-completeness-report.json`, the per-`source` repair counters and the last 3 runs' journal. Related: [archive-divergence](archive.md#stellarindex_stellar_archive_divergence) (hash mismatch; this alert is presence only).

## stellarindex_archive_repair_source_degraded

P3 (`severity: informational`). Fires per `source` when `increase(archive_completeness_repair_failures_total[25h]) / increase(archive_completeness_repair_attempts_total[25h]) > 0.10` for 30m. No customer impact while the chain compensates.

**Check**

```sh
# 1. Which source is degraded?
ssh r1 'curl -s localhost:9100/metrics | grep archive_completeness_repair_failures_total'

# 2. Test the source directly. Source labels (DefaultCrossAnchorSources): sdf-core-live-001/002/003,
#    publicnode-bootes/lyra/hercules, lobstr-v1/v2/v5.
# SDF core_live_001:
curl -sf -m 10 -I https://history.stellar.org/prd/core-live/core_live_001/.well-known/stellar-history.json

# Per-tier-1 validator (substitute URL):
curl -sf -m 10 -I https://bootes-history.publicnode.org/.well-known/stellar-history.json
```

**Likely causes**

- Persistent 5xx: their outage; the chain compensates. Track it.
- 404 on expected paths: gaps in their archive; the next source fills it.
- Low volume (3 repairs, one 404 = 33%): check `increase(archive_completeness_repair_attempts_total[25h])` first.

**Fix** Nothing unless several sources degrade. One source: confirm `archive_files_missing` is 0 or falling. Several: `ssh r1 'systemctl start archive-completeness.service'` and watch the per-source metric. Known maintenance windows go in `deploy/monitoring/silences.yml`.

**Escalate** If `archive_files_missing` starts rising, treat as [files-missing](#stellarindex_archive_files_missing).

## stellarindex_stellar_archive_divergence

P1 (`severity: page`, SEV-1). Fires on `increase(stellarindex_verify_archive_mismatches_total[26h]) > 0`, `for: 0s`, labelled `tier` and `reason` in {chain, sequence, checkpoint}. Rule: `configs/prometheus/rules.r1/stellar.yml`. Chain/sequence = hash-chain break or ledger gap in our archive; checkpoint = divergence vs a reference (SDF / LOBSTR / Satoshipay). Also trips [`verify_archive_unit_failed`](verify-archive.md#stellarindex_verify_archive_unit_failed) (ticket).

Tier A (`verify-archive-tier-a.timer`, chain walk) and Tier B (`verify-archive-tier-b.timer`, checkpoint cross-check) write textfiles `/var/lib/node_exporter/textfile_collector/verify_archive_tier_a.prom` (`tier="chain"`) and `verify_archive_tier_b.prom` (`tier="checkpoint"`) on every exit; totals are cumulative. Emitter: `internal/ops/archive/verify_archive_textfile.go`. If a `.prom` file is missing or older than the last timer trigger the page is blind: `journalctl -u verify-archive-tier-a | grep textfile`.

Blind spots:

1. Tier D (weekly peer sampling, root cron, own lock `verify-archive-tier-d`) and Tier E (`rs-stellar-archivist` scan) emit no metric; a divergence there shows only in `journalctl -t stellarindex-tier-d`.
2. The first run on a host with no `.prom` publishes at its final value, so `increase()` reads 0 until the next run; prime both units at apply time.

**Apply (units take effect on r1 only after the ansible role is applied)**

```sh
# 1. Render BOTH units (tier-b lives in its own `verify_archive_tier_b_enabled` block).
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
  --tags ops-jobs --check --diff
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
  --tags ops-jobs

# 2. Prime BOTH producers once, one at a time (they share run-heavy-job.sh's "verify-archive"
#    lock; the second start queues, up to 20h). A non-zero exit is itself a finding.
systemctl start verify-archive-tier-a.service
systemctl start verify-archive-tier-b.service

# 3. FAIL-CLOSED CONFIRM: both tiers must be exporting.
for t in chain checkpoint; do
  curl -sf localhost:9100/metrics \
    | grep -q "stellarindex_verify_archive_mismatches_total{tier=\"$t\"" \
    || { echo "MISSING tier=$t — apply is HALF-DONE; archive-divergence cannot fire for that tier"; exit 1; }
done; echo "both tiers exporting"
```

Healthy: all six samples (2 tiers x 3 reasons) read 0.

**Check**

```sh
ssh root@136.243.90.96

# Which checkpoint, which bucket, what's the mismatch?
journalctl -u verify-archive-tier-a -n 200 --no-pager
journalctl -u verify-archive-tier-b -n 200 --no-pager
journalctl -t stellarindex-tier-d -n 200 --no-pager   # weekly peer sweep

# Pull the hash from our archive. /srv/history-archive is a plain
# ZFS path (dataset data/archive) — NOT a MinIO bucket; mc has no
# business here. Checkpoint hex 0xAABBCCDD →
# history/AA/BB/CC/history-aabbccdd.json:
cat /srv/history-archive/history/<AA>/<BB>/<CC>/history-<ckpt-hex>.json | jq .currentBuckets

# Pull the same checkpoint from a reference
curl -s https://history.stellar.org/prd/core-live/core_live_001/history/<AA>/<BB>/<CC>/history-<ckpt-hex>.json | jq .currentBuckets

# Diff the bucket hashes — at least one row differs: that is the corrupted / diverged bucket.
# (For reason="chain"/"sequence" the journal names the exact ledger range; no reference needed.)
```

**Likely causes**

- Bit rot: `zpool status -v data` for scrub errors.
- Overwritten archive storage: check access logs and mtimes under `/srv/history-archive`.
- Scanner bug or scan racing a repair write: cross-check one reference archive against another.

**Fix**

1. Do not overwrite the diverged bucket; keep a forensic copy.
2. Determine extent: one bucket or many; for chain/sequence, how wide the broken ledger range is.
3. Storage corruption: restore the range from an independent copy (AWS public blockchain bucket or an SDF / LOBSTR / Satoshipay archive; we run one local mirror).
4. Compromise: SECURITY incident (rotate archive credentials, inspect access logs; see SECURITY.md).
5. Verify a full verify-archive pass over the range reports zero mismatches.

**Escalate** SEV-1; page the responder.

## stellarindex_stellar_archive_publish_fail

P3 (`severity: informational`). Inert everywhere: `stellarindex_stellar_archive_publish_errors_total` has no producer (listed in `KNOWN_INERT` in `scripts/ci/lint-metric-refs.sh`), and nothing of ours publishes to `/srv/history-archive` today. Retained for ADR-0004 Phase 3. Rule: `configs/prometheus/rules.r1/stellar.yml`, `for: 1h`. Trigger: `increase(stellarindex_stellar_archive_publish_errors_total[1h]) > 0`.

**Check** (the archive is the `/srv/history-archive` filesystem, not a MinIO bucket)

```sh
# stellar-core publisher logs
ssh root@<val-host> "journalctl -u stellar-core -n 500 --no-pager" \
  | grep -iE 'history|publish|upload'

# Newest published checkpoint and archive state file
ssh root@<val-host> "cat /srv/history-archive/.well-known/stellar-history.json | head -c 400"

# Space and ownership on the archive volume
ssh root@<val-host> "df -h /srv/history-archive; ls -ld /srv/history-archive"
```

**Likely causes** Archive backend outage or auth failure; network egress broken; wrong `put` / `get` / `mkdir` in the `HISTORY` section of `stellar-core.cfg`; staging dir (`/tmp`) full.

**Fix**

1. Read core's log for the upload error and fix the backend cause (auth / space / network).
2. Core retries on the next checkpoint (~5 min).
3. Gaps in the archive: `stellar-core publish` for the specific checkpoints; context in [archival-node-bringup](../archival-node-bringup.md).
4. Verify `increase(... errors_total[1h]) == 0`. Silence during a deliberate archive cutover.

**Escalate** Disk-full variant: `postgres.md#stellarindex_timescale_disk_full`. See also [divergence](#stellarindex_stellar_archive_divergence).

## Shared context: Galexie archive alerts

Alerts for the R1 durable ledger mirror (`galexie-archive` bucket on MinIO, ADR-0016), the galexie captive core, and host swap pressure. Rules: `configs/prometheus/rules.r1/galexie-archive.yml` (what r1 loads) and `deploy/monitoring/rules/galexie-archive.yml` (identical). `severity: page` = P1, `severity: ticket` = P3 (routing: `configs/alertmanager/alertmanager.r1.yml`).

- No customer impact; `aws-public-blockchain` backstops, and the archive is re-pulled from there, not backed up off-site.
- Partitions are 64,000 ledgers; the fill mirrors only COMPLETE ones, so tip lag is a 0 -> 64,000 sawtooth. Declared shape: `[0, 63999]` plus `[ARCHIVE_FROM = 49,984,000 -> tip]`. The fill is pubnet only.
- Every mc-based guard reads through root's `local` mc alias; a rotated MinIO credential breaks all of them. The `aws-public` alias lives only in root's `~/.mc/config.json`.
- Probe-blindness alerts (`_metric_stale`, `_contiguity_silent`, `_scan_degraded`, `_upstream_check_stale`, `_catchup_probe_degraded`): node_exporter serves the LAST textfile forever, so fix the producer first.
- Read a oneshot's `Result` WITH its timestamp (empty `InactiveEnterTimestamp` = never ran):

  ```sh
  ssh r1 'systemctl show galexie-archive-fill.service -p Result,InactiveEnterTimestamp,ExecMainStartTimestamp'
  ```

## stellarindex_galexie_archive_tip_lag_high

`galexie_archive_tip_lag_ledgers > 64000`, `for: 90m`, ticket. A completed partition stayed un-mirrored across two fill cycles.

Check: `systemctl status galexie-archive-fill.service`; last run in `/var/log/galexie-mirror.log` (`needs work (total): 0` = healthy; also `missing entirely:`, `incomplete (queued by Phase 1b):`, `below hot floor (...)`); `mc admin info local`. Fix: Remediation below.

## stellarindex_galexie_archive_tip_lag_severe

`galexie_archive_tip_lag_ledgers > 128000`, `for: 30m`, page. At least 2 partitions (~8 days) behind; the fill timer is broken. Backfills past the gap fail above the hot floor (below it the ADR-0027 cold tier serves reads).

Fix: `sudo systemctl start galexie-archive-fill.service`, read `journalctl -u galexie-archive-fill.service -n 100` and `/var/log/galexie-mirror.log`, then root-cause the timer/service.

Remediation (both lag alerts):

```sh
# Manual catch-up (idempotent; same unit the timer runs, via run-heavy-job.sh:
# singleton flock, MemoryMax=20G, disk watchdog, 6h TimeoutStartSec):
ssh r1 'sudo systemctl start galexie-archive-fill.service'
# Foreground run with the same guards (never run the script bare: it runs without
# the MemoryMax and watchdog caps, and exits 75 if the timer's run holds the lock):
ssh r1 'sudo /usr/local/sbin/run-heavy-job.sh galexie-archive-fill /usr/local/bin/galexie-archive-fill'
# Force the tip-lag updater to re-read:
ssh r1 'sudo systemctl start galexie-archive-tip-lag.service'
# Re-enable timers if disabled:
ssh r1 'sudo systemctl enable --now galexie-archive-fill.timer galexie-archive-tip-lag.timer'
```

Fill failures: bucket permission denied (rotate mc alias creds), aws-public 503s (retry in an hour), MinIO out of space (`zpool list -o name,size,alloc,free,cap data`). Partitions below the hot floor are intentionally not mirrored. Floor: the larger of `ARCHIVE_HOT_FLOOR` in `/etc/default/galexie-archive-fill` and `/var/lib/galexie-archive/hot-floor`.

## stellarindex_galexie_archive_tip_lag_metric_stale

`galexie_archive_tip_lag_probe_success == 0 or time() - galexie_archive_tip_lag_updated_seconds > 1800`, `for: 15m`, ticket. The last run could not read a bucket tip (the script omits the lag rather than publish 0, so both lag alerts are blind) or the 5-min timer stopped. Restore this metric first.

```sh
ssh r1 'journalctl -u galexie-archive-tip-lag.service -n 50'   # mc-alias misconfig, permissions, MinIO down
ssh r1 'ls -la /var/lib/node_exporter/textfile_collector/galexie_archive_tip_lag.prom'
ssh r1 'cat /var/lib/node_exporter/textfile_collector/galexie_archive_tip_lag.prom'
ssh r1 'systemctl list-timers galexie-archive-fill.timer galexie-archive-tip-lag.timer'
ssh r1 'sudo /usr/local/bin/galexie-archive-tip-lag --self-test'   # pins the object-name parser; no mc alias needed
# Manual gap check (partition names are reverse-hex prefixed, first row = newest):
ssh r1 'mc ls local/galexie-live | head -1; mc ls local/galexie-archive | head -1'
```

## stellarindex_galexie_archive_gap

`galexie_archive_unexpected_gaps > 0`, `for: 1h`, page. Top-level partition coverage no longer matches the declared shape (bad trim cutoff, manual `mc rm`, heal failure, or an overlap from a botched re-fill). Producer: `galexie-archive-contiguity.timer` -> `/usr/local/bin/galexie-archive-contiguity` -> `/var/lib/node_exporter/textfile_collector/galexie_archive_contiguity.prom`. Healthy: gauge 0, `scan_ok == 1`. Chunk-level holes inside a partition: [archive-files-missing](archive.md#stellarindex_archive_files_missing).

Fix:

1. Freeze the trim: hold `compute-trim-cutoff` / any trim job (a mis-computed cutoff is the likeliest cause).
2. Walk the shape for holes/overlaps against the declared trim (`EXPECTED_TRIM` in the service env, default `64000-49983999`):

   ```sh
   mc ls local/galexie-archive/ \
     | grep -oE -- '--[0-9]+-[0-9]+' | sed 's/^--//' | sort -t- -k1,1n
   ```

3. Overlap: compare object counts/sizes; remove the incomplete (usually newer) one only after confirming the other is whole.
4. Hole inside declared coverage: re-pull from `aws-public-blockchain` with `galexie-archive-fill` (incomplete partitions via the `PARTIALS` env), re-run the scan service, confirm the gauge returns to 0. A hole below the hot floor is skipped by the fill.
5. Shape legitimately changed: update `EXPECTED_TRIM` via ansible (empty = strict contiguity) and the HA plan doc in the same change.

## stellarindex_galexie_archive_contiguity_silent

`absent_over_time(galexie_archive_unexpected_gaps[3h])`, `for: 15m`, ticket. The hourly scan stopped emitting; the gap guard is blind.

Check: `systemctl status galexie-archive-contiguity.timer`, the service journal, `mc ls local/galexie-archive/ | head` as root.

## stellarindex_galexie_archive_scan_degraded

```
galexie_archive_scan_ok == 0
or
(time() - galexie_archive_scan_last_run_unix) > 10800
or
absent_over_time(galexie_archive_scan_last_run_unix[6h])
```

`for: 15m`, ticket. `stellarindex_galexie_archive_gap` is blind.

`scan_ok` 0: bucket listing errored (`local` mc alias, MinIO, credentials). `scan_last_run_unix` old or absent: scan or timer not running. `galexie_archive_scan_listing_lines` (diagnostic): 0 = bucket really empty, > 0 with zero partitions = listing shape the parser no longer matches.

Check: `systemctl status galexie-archive-contiguity.timer` and the file's mtime, then run `/usr/local/bin/galexie-archive-contiguity` by hand and read `/var/lib/node_exporter/textfile_collector/galexie_archive_contiguity.prom`.

## stellarindex_galexie_archive_upstream_divergence

`sum(galexie_archive_upstream_objects{result=~"upstream-rewritten|local-differs|local-only"}) > 0`, `for: 15m`, ticket. The weekly `galexie-mirror-verify.timer` (Sunday 03:41 UTC, under the heavy-job lock; installed only where `storage.s3_cold_bucket_archive` is set) found mirrored objects that differ from upstream or that upstream no longer lists.

```bash
systemctl status galexie-mirror-verify.timer galexie-mirror-verify.service
journalctl -u galexie-mirror-verify.service --no-pager | grep -E 'MISMATCH|galexie-mirror-verify:'
cat /var/lib/node_exporter/textfile_collector/galexie_archive_upstream.prom
```

Each line: `MISMATCH partition=<p> ledger=<n> cause=<c> ... key=…`. Pairs that decode equal are `equivalent` and print nothing. Causes:

- `upstream-rewritten`: upstream re-exported after we mirrored; ours is stale.
- `local-differs`: our copy is newer and still differs; something rewrote our object. Treat ours as suspect.
- `local-only`: upstream no longer lists a key we hold; usually an upstream deletion, confirm first.

`equivalent`, `missing-local` (`MISSING ...` lines) and `unverifiable` never fire the alert.

Fix: re-run one range by hand (read-only):

```bash
sudo -u stellarindex /usr/local/bin/stellarindex-ops galexie-mirror-verify \
  -config /etc/stellarindex.toml -from 58973375 -to 58973375
```

To replace a stale or suspect object with upstream's, keep the old bytes, then rehydrate (it skips existing keys, so the old object must go):

```bash
mc cp local/galexie-archive/<key> /var/tmp/<ledger>.xdr.zst.local
mc rm local/galexie-archive/<key>
sudo -u stellarindex /usr/local/bin/stellarindex-ops rehydrate-galexie-archive \
  -config /etc/stellarindex.toml -from <ledger> -to <ledger> -write
```

Re-run the range check; it must report `matched`. If decoded events or operations differ, rebuild the lake rows and replay affected projected domains per [the replay decision rule](../../architecture/ingest-pipeline.md#the-replay-decision-rule); an identical decode needs no replay.

## stellarindex_galexie_archive_upstream_check_stale

```
(time() - galexie_archive_upstream_last_success_unix) > 1296000
or
absent_over_time(galexie_archive_upstream_last_success_unix[1h]) == 1
```

`for: 1h`, ticket (15 days = two missed weekly runs). Runs keep failing (the journal names the listing error), the timer is not firing, or none ever completed (fresh deploy; a full walk takes hours). Start it with `systemctl start galexie-mirror-verify.service`. Check: `systemctl status galexie-mirror-verify.timer` and the service journal.

## stellarindex_galexie_catchup_refused

`stellarindex_galexie_catchup_refusals_5m > 0`, `for: 10m`, page. The captive core inside galexie logs `History: Skipping catchup: incompatible core version or invalid local state`; the lake tip is FROZEN (served-data staleness within ~10 min). Usual cause: host swap pressure corrupting the core's local state.

Check:

1. Rule out version mismatch (rare): `stellar-core version` vs the network protocol. A mismatch is an upgrade task, not a restart.
2. Gap: lake tip (`SELECT max(ledger_seq) FROM stellar.ledgers` on :8123) vs `seq=` in recent galexie journal lines.
3. Why the state went bad: swap ([stellarindex_host_swap_activity](#stellarindex_host_swap_activity), `free -g`), OOM kills, disk errors. Heavy one-shots MUST run under `/usr/local/sbin/run-heavy-job.sh`.

Fix: `systemctl restart galexie`. The core discards wedged state, catches up from the history archive (~5-10 min), replays the gap; no data is lost. If that fails see [archival-node-bringup](../archival-node-bringup.md).

### Applying galexie config with ansible (restart ack)

A galexie restart costs a ~9 min cold catch-up. `ansible-playbook --check --diff` prints `RUNNING HANDLER [archival-node : Restart galexie]` when an apply would restart. With galexie active, a real apply FAILS before writing (`... re-run in a maintenance window with -e galexie_restart_ack=true`). Re-run in a window, watch the tip ~10 min, do not re-restart mid-catchup:

```bash
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
  --tags galexie -e galexie_restart_ack=true
```

Never set `galexie_restart_ack` in inventory.

## stellarindex_galexie_catchup_probe_degraded

```
stellarindex_galexie_catchup_probe_read_ok == 0
or
stellarindex_galexie_catchup_probe_journal_lines == 0
or
(time() - stellarindex_galexie_catchup_probe_last_run_unix) > 600
or
absent_over_time(stellarindex_galexie_catchup_probe_last_run_unix[30m])
```

`for: 15m`, ticket. The probe behind `stellarindex_galexie_catchup_refused` (`/var/lib/node_exporter/textfile_collector/galexie_catchup.prom`) is not producing usable metrics.

`..._probe_read_ok` 0: journal read errored. `..._probe_journal_lines` 0: unit name no longer matches or galexie is down (healthy ~400 lines per 5 min). `..._probe_last_run_unix` old or absent: probe or timer not running.

Check: `systemctl status galexie-catchup-probe.timer` and the file's mtime, then run `/usr/local/sbin/galexie-catchup-probe.sh` by hand. A frozen lake still pages via `stellarindex_ingestion_ledger_stalled` in ~10 min; remedy is the `_catchup_refused` Fix.

## stellarindex_host_swap_activity

`rate(node_vmstat_pswpout[10m]) > 100`, `for: 15m`, ticket (`component: infra`). Anonymous memory is being swapped out. Heavy jobs run under `/usr/local/sbin/run-heavy-job.sh` (`MemoryMax=20G`, `MemorySwapMax=0`) and galexie has `MemoryLow=16G`, so a swapper is OUTSIDE those fences. Early warning for [_catchup_refused](#stellarindex_galexie_catchup_refused).

```sh
ssh root@136.243.90.96 'vmstat 1 5; free -g'
# processes holding swap, largest last
ssh root@136.243.90.96 'for p in /proc/[0-9]*; do s=$(awk "/VmSwap/{print \$2}" $p/status 2>/dev/null); [ -n "$s" ] && [ "$s" -gt 0 ] && echo "$s KB $(cat $p/comm)"; done | sort -n | tail -20'
# a scoped job cannot swap, so a swapper is outside the fence
ssh root@136.243.90.96 'systemctl status "heavy-*.scope" --no-pager; systemd-cgtop --order=memory --iterations=2 -n 20'
```

Fix by cause:

1. Heavy ops job run raw: stop it, re-run under `run-heavy-job.sh`.
2. Service leaking heap: pprof heap dump, restart the unit, file an incident.
3. Postgres `work_mem` x backends: see [host-memory-high](infra.md#stellarindex_host_memory_high); also [host-cpu-high](infra.md#stellarindex_host_cpu_high).
4. Genuine undersize: a capacity decision.

Before closing: the lake tip must be advancing (else [_catchup_refused](#stellarindex_galexie_catchup_refused)); `rate(node_vmstat_pswpout[10m])` back to 0 and swap used flat (used swap does not fall on its own).

## stellarindex_galexie_archive_mirror_stale

P3 (ticket). Retired: with `galexie_archive_mirror_enabled: false` (default) ansible removes the mirror units, so this cannot fire; the rest applies only to a host that re-enables it. Rules: `deploy/monitoring/rules/storage.yml`, `configs/prometheus/rules.r1/storage.yml`. No verified mirror run in 48 h; `stellarindex_galexie_archive_mirror_configured{instance="<host>:9100"}` is `0` (no target) or `1`. Producer: `scripts/ops/galexie-archive-mirror.sh` via `galexie-archive-mirror.timer`.

```bash
journalctl -u galexie-archive-mirror.service -n 100 --no-pager
cat /etc/default/galexie-archive-mirror        # DEST_ENDPOINT set?
mc alias list | grep r2-archive                # alias configured?
mc mirror --dry-run local/galexie-archive r2-archive/stellarindex-galexie-archive
```

Fix:

1. `configured 0`: set `galexie_archive_mirror_s3_endpoint` (inventory) and `galexie_archive_mirror_s3_key` / `galexie_archive_mirror_s3_key_secret` (vault), apply with `--tags backup`, then `systemctl start galexie-archive-mirror.service`.
2. Network/credential failure or dry-run verification failure: fix vars, bucket policy and `mc alias list`, re-apply; `mc mirror` resumes.
3. Dry-run still lists objects: the sync did not converge; check target quota.

Restore: `mc mirror` from `r2-archive/<bucket>` back to a rebuilt `local/galexie-archive`, then run the `galexie-archive-contiguity.sh` probe before pointing ingest at it.

## Related

[backup-offsite](backup-offsite.md), [ch-lake-backup](ch-lake-backup.md), [Off-site backup plan](../off-site-backup-plan.md), [verify-archive](verify-archive.md#stellarindex_verify_archive_run_stale), [bootstrap-archival-node](bootstrap-archival-node.md), ADR-0004, ADR-0016, ADR-0027.
