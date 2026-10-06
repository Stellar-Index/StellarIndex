---
title: Runbook — verify-archive
last_verified: 2026-10-06
status: living
severity: P2
---

# Runbook — verify-archive alerts

Four alerts cover the archive verification tiers. Rules: `configs/prometheus/rules.r1/verify-archive.yml` (group `stellarindex.verify_archive`, the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/verify-archive.yml`). Component `archive`. All are R1-only: R2/R3 trust R1 for chain-link and run no nightly timer.

| Tier | What it checks | Schedule | Alerts |
| ---- | -------------- | -------- | ------ |
| A (`-tier chain`) | Chain-link integrity (`prev_hash` of each ledger vs the previous ledger's hash) | `verify-archive-tier-a.service`/`.timer`, nightly 03:23 UTC | `_unit_failed` (ticket), `_run_stale` (page) |
| B (`-tier checkpoint`) | Header-hash of every 64-ledger checkpoint vs the local `/srv/history-archive` mirror | `verify-archive-tier-b.service`/`.timer`, nightly 04:37 UTC | `_tier_b_unit_failed`, `_tier_b_run_stale` (ticket) |
| D (`-tier peers`) | Sampled peer archives' checkpoint hashes vs ours | root cron, Sunday 16:23 UTC | `_tier_d_run_stale` (ticket) |

Ticket alerts: no immediate impact, the API serves correct data from existing bytes (the P2 `_run_stale` page has its own impact note below). R1 is the integrity leader (ADR-0016); the cross-region trust property degrades with each missed cycle. Staleness alerts read `stellarindex_verify_archive_last_success_unix{tier=...}`, a node_exporter textfile gauge the binary advances ONLY on a clean exit (`internal/ops/archive/verify_archive_textfile.go`); a failed run carries the prior value forward. Each has an `absent_over_time` branch so a never-written textfile also fires; the `> 0` guard means a fresh host that never completed a run does not fire.

A missing-file gap in the archive is the most common cause of a verify-archive failure; `stellarindex_archive_files_missing` (see [archive-completeness](archive-completeness.md#stellarindex_archive_files_missing)) often co-fires.

Shared lock: Tier A, Tier B and manual runs share the `verify-archive` heavy-job lock, keyed on the first argument to `run-heavy-job.sh`. A manual run is refused (exit 75) while a timer run holds it; a timer fire queues behind a manual run (or the other tier) for up to 20h (`HEAVY_JOB_LOCK_WAIT`, the unit shows `activating`) and fails only if the wait runs out ("still held after waiting" in the journal). Find the holder:

```sh
fuser -v /run/lock/stellarindex-heavy-verify-archive.lock
```

Do not start manual runs while a timer is active, and always wrap manual Tier A/B runs as `/usr/local/sbin/run-heavy-job.sh verify-archive ...` (the name must match the unit; `scripts/ci/lint-verify-archive-lock-name.sh` enforces it). Tier D deliberately uses its own lock, `verify-archive-tier-d`, so a long Tier A walk cannot swallow the weekly fork check.

Shared access and units:

```sh
ssh root@136.243.90.96
set -a; source /etc/default/stellarindex-ops; set +a   # creds for manual runs
```

Max-runtime: the ansible templates (`configs/ansible/roles/archival-node/templates/systemd/verify-archive-tier-{a,b}.service.j2`) and the `deploy/systemd/verify-archive-tier-{a,b}.service` reference copies both set `VERIFY_ARCHIVE_MAX_RUNTIME=0` (uncapped, bounded by `WatchdogSec=1h`, which kills on 1h of silence, not on duration); the authority lint enforces that they agree. The binary's own `-max-runtime` default (24h) applies only to a manual run that omits the flag. The unit runs `Type=notify`, incrementally with `-from-last-verified`, `-state-file /var/lib/stellarindex/verify-archive-state.json`, `-safety-overlap 5000`, under `run-heavy-job.sh`; Tier A and B share one state file keyed per tier. Steady-state nightly runs are a short tip-window walk; only a first-ever or state-lost run takes the ~13.8h full pass.

Corruption signals (`chain break`, `checkpoint anchor ... MISMATCH`, peers disagree): STOP. This is what the system exists to surface; do NOT auto-recover. Escalate per [the RCA](#root-cause-analysis-chain-break-or-anchor-mismatch).

## At a glance

- [`stellarindex_verify_archive_unit_failed`](#stellarindex_verify_archive_unit_failed) — P3, Tier A nightly run failed
- [`stellarindex_verify_archive_run_stale`](#stellarindex_verify_archive_run_stale) — P2 page, Tier A no clean run in 36h
- [`stellarindex_verify_archive_tier_b_unit_failed`](#stellarindex_verify_archive_tier_b_unit_failed) — P3, Tier B run failed
- [`stellarindex_verify_archive_tier_b_run_stale`](#stellarindex_verify_archive_tier_b_run_stale) — P3, Tier B no clean run in 36h
- [`stellarindex_verify_archive_tier_d_run_stale`](#stellarindex_verify_archive_tier_d_run_stale) — P3, Tier D no success in 8d
- [Root cause analysis](#root-cause-analysis-chain-break-or-anchor-mismatch), [operator hygiene](#operator-hygiene-tmpva-log-cleanup)

## stellarindex_verify_archive_unit_failed

P3 (`severity: ticket`), `for: 5m` (rules out transient systemd state during a manual restart). The most-recent run of `verify-archive-tier-a.service` exited `failed`. MTTR 30 min (diagnosis) to several hours (re-run).

Causes (per the rule): chain-link mismatch (real corruption); missing file in the galexie-archive bucket; max-runtime/watchdog; lock wait expired (see shared lock above).

Diagnose (5 min):

```sh
systemctl status verify-archive-tier-a.service
journalctl -u verify-archive-tier-a.service --since "yesterday" --no-pager | tail -50
# missing-file gap is the most common cause; both alerts fire together
journalctl -u archive-completeness.service --since "yesterday" --no-pager | tail -20
```

| Pattern | Cause |
| ------- | ----- |
| `chain break at ledger L` | Real corruption, escalate. Emitted by `verify_archive_chunks.go:180` (chunk boundary) and `:320` (in-chunk). Tier B's analogue is `checkpoint anchor mismatch at ledger N`. |
| `is missing` (the datastore's missing-object error) | Missing file in galexie-archive; should also fire `stellarindex_archive_files_missing`. Trailing-edge misses (files galexie hasn't written yet) are tolerated. |
| `WATCHDOG` timeout / `SIGTERM` | 1h of silence tripped `WatchdogSec`; investigate what the walk was stuck on. |
| `still held after waiting` | Lock wait expired; find the holder (shared lock above). |
| `access denied` / `403` | AWS / MinIO credentials in `/etc/default/stellarindex-ops` rotated or wrong. |

Mitigation (15 min):

- [ ] Re-run manually to confirm the failure is reproducible, not a transient blip:
  ```sh
  /usr/local/sbin/run-heavy-job.sh verify-archive \
    /usr/local/bin/stellarindex-ops verify-archive \
    -config /etc/stellarindex.toml \
    -from 2 \
    -tier chain \
    -workers 8 \
    -max-runtime 1h
  # To reproduce the scheduled unit's incremental window instead, add:
  #   -state-file /var/lib/stellarindex/verify-archive-state.json -from-last-verified
  ```
  A 1h budget on 8 workers gets enough coverage to confirm whether the failure persists. Don't push the full run; the next timer will retry. (`-from 2`: ledger 1 has no predecessor.)
- [ ] If `is missing`: backfill from the public-archive fallback chain. The subcommand has NO `-config` flag, and `-to` and `-write` are both REQUIRED (without `-write` it is a dry-run that writes nothing):
  ```sh
  # head = current network tip ledger (e.g. from /v1/diagnostics/cursors)
  /usr/local/sbin/run-heavy-job.sh archive-completeness-fix \
    /usr/local/bin/stellarindex-ops archive-completeness fix \
    -write -from 2 -to <head-ledger>
  # or re-fire the scheduled path (check, fix, re-check + textfile):
  systemctl start archive-completeness.service
  ```
  Then re-run verify-archive.
- [ ] If `chain break` (or Tier B `checkpoint anchor mismatch`): STOP, escalate per the RCA.
- [ ] Verify: the next scheduled run completes cleanly (Prometheus or `systemctl is-active`).

False positives: a manual run holding the lock (shared lock above); a MinIO restart mid-run surfaces as a chain-walk connection reset, so the next run completes cleanly. Don't escalate unless it repeats two nights in a row.

## stellarindex_verify_archive_run_stale

P2 (`severity: page`), `for: 10m` (rules out a node_exporter scrape blip). `verify-archive-tier-a` (`tier="chain"`) has had no clean completion for 36h (24h cadence + 12h cushion), or the series is absent. MTTR about 1h. Impact: R2/R3 trust R1 for chain integrity (ADR-0016); the longer R1 goes without a clean nightly verify, the further the fleet drifts from byte-identical everywhere. Usually preceded by `_unit_failed` on each earlier night; this page means that ticket was not actioned in time.

Because the gauge advances only on a clean exit, "timer fires but every run fails" DOES trip this page (it did not when the expr read the timer's last trigger).

Diagnose (5 min):

```sh
systemctl status verify-archive-tier-a.timer
systemctl list-timers verify-archive-tier-a.timer --all
journalctl -u verify-archive-tier-a.service --since "3 days ago" \
  | grep -E "Started|Finished|FAILED|Failed|exit-code"
```

Also check `archive_files_missing`: an upstream gap blocks verify.

| State | Cause | Action |
| ----- | ----- | ------ |
| `inactive (dead)`, last trigger > 36h ago | Timer disabled (forgotten after maintenance) | `systemctl enable --now verify-archive-tier-a.timer` |
| `active (waiting)`, last trigger fresh, yet alert fired | Expected: timer fires but every run FAILS | `journalctl -u verify-archive-tier-a.service`; [unit_failed](#stellarindex_verify_archive_unit_failed) should also be firing |
| `active (running)` for hours | Hung run, or the expected ~13.8h full pass | Watch per-chunk progress (below) |

Mitigation (15 min):

- [ ] Timer disabled: `systemctl enable --now verify-archive-tier-a.timer` is the whole fix for the most common cause. Verify the next trigger is < 24h ahead.
- [ ] Runs failing: follow [unit_failed](#stellarindex_verify_archive_unit_failed) to bring one manual run to green. One clean run clears this alert.
- [ ] Hung run: `journalctl -u verify-archive-tier-a.service -f`. If per-chunk progress is steady, wait (the watchdog kills on silence, not duration). If output is frozen, let the watchdog SIGTERM it (or stop it yourself) and investigate. Do NOT raise `-max-runtime`: the uncapped setting is intentional (Task #13 mid-pass-cap incident), and runtime isn't the limiter.
- [ ] Communicate degradation: post on the status page and the on-call channel; that is the whole action, there is no config to flip. The API's `flags.reduced_redundancy` (`internal/api/v1/envelope.go`) has no producer: per ADR-0017 the R2/R3 regions set it when R1's last successful run is stale, and they are not provisioned yet ([L4.14 / L4.15](../../architecture/launch-readiness-backlog.md)). Until then the flag stays `false`. After 48h consider the degradation comms call for R2/R3.
- [ ] Verify: a clean run completes within 24h; the alert clears.

Postmortem questions: why was the `_unit_failed` ticket not actioned (adjust SLA if needed); was there a coincident upstream archive incident; did `archive-completeness` also drift (a missing-file gap can starve verify-archive indefinitely). Gather three nights of `verify-archive-tier-a.service` and `archive-completeness.service` logs, public Stellar archive status snapshots, and the `last_verified` chain on relevant ADRs (a documented invariant may be invalidated).

False positives: a planned maintenance window that disabled the timer for > 36h (re-enable, accept one alert); Prometheus clock skew (investigate node_exporter health if it persists).

## stellarindex_verify_archive_tier_b_unit_failed

P3 (`severity: ticket`), `for: 5m`. `verify-archive-tier-b.service` exited `failed`. Tier B catches single-source corruption that is still chain-link-consistent, which Tier A is blind to (a self-consistent-but-wrong chain passes Tier A). It is defence-in-depth over the page-level Tier A check. MTTR 30 min (diagnosis) to hours (mirror re-sync / re-run). Failed/stale Tier B means our LCM header-hashes are not being confirmed against the local mirror.

Causes (per the rule): checkpoint anchor mismatch; local `/srv/history-archive` mirror missing/stale for the checked range (every checkpoint "missed", inconclusive-and-fatal, DAT-09); max-runtime/watchdog; lock wait expired (shared lock above).

Diagnose (5 min):

```sh
systemctl status verify-archive-tier-b.service verify-archive-tier-b.timer
journalctl -u verify-archive-tier-b.service --since "yesterday" --no-pager | tail -60
# empty/stale mirror makes every checkpoint "missed"
ls /srv/history-archive/ledger | head; df -h /srv/history-archive
```

| Pattern | Cause |
| ------- | ----- |
| `checkpoint anchor ... MISMATCH` | Our LCM header-hash disagrees with the mirror's canonical hash. Single-source corruption, escalate. |
| `checkpoint anchor inconclusive — N missed, 0 matched` | Mirror lacks the checked range (not synced far enough). Not corruption; a mirror-sync gap. |
| `N checkpoint(s) missing from cross-anchor archive (with -fail-on-missed ...)` | Partial miss (some matched, some missed) inside the mirror's own coverage: a real hole, not sync lag. Raised by default; suppressed only by `-fail-on-missed=false`. |
| `context deadline exceeded` / watchdog | Hit its runtime bound; investigate per-chunk progress. |
| `still held after waiting` | Lock wait expired (shared lock above). |
| `access denied` / `403` | galexie-archive S3 creds in `/etc/default/stellarindex-ops` rotated/wrong. |

Mitigation:

- [ ] Anchor MISMATCH: STOP. Compare the offending checkpoint's header-hash against a peer archive (SDF / Lobstr / SatoshiPay) and escalate per the [RCA](#root-cause-analysis-chain-break-or-anchor-mismatch).
- [ ] Inconclusive / all-missed (mirror gap): advance the mirror (the rs-stellar-archivist sync that backfills `/srv/history-archive`), then re-run Tier B manually. Never pass `-fail-on-missed=false` here (ADR-0017 X1.7): with it, a partial miss inside the mirror's coverage span exits 0 and prints `checkpoint anchor OK`, so the mitigation looks resolved while a hole goes unreported:
  ```sh
  /usr/local/sbin/run-heavy-job.sh verify-archive \
    /usr/local/bin/stellarindex-ops verify-archive \
    -config /etc/stellarindex.toml \
    -tier checkpoint -archive-root /srv/history-archive \
    -from 2 -workers 8 -max-runtime 1h \
    -fail-on-missed
  ```
- [ ] Verify: the next scheduled `verify-archive-tier-b.service` run completes cleanly (state `active`/`inactive`, not `failed`).

## stellarindex_verify_archive_tier_b_run_stale

Same family and diagnosis as [tier_b_unit_failed](#stellarindex_verify_archive_tier_b_unit_failed); differences: P3 ticket, `for: 10m`, fires when `tier="checkpoint"` has no clean completion for 36h+ (24h cadence + 12h cushion) or the series is absent, i.e. the timer is disabled or every recent run failed, so the single-source anchor check is not running. Lower urgency than the Tier A staleness page.

Diagnose: `systemctl status verify-archive-tier-b.timer` (active?), `journalctl -u verify-archive-tier-b.service` (what's failing?), is the `/srv/history-archive` mirror synced to the checked range?

Mitigation: confirm the timer is enabled (`systemctl enable --now verify-archive-tier-b.timer`). During a long Tier A run a Tier B fire queues behind the shared `verify-archive` lock and runs when Tier A finishes (unit shows `activating`); a wait that runs out fails the unit with "still held after waiting" (find the holder with `fuser -v /run/lock/stellarindex-heavy-verify-archive.lock`). If runs are failing, use the unit_failed section. Verify as there.

## stellarindex_verify_archive_tier_d_run_stale

P3 (`severity: ticket`), `for: 30m`. No clean exit for `tier="peers"` in over 8 days (weekly cycle plus one day slack), or the series is absent. MTTR 30 min diagnosis; a re-run takes minutes to an hour. Impact: the weekly multi-peer cross-check hasn't confirmed our archive against the network; a genuine divergence would surface only in journald until the next success. Tier A's nightly chain-link carries the page-level signal.

Tier D (`stellarindex-ops verify-archive -tier peers`) samples 50 peer archives and compares their checkpoint hashes with ours, the failure mode Tier A/B cannot see because both anchor on our own mirror (ADR-0016 section 7.4). It is a weekly cron entry (`stellarindex-verify-archive-tier-d`, Sunday 16:23 UTC, `configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml`), not a systemd timer, so there is no `node_systemd_unit_state` series; staleness is the only signal, covering both "every run failed" and "cron entry never installed" (gated on `verify_archive_tier_d_enabled`, pubnet only, since a single-region testnet/futurenet host has no peer region).

Diagnose (5 min):

```sh
grep verify-archive-tier-d /etc/cron.d/stellarindex-verify-archive-tier-d
journalctl -t stellarindex-tier-d --since "-10 days" --no-pager | tail -80
# or, depending on syslog routing: grep stellarindex-tier-d /var/log/syslog
```

| Pattern | Cause |
| ------- | ----- |
| `PEERS DISAGREE` / mismatch reported | A sampled peer's checkpoint hash disagrees with ours. Escalate as a possible fork; compare the disputed checkpoint against a second peer before concluding which side is wrong. |
| `SELF DIVERGES FROM PEER CONSENSUS` / `our archive ... diverges` | Peers agree with each other but our `-archive-root` checkpoint JSON differs. Possible fork or corruption of OUR mirror; same STOP. |
| `our archive ... matched no consensus-verified checkpoint` / `missing from our archive` | Our mirror's `history/` tree lacks the sampled checkpoints (wrong `-archive-root`, or a hole in coverage). Check the mirror before re-running. |
| `verify-archive: ...` parse or config error | Rendered flags are wrong for this host shape; see `scripts/ci/verify-archive-tier-d-test.sh`. |
| no log entry in the window | Cron entry not installed, or `run-heavy-job.sh`'s lock was held by another heavy job for the whole window. Check `verify_archive_tier_d_enabled` in inventory. |

Mitigation:

- [ ] Peers disagree: STOP, do not auto-recover. Compare the disputed checkpoint against a second and third peer to determine which side diverges.
- [ ] Cron entry missing: re-run the archival-node role's `14-stellarindex-services.yml` tasks (tag `ops-jobs`) with `verify_archive_tier_d_enabled: true` in inventory.
- [ ] Manual re-run: use the cron's exact command line (task "Install Tier D verify-archive weekly cron" in `configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml`): `HEAVY_JOB_CLASS=scheduled /usr/local/sbin/run-heavy-job.sh verify-archive-tier-d /usr/local/bin/stellarindex-ops verify-archive -config /etc/stellarindex.toml -tier peers -peer-samples 50 -from <hot floor> -textfile-output /var/lib/node_exporter/textfile_collector/verify_archive_tier_d.prom`, with the ops environment file loaded first and `-to <live seam ledger - 1>` if the cron renders one. Without `-textfile-output` a hand run cannot clear the alert.
- [ ] Verify: the next scheduled (or hand) run completes cleanly and advances `stellarindex_verify_archive_last_success_unix{tier="peers"}`.

## Root cause analysis: chain break or anchor mismatch

A real mismatch means the archive's trust anchor is in question. Candidates:

1. Upstream archive corruption: Stellar's published history archive disagrees with what we mirrored. Rare but real. Pull the offending ledger from a different upstream archive (SDF / Lobstr / SatoshiPay) and compare.
2. Mirror corruption in transit: re-fetch the offending range; confirm the new copy verifies.
3. Decoder bug: check git log for recent decoder changes; reproduce against the same range under the previous binary.

Gather: `journalctl -u verify-archive-tier-a.service --since '24h ago'` (Tier B: `-u verify-archive-tier-b.service`); the offending hash and expected hash for the reported ledger pair; another archive's same-range hashes; recent commits to `cmd/stellarindex-ops/`, `internal/ops/archive/` (verify code) and `internal/ledgerstream/`.

## Operator hygiene: /tmp/va-*.log cleanup

Manual long-range `verify-archive` runs produce multi-GB stdout, usually captured by redirection (e.g. `stellarindex-ops verify-archive -config /etc/stellarindex.toml -from 2 -to 0 > /tmp/va-full.log 2>&1`; floor is `-from 2`). The 2026-05-26 audit found about 4 GB of orphaned `/tmp/va-*.log` on r1 after one investigation. The binary does not create them, so the operator cleans up. Manual long-range scans are heavy jobs: always go through `run-heavy-job.sh`.

```sh
LOGFILE=$(mktemp /tmp/va-XXXXXX.log)
trap 'gzip -9 "$LOGFILE" >/dev/null 2>&1; mv "${LOGFILE}.gz" /var/log/stellarindex/ 2>/dev/null || rm -f "$LOGFILE" "${LOGFILE}.gz"' EXIT
/usr/local/sbin/run-heavy-job.sh verify-archive \
  stellarindex-ops verify-archive -config /etc/stellarindex.toml -from "$FROM" -to "$TO" > "$LOGFILE" 2>&1
```

The log then lands under `/var/log/stellarindex/` (picked up by logrotate, F-0009) or is deleted on exit. The scheduled units don't need this: stdout goes to the journal, rotated by journald.

## Related

- ADR-0016 (per-region trust model), ADR-0017 (archive-completeness invariants, checkpoint tiers, DAT-09).
- `docs/operations/archival-node-bringup.md` section "Per-region trust + verification model".
- [archive-completeness](archive-completeness.md#stellarindex_archive_files_missing), the adjacent failure mode that often co-fires.
