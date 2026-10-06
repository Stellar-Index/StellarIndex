---
title: Runbook — archive
last_verified: 2026-10-06
status: living
severity: P1
---

# Runbook — archive alerts

Archive alerts: archive completeness, SDF archive divergence and publish, Galexie archive and its mirror. Merged from five former pages; each section keeps one page's content.

## At a glance

- [`stellarindex_archive_completeness_stale`](#stellarindex_archive_completeness_stale)
- [`stellarindex_archive_completeness_critical_stale`](#stellarindex_archive_completeness_critical_stale)
- [`stellarindex_archive_files_missing`](#stellarindex_archive_files_missing)
- [`stellarindex_archive_repair_source_degraded`](#stellarindex_archive_repair_source_degraded)
- [`stellarindex_stellar_archive_divergence`](#stellarindex_stellar_archive_divergence)
- [`stellarindex_stellar_archive_publish_fail`](#stellarindex_stellar_archive_publish_fail)
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
- [`stellarindex_galexie_archive_mirror_stale`](#stellarindex_galexie_archive_mirror_stale)

## Shared context: `archive.md` alerts

_Source page `archive.md`: status draft, last verified 2026-10-05._

Alerts from `deploy/monitoring/rules/archive-completeness.yml` and `configs/prometheus/rules.r1/archive-completeness.yml`. The daily `archive-completeness.timer` / `archive-completeness.service` verifies the cross-anchor archive and fills gaps from a nine-source fallback chain. Policy: [ADR-0017](../../adr/0017-archive-completeness-invariants.md); operational overview: [archive.md](../archive-completeness.md).

### At a glance

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
   For the missing checkpoints, see [archive.md](../archive-completeness.md) for the bootstrap step that addresses the gap.
4. **Verify** `archive_completeness_last_success_timestamp` updates to now; alert clears within one eval cycle (1 min).

**False positives** R2/R3 alerting while R1 is fine: R2/R3 scrape R1's metrics endpoint for the cross-anchor timestamp, so a broken federation scrape (firewall, DNS) shows stale. Check R1's local timestamp; if fresh, escalate to the metrics-federation runbook.

**RCA** Capture `journalctl -u archive-completeness.service --since="7 days ago"` (one-off or degrading?); `last reboot` plus journal around boot if the timer dropped; for repeated failures, diff gap-report JSONs across runs (stable or growing).

## stellarindex_archive_completeness_critical_stale

**Severity** P1 (`severity: page`), R1 only: R1 is the integrity leader for the fleet (ADR-0016/0017). MTTR as above.
**Trigger** `(time() - archive_completeness_last_success_timestamp) > (48 * 3600)` for 5m. At 48 h without a clean verify, R2/R3 should already be flagging `reduced_redundancy`. Escalate to the SEV-2 playbook; a manual `archive-completeness verify` run is the first diagnostic.

**Diagnose / Fix** Identical to [stellarindex_archive_completeness_stale](#stellarindex_archive_completeness_stale): same metric, same timer, same steps. Start with a manual verify run (step 4 of Diagnose), then the Fix steps; the `0` = never-succeeded reading and the R2/R3 federation false positive apply here too. Postmortem capture as above.

## stellarindex_archive_files_missing

**Severity** P2 (ticket). MTTR 5-15 min (next daily run refills); 1-4 h manual after the fallback chain is exhausted.
**Trigger** `archive_files_missing > 0` for 4h. Only `archive="cross-anchor"` can appear today (`internal/archivecompleteness/report.go`: `Report.Primary` is nil, so no `galexie-archive` series); Galexie's own archive is covered by [galexie-archive](archive.md#stellarindex_galexie_archive_gap) and [tip-lag](archive.md#stellarindex_galexie_archive_tip_lag_high). The daily timer must still be alive (`archive_completeness_last_success_timestamp` within 26 h); if older, go to [stale](#stellarindex_archive_completeness_stale).
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

Most of the chain `200` and few checkpoints missing: a per-checkpoint 404 the chain did not resolve last pass; do step 1. Whole chain unreachable: egress/DNS incident on the host, not an archive incident; triage [host-down](infra.md#stellarindex_host_down) / [all-ingestion-down](all-ingestion-down.md) first, the next daily run refills on its own.

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

Related: [archive-divergence](archive.md#stellarindex_stellar_archive_divergence) (content/hash mismatch; this alert is presence gaps only).

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

## stellarindex_stellar_archive_divergence

_Source page `archive.md#stellarindex_stellar_archive_divergence`: status current, severity P1, last verified 2026-08-29._

> **LIVE (wired 2026-08-29, issue #282).** Two separate defects
> kept this page from ever firing; both are fixed:
>
> - **No producer.** The counter
>   `stellarindex_verify_archive_mismatches_total` only left the
>   verify-archive process through the opt-in `-metrics-listen`
>   HTTP endpoint, which the `verify-archive-tier-a` / `-tier-b`
>   units never passed and `configs/prometheus/prometheus.r1.yml`
>   has no scrape job for (and a one-shot job is gone between
>   scrapes anyway). Both units now pass `-textfile-output`, so the
>   counter reaches Prometheus through node_exporter's textfile
>   collector — cumulative across runs, zero-seeded on all three
>   reasons, labelled `tier` (`chain` for tier-a,
>   `checkpoint` for tier-b). Emitter:
>   `internal/ops/archive/verify_archive_textfile.go`.
> - **A 1h lookback on a nightly producer.** `increase(…[1h])`
>   made the step visible for one hour in every twenty-four, so the
>   page self-resolved before an operator saw it. The window is now
>   **26h** (24h timer cadence + jitter + cushion).
>
> Pinned by `deploy/monitoring/rule-tests/stellar_test.yml`,
> `internal/ops/archive/verify_archive_unit_wiring_test.go` (the units
> wire an export path, and the tier-b ansible block is reachable under
> the `ops-jobs` tag the apply below uses) and
> `internal/ops/archive/verify_archive_chunks_test.go` (a boundary
> divergence reaches the counter).
>
> **Takes effect on r1 only after the ansible role is applied** —
> the templates under `configs/ansible/…/templates/systemd/` are
> the authority for the running units, so a binary-only deploy
> ships this dead. Until then a mismatch still surfaces only as
> `stellarindex_verify_archive_unit_failed` (severity ticket).
> The unit render is behind the low-risk `ops-jobs` tag (it does
> not touch galexie):
>
> ```sh
> # 1. Render BOTH units. tier-b lives in its own
> #    `verify_archive_tier_b_enabled` block; it carries the
> #    ops-jobs tag too, pinned by
> #    TestVerifyArchiveUnits_ReachableUnderOpsJobsTag. Sanity-check
> #    the selection first if you want:
> #      ansible-playbook --list-tasks -i inventory/r1.yml \
> #        playbooks/archival-node.yml --tags ops-jobs
> ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
>   --tags ops-jobs --check --diff
> ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
>   --tags ops-jobs
>
> # 2. Prime BOTH producers once, one at a time — they share
> #    run-heavy-job.sh's "verify-archive" singleton lock, so the
> #    second start queues until the first run ends (HEAVY_JOB_LOCK_WAIT,
> #    up to 20h; `systemctl start` blocks that long) (Tier D's cron
> #    takes its own "verify-archive-tier-d" lock instead). This is also what
> #    puts the zero baseline on disk BEFORE the first nightly run
> #    (blind spot 2 below), so do not skip it. A non-zero exit here
> #    is itself a finding: read the journal.
> systemctl start verify-archive-tier-a.service
> systemctl start verify-archive-tier-b.service
>
> # 3. FAIL-CLOSED CONFIRM — run it, do not eyeball it. Both tiers
> #    must be exporting; a missing tier means the apply is
> #    HALF-DONE and this page is still dead for that tier.
> for t in chain checkpoint; do
>   curl -sf localhost:9100/metrics \
>     | grep -q "stellarindex_verify_archive_mismatches_total{tier=\"$t\"" \
>     || { echo "MISSING tier=$t — apply is HALF-DONE; archive-divergence cannot fire for that tier"; exit 1; }
> done; echo "both tiers exporting"
> ```
>
> On a healthy host all six samples (2 tiers × 3 reasons) read 0 —
> the zero-seed IS the proof the producer is alive. If step 3 prints
> `MISSING tier=checkpoint`, start the tier-b unit by hand
> (`systemctl start verify-archive-tier-b.service`) and re-run it;
> do not treat a green `tier="chain"` as an applied fix.
>
> Blind spots this page still has are listed under
> [Known blind spots](#known-blind-spots) — read them before you
> trust a silent dashboard.

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_stellar_archive_divergence` |
| Severity | P1 (`severity: page` — SEV-1) |
| Detected by | `configs/prometheus/rules.r1/stellar.yml` (group `stellarindex.stellar`, `severity: page`, `for: 0s`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/stellar.yml`. |
| Typical MTTR | hours |
| Impact | The verify-archive walk found a mismatch. Since the F-1329 repoint this fires equally on **lake-integrity failures** — a hash-chain break or ledger-sequence gap in our own archive — and on cross-archive checkpoint divergence vs a reference (SDF / LOBSTR / Satoshipay). Either way it is a **correctness incident**: our archive's bytes are not the network's bytes, by corruption, bug, or compromise. |

### Symptoms

- `increase(stellarindex_verify_archive_mismatches_total[26h]) > 0`.
  Fires immediately (`for: 0s`) — there's no such thing as a
  "transient" divergence. The textfile-exported series is labelled
  by `tier` (which unit found it) and `reason ∈ {chain | sequence |
  checkpoint}` — the reason label tells you which failure class you
  have (chain break / sequence gap = our lake's internal integrity;
  checkpoint = cross-archive comparison). The in-process
  `-metrics-listen` view of the same counter additionally carries
  `chunk_idx`; the textfile aggregates that away because a chunk
  index is a per-run worker slot with no cross-run meaning.
- The same event ALSO trips
  [`verify-archive.md#stellarindex_verify_archive_unit_failed`](verify-archive.md#stellarindex_verify_archive_unit_failed)
  (severity ticket) — a mismatch aborts the run non-zero. Expect
  both; this page is the one to work.

### Who produces the signal

There is no "history-scanner job". The real producers are:

- **Tier A** — nightly incremental chain walk
  (`verify-archive-tier-a.timer` → `-tier chain
  -from-last-verified` with a state file, under
  `run-heavy-job.sh` semantics).
- **Tier B** — checkpoint cross-check against the local
  archivist mirror (`verify-archive-tier-b.{service,timer}`).
- **Tier D** — weekly multi-peer sampling: root cron, Sunday
  16:23 (`-tier peers -peer-samples 50`, output to journald tag
  `stellarindex-tier-d`), under `run-heavy-job.sh` with its OWN
  lock, `verify-archive-tier-d` — it does not share the tier A/B
  `verify-archive` lock, so a tier A/B run never skips it and it
  never skips them. **Still metric-less** — Tier D runs
  outside the systemd units that carry `-textfile-output`, so a
  Tier D divergence pages nobody. Read the journal.

Tier A and Tier B publish through node_exporter's textfile
collector:

| Unit | `-tier` | `.prom` file | series |
| ---- | ------- | ------------ | ------ |
| `verify-archive-tier-a.service` | `chain` | `/var/lib/node_exporter/textfile_collector/verify_archive_tier_a.prom` | `…mismatches_total{tier="chain",reason=…}` |
| `verify-archive-tier-b.service` | `checkpoint` | `/var/lib/node_exporter/textfile_collector/verify_archive_tier_b.prom` | `…mismatches_total{tier="checkpoint",reason=…}` |

The files are written on EVERY exit, success or failure (a mismatch
aborts the walk, and that is exactly the run whose counter matters).
Totals are cumulative across runs — a clean run re-emits the
previous total rather than resetting it, which is what makes
`increase()` meaningful. If a `.prom` file is missing or its mtime
is older than the last timer trigger, the page is blind: check
`journalctl -u verify-archive-tier-a | grep textfile`.

#### Known blind spots

Every divergence the Tier A / Tier B walk can detect now increments
`stellarindex_verify_archive_mismatches_total`, including the two
seams that are checked OUTSIDE the per-chunk walk and used to abort
the run without touching the counter (so they surfaced only as the
severity-ticket `verify-archive-unit-failed`, never as this page):
cross-chunk boundaries (`stitchChunks`, ~11 of them in every
12-worker nightly run) and the cross-run resume seam
(`checkResumeFromHash`, which only runs under `-safety-overlap 0`;
the r1 units pass 5000, so on r1 the overlap re-walk covers that seam
instead). Both are counted as of 2026-08-29 and pinned by
`TestStitchChunks_BoundaryBreakIsPageable` /
`TestCheckResumeFromHash_MismatchIsPageable`.

What this page still cannot see:

1. **Tier D and Tier E.** Tier D (weekly multi-peer sampling, root
   cron) and Tier E (`rs-stellar-archivist` scan, operator-run) run
   outside the two systemd units that carry `-textfile-output` and
   emit no metric of any kind. A Tier D divergence surfaces in
   journald (`journalctl -t stellarindex-tier-d`) only — nothing
   pages. Wiring them is not part of #282.
2. **The first run on a host that has no `.prom` file yet.** The
   file is written when the run exits, so the very first run
   publishes a series that APPEARS at its final value. If that
   first-ever run is also the one that finds a break, the series
   appears at 1 and stays flat, and `increase()` has no earlier
   sample to subtract — it reads 0, so this page waits for the NEXT
   run's increment (the following night). Priming both units by hand
   during the apply (step 2 of the banner) is what closes this
   window: it puts the zero baseline on disk before any nightly run,
   narrowing the exposure to that single manual run. Read the
   priming run's exit status rather than trusting the page for it.

### Quick diagnosis (≤ 5 min)

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

# Diff the bucket hashes — there'll be at least one row that
# differs. That's the corrupted / diverged bucket. (For
# reason="chain"/"sequence" the journal names the exact ledger
# range instead — that's an internal lake break, no reference
# needed.)
```

### Typical root causes (in order of how much you should worry)

1. **Bit rot / storage corruption.** ZFS scrubs should catch this
   before a reader does, but a drive silently returning bad data
   between scrub cycles can corrupt buckets.
   - Investigation: `zpool status -v data` for any scrub errors on
     the archive pool.

2. **Core binary bug** — stellar-core produced a different bucket
   hash than the rest of the network. Not applicable on today's
   posture (we don't publish — the mirror was filled by
   `stellar-archivist mirror`); retained for Phase-3 (ADR-0004
   validator rollout).
   - Investigation: are we running stock stellar-core from a
     tagged release?

3. **Compromised archive storage**. Someone (malicious or mistaken
   operator) overwrote our archive with wrong data. Check
   access logs + recent filesystem mtimes under
   `/srv/history-archive`.

4. **Scanner bug.** verify-archive itself reports divergence when
   the truth is our archive is correct. Rare but possible —
   cross-check one of the reference archives against another to
   confirm it's us.

### Mitigation (urgent)

- [ ] Step 1 — **stop advertising the affected checkpoints** so
      downstream consumers stop relying on our data. (Applies when
      we serve the archive publicly / Phase-3; today the blast
      radius is our own ingest + verification chain.)

- [ ] Step 2 — determine the extent. Is it one bucket or many?
      One checkpoint or several? For `reason="chain"/"sequence"`:
      how wide is the broken ledger range?

- [ ] Step 3 — if storage corruption: restore the affected range
      from an independent copy. Today that means the AWS public
      blockchain bucket or an external reference archive (SDF /
      LOBSTR / Satoshipay) — we run **one** local mirror; the
      "three geographically-separated validator archives" are the
      ADR-0004 **aspiration**, not a present-tense resource.

- [ ] Step 4 — if core-binary bug (Phase-3 posture): this is a
      sev-1 engineering incident. Contact SDF; coordinate with the
      broader core community; potentially do an emergency binary
      swap.

- [ ] Step 5 — if compromise: SECURITY incident. Rotate archive
      credentials, inspect access logs, engage security-ops. See
      SECURITY.md.

- [ ] Verification: a full verify-archive pass over the affected
      range completes with zero mismatches; bucket hashes match
      references for all recent checkpoints.

### Root cause analysis

- Forensic copy of the diverged bucket (DO NOT overwrite it until
  analysis is done).
- Hash chain: which checkpoint was the first to diverge? Work
  back to that point.
- Storage-layer evidence: `zpool status -v data`, scrub history,
  SMART state of the backing drives.
- (Phase-3) Core version + host state when the checkpoint was
  generated.

### Known false-positive patterns

Very few. The spec is deterministic; a divergence is almost
always real. But:

- **Scanner race with an in-flight write** — scanning the mirror
  *during* a re-mirror / repair pass can read a partial file. The
  scanner should retry; if it's alerting without retry, fix the
  scanner.
- **Reference archive is the one that's wrong.** Improbable
  (they're the source of truth) but if two of the three reference
  archives agree with us and only one disagrees, it might be
  them. Cross-verify before panicking.

### Changelog

- 2026-08-29 (later, #282) — **the page is no longer inert.** The
  tier-a/tier-b units now pass `-textfile-output`, so
  `stellarindex_verify_archive_mismatches_total` reaches Prometheus
  via node_exporter's textfile collector instead of dying with the
  process (new emitter:
  `internal/ops/archive/verify_archive_textfile.go`; cumulative,
  zero-seeded, `tier`-labelled). The rule's lookback widened
  `1h → 26h` because the producer is a nightly timer — a 1h window
  showed the step for one hour in twenty-four and the SEV-1 page
  self-resolved before morning. Banner rewritten; symptoms updated
  (`chunk_idx` is not on the exported series). Pinned by
  `deploy/monitoring/rule-tests/stellar_test.yml` +
  `internal/ops/archive/verify_archive_unit_wiring_test.go`.
  Requires an ansible apply on r1 to take effect.
- 2026-08-29 — re-verified against HEAD. Symptom metric corrected:
  `stellarindex_archive_divergence_total` never existed — the rule
  (repointed 2026-06-11, F-1329) was
  `increase(stellarindex_verify_archive_mismatches_total[1h]) > 0`,
  `for: 0s`, labels `chunk_idx` + `reason∈{chain|sequence|checkpoint}`.
  Banner replaced (was triple-false: cited the nonexistent
  `scripts/ops/archive-cross-check.sh`, the wrong metric, and
  claimed the alert was live): the counter is only exported via
  the CLI's opt-in `-metrics-listen` flag which the tier-a/b units
  don't pass, prometheus.r1.yml has no verify-archive scrape job,
  and Tier D emits no metric — so the alert is INERT IN PRACTICE
  and a real mismatch surfaces as `verify_archive_unit_failed`
  (ticket, not this page); tracked in #282, with a TODO to wire or
  repoint — closed out by the entry above, later the same day. `mc cat myminio/history-archive/...` replaced —
  /srv/history-archive is a plain ZFS path (dataset data/archive),
  hex-sharded layout shown. "History-scanner job" replaced with
  the real producers (nightly Tier A incremental walk, Tier B
  mirror cross-check, weekly Tier D peer cron with journald tag
  stellarindex-tier-d — added to diagnosis). Impact widened to
  lake-integrity failures (chain/sequence reasons). "Restore from
  a replica archive (we run three)" corrected: one mirror today +
  AWS public bucket + external references; three archives are the
  ADR-0004 aspiration. `zpool status -v data` (pool name). Rule
  citation → `rules.r1/stellar.yml`; commands use r1 shapes.
- 2026-04-23 — initial draft. Urgency justified: this is a
  correctness guarantee we've explicitly committed to in ADR-0004.
- 2026-04-30 — top-of-file deployment-posture callout. r1 doesn't
  publish today (stellar-core removed 2026-04-23); the alert
  remains live via the cross-check script, but root causes /
  mitigation steps tied to a publishing core don't apply on the
  current posture. Retained for Phase-3 validator rollout.
  (Superseded 2026-08-29 — the cross-check script never existed;
  see the current banner.)

## stellarindex_stellar_archive_publish_fail

_Source page `archive.md#stellarindex_stellar_archive_publish_fail`: status current, severity P3, last verified 2026-08-29._

> **INERT ALERT (re-verified 2026-08-29).** This alert is inert
> **everywhere**, not just on r1: the metric
> `stellarindex_stellar_archive_publish_errors_total` has **no
> producer anywhere in the codebase** (F-1329; listed in
> `scripts/ci/lint-metric-refs.sh`'s `KNOWN_INERT` set). On the
> deployment side, there is no **standalone** stellar-core service
> on r1 (removed 2026-04-23) — the only core is galexie's
> captive-core subprocess, and it does **not** publish history.
> `/srv/history-archive` was filled by `stellar-archivist mirror`
> (one-shot, completed) and is read-only today via the
> verify-archive integrity tiers. No process actively *publishes*
> to it.
>
> The rule remains in both trees for Phase-3 (Tier-1 validator
> rollout, ADR-0004) when a stellar-core of ours resumes
> checkpoint-publish duty. Until then this runbook is
> *future-tense*.

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_stellar_archive_publish_fail` — **inert, see banner** |
| Severity | P3 (`severity: informational`) |
| Detected by | `configs/prometheus/rules.r1/stellar.yml` (group `stellarindex.stellar`, `severity: informational`, `for: 1h`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/stellar.yml`. |
| Typical MTTR | 1–4 h |
| Impact | stellar-core couldn't publish a checkpoint to our history archive. Not customer-visible. Matters for Tier-1 posture (ADR-0004) — we advertise continuous archive publishing. Sustained failures reflect badly on validator quality scores. |

### Symptoms

- `increase(stellarindex_stellar_archive_publish_errors_total[1h]) > 0`
  for ≥ 1 h.
- `stellar-core/info` may show a `history_archive_state` that's
  not `Published` for recent checkpoints.
- Archive scanners (GitHub's archive-divergence checker, LOBSTR's
  validator scoring) flag us as lagging on history.

### Quick diagnosis (≤ 5 min)

The archive is the `/srv/history-archive` filesystem; r1's MinIO holds
`galexie-live`, `galexie-archive` and `backups`, not a history bucket
(`configs/ansible/roles/archival-node/tasks/09-minio.yml`).

```sh
# stellar-core publisher logs
ssh root@<val-host> "journalctl -u stellar-core -n 500 --no-pager" \
  | grep -iE 'history|publish|upload'

# Newest published checkpoint and archive state file
ssh root@<val-host> "cat /srv/history-archive/.well-known/stellar-history.json | head -c 400"

# Space and ownership on the archive volume
ssh root@<val-host> "df -h /srv/history-archive; ls -ld /srv/history-archive"
```

When a publish target moves to S3/MinIO (Phase-3), replace these with the
matching `mc ls` / `mc stat` / `mc admin info` against that bucket.

### Typical root causes

1. **Archive backend outage or auth failure** (today: the
   `/srv/history-archive` volume; Phase-3: MinIO / S3).
   Credentials rotated without updating core, bucket policy
   changed, bucket full.
   - Mitigation: fix auth; confirm bucket has capacity.

2. **Network egress broken** from the stellar-core host to the
   archive endpoint.

3. **Core compiled / configured wrong**. The `HISTORY` archives
   section in `stellar-core.cfg` has wrong `put` / `get` / `mkdir`
   commands. Less common but has happened after an upgrade.

4. **Disk full on the core host** — it stages checkpoints before
   uploading. If `/tmp` or the staging dir is out of space, upload
   fails before it even starts.

### Mitigation

- [ ] Step 1 — look at core's log to see the specific upload
      error.
- [ ] Step 2 — fix the backend cause (auth / space / network).
- [ ] Step 3 — core retries the publish on its next checkpoint
      (every 64 ledgers, ~5 min). No manual retry needed.
- [ ] Step 4 — if gaps exist in the archive, run the
      archive-repair procedure: `stellar-core publish` for specific
      checkpoints. Node-recovery context in
      [`docs/operations/archival-node-bringup.md`](../archival-node-bringup.md).
- [ ] Verification: `increase(... errors_total[1h]) == 0`; archive
      scanners show us caught up.

### Known false-positive patterns

- **Very brief transient** — S3 returns a 503, core retries,
  successfully publishes on retry. Counter still went up. The
  alert's `for: 1h` threshold filters these.
- **Deliberate archive cutover** — during a storage migration we
  may temporarily disable publishing. Silence during the window.

### Changelog

- 2026-10-03 — Quick-diagnosis commands now target the real
  `/srv/history-archive` filesystem instead of a non-existent
  `myminio/history-archive` bucket.
- 2026-08-29 — re-verified against HEAD. Banner tightened: the
  alert is inert EVERYWHERE (no producer exists in the codebase at
  all — F-1329, KNOWN_INERT in scripts/ci/lint-metric-refs.sh),
  not merely inert-on-r1. "stellar-core is not running on r1"
  refined: no STANDALONE service — galexie's captive-core
  subprocess runs, and it does not publish history. Dead pointer
  `bootstrap-archival-node.md` → `archival-node-bringup.md`. The
  mc commands marked as Phase-3 placeholders — no
  myminio/history-archive bucket exists (archive is the
  /srv/history-archive filesystem; MinIO holds
  galexie-live/galexie-archive/backups). Rule citation →
  `rules.r1/stellar.yml` with group/severity/for.
- 2026-04-23 — initial draft.
- 2026-04-30 — top-of-file deployment-posture callout: this alert
  is inert on r1 (stellar-core removed 2026-04-23, no active archive
  publisher). Retained for Phase-3 validator rollout.

## Shared context: `archive.md` alerts

_Source page `archive.md`: status current, severity P1 | P3, last verified 2026-10-06._

Alerts for the R1 durable ledger mirror (`galexie-archive` bucket on MinIO, ADR-0016), the galexie captive core that feeds the lake, and host swap pressure. Rules live in `configs/prometheus/rules.r1/galexie-archive.yml` (what r1 loads) and `deploy/monitoring/rules/galexie-archive.yml`; the two files are identical. Rule `severity: page` = P1, `severity: ticket` = P3 (routed by `configs/alertmanager/alertmanager.r1.yml`).

### At a glance

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

### Shared context

- Archive alerts: no customer impact (serving unaffected; `aws-public-blockchain` backstops); the R1 mirror degrades; the archive is re-pulled from `aws-public-blockchain`, not backed up off-site.
- Scope: r1 / pubnet. `galexie-archive-fill` is installed only when `stellar_network == pubnet` (aws-public has no testnet/futurenet dataset) and removed elsewhere; the tip-lag updater is on every network, so fill remediation does not apply off pubnet.
- Partition model: galexie writes 64,000 ledgers per partition (`files_per_partition=64000`, `ledgers_per_file=1`); the fill mirrors only COMPLETE partitions. Archive tip lag is a sawtooth 0 → 64,000 (~4 days per partition, back to ~0 on the first hourly fill after a partition completes).
- Declared archive shape: genesis partition `[0, 63999]` plus `[ARCHIVE_FROM = 49,984,000 → tip]`; the middle is a deliberate capacity trim (recoverable only from `aws-public-blockchain`).
- Every mc-based guard reads through root's `local` mc alias; a rotated MinIO credential breaks all of them at once. The `aws-public` alias lives only in root's `~/.mc/config.json` and is not ansible-managed.
- Probe-blindness alerts (`_metric_stale`, `_contiguity_silent`, `_scan_degraded`, `_upstream_check_stale`, `_catchup_probe_degraded`) are `ticket`: node_exporter serves the LAST textfile forever, so a dead producer freezes the guarded alert. Fix the producer first.
- Timers and units: `galexie-archive-fill.timer` (hourly, :17 + jitter), `galexie-archive-tip-lag.timer` (5 min), `galexie-archive-contiguity.timer` (hourly), `galexie-mirror-verify.timer` (weekly), `galexie-archive-trim.timer`, `galexie-catchup-probe.timer`. Sources: `configs/ansible/roles/archival-node/templates/systemd/*.j2`. `tasks/07-galexie.yml` installs only the fill, tip-lag and contiguity timers; trim and mirror-verify come from `tasks/14-stellarindex-services.yml` (trim is left for a hand `systemctl enable --now`); the catchup-probe unit is inline in `tasks/10-observability.yml`. The `deploy/systemd/` copies have drifted and are not what runs on r1.
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
# Foreground run with the same guards (never run the script bare: it runs without
# the MemoryMax and watchdog caps, and exits 75 if the timer's run holds the lock):
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
- Means: the mirror's top-level partition coverage no longer matches its declared shape (a partition deleted or corrupted in the middle by a bad trim cutoff, manual `mc rm` or MinIO heal failure, or an overlap from a botched re-fill); only the declared trim hole is excluded. Producer: hourly `galexie-archive-contiguity.timer` -> `/usr/local/bin/galexie-archive-contiguity` -> `/var/lib/node_exporter/textfile_collector/galexie_archive_contiguity.prom`. Steady state: gauge 0, `scan_ok == 1`, `scan_last_run_unix` within the hour. Chunk-level holes inside a partition are restore-drill territory (`deploy/monitoring/rules/restore-drill.yml`; see [archive-files-missing](archive.md#stellarindex_archive_files_missing)).
- Fix:
  1. Freeze the trim: hold `compute-trim-cutoff` / any trim job until diagnosed (a mis-computed cutoff deleting live partitions is the likeliest cause).
  2. Enumerate the shape and walk it for holes/overlaps against the declared trim (`EXPECTED_TRIM` in the service env, default `64000-49983999`):

     ```sh
     mc ls local/galexie-archive/ \
       | grep -oE -- '--[0-9]+-[0-9]+' | sed 's/^--//' | sort -t- -k1,1n
     ```

  3. Overlap: compare both dirs' object counts/sizes; the newer partial one is usually a botched re-fill. Remove the incomplete one only after confirming the other is whole.
  4. Hole inside declared coverage: recover from `aws-public-blockchain` (the archive is re-pulled from there, not backed up off-site) with `galexie-archive-fill` (it copies absent partitions; incomplete ones go in via the `PARTIALS` env), then re-run the scan service and confirm the gauge returns to 0. A hole below the hot floor is skipped by the fill (see above).
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

## stellarindex_galexie_archive_mirror_stale

_Source page `archive.md#stellarindex_galexie_archive_mirror_stale`: status draft, severity P3, last verified 2026-10-03._

> **Retired 2026-10-02.** The raw archive is not mirrored off-site; it is re-pulled from SDF's public bucket (ADR-0043 §2). With `galexie_archive_mirror_enabled: false` (the default) ansible removes the units, script and `galexie_archive_mirror.prom`, so the metric is absent and this alert cannot fire on that host. The rest of this page applies only to a host that re-enables the mirror.

### At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_galexie_archive_mirror_stale` (no successful, verified mirror run in 48 h — including a host with no off-site target configured; per host, carries `instance`) |
| Severity | P3 (ticket) |
| Detected by | `deploy/monitoring/rules/storage.yml` and `configs/prometheus/rules.r1/storage.yml` |
| Producer | `scripts/ops/galexie-archive-mirror.sh` via `galexie-archive-mirror.timer` (hourly) |
| Typical MTTR | 15 min to fix a credential; a first full sync of a multi-TiB archive can run for hours |
| Impact | No immediate customer impact. While it fires, the archive has no off-site copy: a pool or box loss recovers only via re-ingest of the public Stellar history archives (days–weeks). |

### Symptoms

- The ticket names a host. `stellarindex_galexie_archive_mirror_configured{instance="<host>:9100"}`
  is `0` (no off-site target configured) or `1` (configured, but no verified
  run has succeeded in 48 h).
- `systemctl status galexie-archive-mirror.service` shows a failed or
  long-running run. An unconfigured host fails every run on purpose
  (`no DEST_ENDPOINT configured` in the journal): a run that copied nothing
  never reports success.

### Quick diagnosis (≤ 5 min)

```bash
journalctl -u galexie-archive-mirror.service -n 100 --no-pager
cat /etc/default/galexie-archive-mirror        # DEST_ENDPOINT set?
mc alias list | grep r2-archive                # alias configured?
mc mirror --dry-run local/galexie-archive r2-archive/stellarindex-galexie-archive
```

### Mitigation (≤ 15 min)

- **`configured 0`** — no target. Set `galexie_archive_mirror_s3_endpoint`
  in the host inventory and `galexie_archive_mirror_s3_key` /
  `galexie_archive_mirror_s3_key_secret` in the vault, then apply with
  `--tags backup` (or `galexie-archive-mirror`). `systemctl start
  galexie-archive-mirror.service` starts the first sync.
- **mirror fails (network/credential)** — fix the vars/bucket policy and
  re-apply; the next hourly run retries. `mc mirror` is incremental, so a
  retry after a partial failure resumes rather than restarting.
- **dry-run verification fails** — the script treats a non-zero exit from
  the post-mirror `mc mirror --dry-run` as a failure even with no stdout
  (an auth/network failure can print nothing); check `mc alias list` and
  bucket permissions.
- **stdout still lists objects after mirror** — the sync did not
  converge (quota, throttling); check the target bucket's free space.

### Restore

The archive is append-only, so a restore is: point a fresh MinIO at the
off-site bucket read-only, or `mc mirror` from `r2-archive/<bucket>` back to
a rebuilt `local/galexie-archive`. Re-run
`scripts/ci/galexie-archive-contiguity-test.sh`'s underlying probe
(`galexie-archive-contiguity.sh`) against the restored archive before
pointing ingest at it.

### Root cause analysis

`mc mirror` alone can exit 0 having copied nothing useful if the source
listing is empty or truncated; the post-mirror `mc mirror --dry-run`
verification exists to catch that — see the header of
`scripts/ops/galexie-archive-mirror.sh` for why its exit status and its
stdout are checked separately.

### Known false-positive patterns

- A freshly provisioned host fires until its first full sync completes.

### Changelog

- 2026-09-28 — created with the mirror job (NS03).

## Related

**`archive.md` alerts**

- [backup-offsite](backup-offsite.md): off-site backup alerts.

**stellarindex_stellar_archive_divergence**

- `verify-archive.md#stellarindex_verify_archive_unit_failed` — the ticket-severity sibling a
  mismatch also trips (the run exits non-zero).
- `verify-archive.md#stellarindex_verify_archive_run_stale` — the timer-staleness sibling.
- `archive.md#stellarindex_stellar_archive_publish_fail` — when we fail to publish at all
  (Phase-3; inert everywhere today).
- ADR-0004 (three-validator aspiration + independent archives).
- ADR-0016 (per-region storage + trust model).
- SECURITY.md — if compromise suspected.

**stellarindex_stellar_archive_publish_fail**

- `archive.md#stellarindex_stellar_archive_divergence` — when what we publish differs from
  other validators (much worse than not publishing at all; also
  inert-in-practice today — see its banner).
- `db-disk-full.md` — staging-dir disk-full variant.
- ADR-0004 (three-validator + independent archives).

**`archive.md` alerts**

- ADR-0016 (R1 = full mirror), ADR-0027 (hot floor + trim, `docs/operations/lcm-cache-tiering.md`), [archive-files-missing](archive.md#stellarindex_archive_files_missing), [bootstrap-archival-node](bootstrap-archival-node.md), [galexie-archive-mirror](archive.md#stellarindex_archive_files_missing).

**stellarindex_galexie_archive_mirror_stale**

- [Off-site backup plan](../off-site-backup-plan.md) §1
- [ch-lake-backup](ch-lake-backup.md) — the same inert-mechanism-first precedent for the lake's data backup
