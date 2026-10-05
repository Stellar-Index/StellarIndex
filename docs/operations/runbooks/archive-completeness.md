---
title: Runbook — archive-completeness alerts
last_verified: 2026-10-05
status: draft
---
# Archive completeness alerts

Alerts from `deploy/monitoring/rules/archive-completeness.yml` and `configs/prometheus/rules.r1/archive-completeness.yml`. The daily `archive-completeness.timer` / `archive-completeness.service` verifies the cross-anchor archive and fills gaps from a nine-source fallback chain. Policy: [ADR-0017](../../adr/0017-archive-completeness-invariants.md); operational overview: [archive-completeness.md](../archive-completeness.md).

## At a glance

- [`stellarindex_archive_completeness_critical_stale`](#stellarindex_archive_completeness_critical_stale)
- [`stellarindex_archive_completeness_stale`](#stellarindex_archive_completeness_stale)
- [`stellarindex_archive_files_missing`](#stellarindex_archive_files_missing)
- [`stellarindex_archive_repair_source_degraded`](#stellarindex_archive_repair_source_degraded)

## stellarindex_archive_completeness_stale

**Severity** P2 (ticket). MTTR 5 min if the cron silently failed, 1 h if the daemon is broken.
**Trigger** `(time() - archive_completeness_last_success_timestamp) > (26 * 3600)` for 5m (24 h cron + 2 h cushion). Value exactly `0` means no clean verify was ever recorded on this host (fresh deploy or wiped collector dir): a real staleness condition. A run that ends with residuals leaves the timestamp at its previous value (carried forward), so the series stays present and the alert keeps firing; that is intended. To tell "timer not firing" from "runs failing", compare `last_success_timestamp` with the timer and journal output below.
**Impact** Same as [files-missing](#stellarindex_archive_files_missing) given time: `flags.reduced_redundancy` on API responses, status page degrades.

**Diagnose** (5 min)

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

Patterns:
- Timer dropped on reboot (`Persistent=true` should re-fire; hardware fault or systemd misconfig can break it): re-enable.
- Daemon crash mid-run (usually a panic in repair-fetch on malformed upstream data): check the journal tail; the next run resumes.
- Disk full: the daemon writes a JSON gap report to `/var/lib/galexie/` and exits non-zero if it cannot. Free space, re-run.
- Stuck on one file (every fallback source fails; should give up after the chain is exhausted): `kill -9` and re-run.
- oom-killed (`dmesg | grep oom` near the failure): fallback fetcher buffers responses; bump the unit's `MemoryMax=` and re-run.

**Fix** (15 min)

1. If the timer is not scheduled: `ssh r1 'systemctl enable --now archive-completeness.timer'`
2. Run the daemon and watch it complete:
   ```sh
   ssh r1 'systemctl start archive-completeness.service'
   ssh r1 'journalctl -u archive-completeness.service -f'
   ```
3. If that fails, run verify manually and bisect the range. `verify` is a single cross-anchor structural check (no `-checks` modes; `cmd/stellarindex-ops/main.go::archiveCompletenessVerify`).
   ```sh
   # ARCHIVE_FROM / ARCHIVE_TO from the unit's Environment (see step 4 above); -to is required.
   ssh r1 'stellarindex-ops archive-completeness verify \
     -from <ARCHIVE_FROM> -to <ARCHIVE_TO> -workers 8 \
     -output-file /tmp/completeness-report.json'

   # Inspect the missing-file list, then re-run scoped to a narrow range (LO/HI from the report).
   ssh r1 'jq ".missing" /tmp/completeness-report.json | head'
   ssh r1 'stellarindex-ops archive-completeness verify -from LO -to HI -workers 8'
   ```
   For the missing checkpoints, see [archive-completeness.md](../archive-completeness.md) for the bootstrap step that addresses the gap.
4. **Verify** `archive_completeness_last_success_timestamp` updates to now; alert clears within one eval cycle (1 min).

**False positives** R2/R3 alerting while R1 is fine: R2/R3 scrape R1's metrics endpoint for the cross-anchor timestamp, so a broken federation scrape (firewall, DNS) shows stale. Check R1's local timestamp; if fresh, escalate to the metrics-federation runbook.

**RCA** Capture `journalctl -u archive-completeness.service --since="7 days ago"` (one-off or degrading?); `last reboot` plus journal around boot if the timer dropped; for repeated failures, diff gap-report JSONs across runs (stable or growing).

## stellarindex_archive_completeness_critical_stale

**Severity** P1 (`severity: page`), R1 only: R1 is the integrity leader for the fleet (ADR-0016/0017). MTTR as above.
**Trigger** `(time() - archive_completeness_last_success_timestamp) > (48 * 3600)` for 5m. At 48 h without a clean verify, R2/R3 should already be flagging `reduced_redundancy`. Escalate to the SEV-2 playbook; a manual `archive-completeness verify` run is the first diagnostic.

**Diagnose / Fix** Identical to [stellarindex_archive_completeness_stale](#stellarindex_archive_completeness_stale): same metric, same timer, same steps. Start with a manual verify run (step 4 of Diagnose), then the Fix steps; the `0` = never-succeeded reading and the R2/R3 federation false positive apply here too. Postmortem capture as above.

## stellarindex_archive_files_missing

**Severity** P2 (ticket). MTTR 5-15 min (next daily run refills); 1-4 h manual after the fallback chain is exhausted.
**Trigger** `archive_files_missing > 0` for 4h. Only `archive="cross-anchor"` can appear today (`internal/archivecompleteness/report.go`: `Report.Primary` is nil, so no `galexie-archive` series); Galexie's own archive is covered by [galexie-archive-contiguity](galexie-archive-contiguity.md) and [galexie-archive-tip-lag](galexie-archive-tip-lag.md). The daily timer must still be alive (`archive_completeness_last_success_timestamp` within 26 h); if older, go to [stale](#stellarindex_archive_completeness_stale).
**Impact** `flags.reduced_redundancy = true` on API responses while the gap persists; rate data still served correctly from CAGGs. Status page may show Degraded performance if R1 is affected; it stays "Operational" if only R2/R3 are affected.

**Diagnose** (5 min)

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

Most of the chain `200` and few checkpoints missing: a per-checkpoint 404 the chain did not resolve last pass; do step 1. Whole chain unreachable: egress/DNS incident on the host, not an archive incident; triage [host-down](host-down.md) / [all-ingestion-down](all-ingestion-down.md) first, the next daily run refills on its own.

**Fix** (15 min)

1. Re-run the daily unit. Its `ExecStartPre` (`compute-archive-to.sh`) derives `-to` from `ingestion_cursors` and `ARCHIVE_FROM` tracks the ADR-0027 hot floor; idempotent, refetches only what is missing.
   ```sh
   ssh r1 'systemctl start archive-completeness.service'
   ssh r1 'journalctl -u archive-completeness.service -f --since="5 min ago"'
   ```
2. If that did not clear it, run the range by hand with more workers. Modes: `check` / `fix` (check + fallback-fill) / `verify` (check, fix, re-check, emit the Prometheus textfile). No `-force-all-sources` flag; the chain already tries all nine sources per missing file. `-to` is REQUIRED and `-to 0` errors (`-to is required`). `-write` is REQUIRED; without it this is a fail-closed dry run. Take `-from` from the unit, never type `2`: on an ADR-0027-trimmed host the cold range is deliberately empty and `-from 2` reports it as missing.
   ```sh
   ssh r1 'systemctl show -p Environment archive-completeness.service; cat /run/archive-completeness.env'
   # -> ARCHIVE_FROM=<hot floor>  ARCHIVE_TO=<cursor-derived head>

   ssh r1 'stellarindex-ops archive-completeness fix \
     -write -from <ARCHIVE_FROM> -to <ARCHIVE_TO> -workers 16 \
     -output-file /var/lib/galexie/last-completeness-report.json'
   ```
   Exit 0 = no residual missing files; 1 = chain exhausted on some (step 3).
3. If files stay unfilled, log the residual list as a known incident and escalate to the responder for [archive-completeness-stale](#stellarindex_archive_completeness_stale). The files are unrecoverable from any public archive; recovery needs our own validator catching up from peers (multi-week, ADR-0004 territory) or an out-of-band request to a full-archive operator.
4. **Verify** `archive_files_missing` drops to 0 on the run after step 1. The gauge is rewritten only when the textfile is emitted (`verify`); a hand-run `fix` clears the archive but the gauge waits for the next timer tick or a `verify` run. `flags.reduced_redundancy` clears on each region's next health poll (~60 s after the gauge).

**RCA capture** Gap report `/var/lib/galexie/last-completeness-report.json` (`cross_anchor.missing`, `range`); the per-`source` repair attempts/failures counters before mitigation; `journalctl -u archive-completeness.service` for the last 3 runs; `curl -sf -w "%{http_code}" https://history.stellar.org/prd/core-live/core_live_001/.well-known/stellar-history.json` from r1 (and the same for publicnode / lobstr members showing failures).

**False positives** Fresh region bring-up (new R3 starts with an empty cross-anchor archive; suppress per the bring-up runbook). Hand-run with wrong `-from` on a trimmed host (see step 2). Test-net host: cross-anchor fill is pubnet-only and `NewCrossAnchorFiller` REFUSES on `-network testnet|futurenet`; test nets self-heal from their own galexie/core.

Related: [archive-divergence](archive-divergence.md) (content/hash mismatch; this alert is presence gaps only).

## stellarindex_archive_repair_source_degraded

**Severity** P3 (`severity: informational`). No MTTR; customers see no impact because the fallback chain compensates. It becomes the upstream of [files-missing](#stellarindex_archive_files_missing) if multiple sources degrade at once.
**Trigger** per `source`, `increase(archive_completeness_repair_failures_total[25h]) / increase(archive_completeness_repair_attempts_total[25h]) > 0.10` for 30m (25 h = one daily verify cadence plus 1 h). `archive_files_missing` is usually still 0.

**Diagnose** (5 min)

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

Persistent 5xx: their problem; the chain is the answer. Open an issue tracking the outage, close the alert when their status page is green. Reachable but 404 on paths the daemon expects: retroactive gaps in their archive; same fix as files-missing (the next-tier source fills it).

**Fix** No immediate action unless several sources are degraded.
- One source degraded: confirm the chain still fills files (`archive_files_missing` 0 or trending down).
- Multiple degraded: re-run to confirm completeness holds and watch the per-source metric: `ssh r1 'systemctl start archive-completeness.service'`

**RCA** Capture per-source counter snapshots for 24 h; cross-reference the source's public status page (SDF / publicnode / lobstr); if a known maintenance window, add the schedule to `deploy/monitoring/silences.yml`.

**False positives** First ~24 h after adding a new source URL (suppress). One checkpoint 404 on one source: the threshold ignores single-file misses, but a low-volume cycle amplifies (3 repairs, one 404 = 33%); check absolute `increase(archive_completeness_repair_attempts_total[25h])` before opening anything.

## Related

- [backup-offsite](backup-offsite.md): off-site backup alerts.
