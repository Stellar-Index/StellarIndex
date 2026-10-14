---
title: Runbook — verify-archive
last_verified: 2026-10-06
status: living
severity: P2
---

# Runbook — verify-archive alerts

## At a glance

Verify-archive run and unit alerts for tiers A, B and D.

Rules: `configs/prometheus/rules.r1/verify-archive.yml` (group `stellarindex.verify_archive`; twin in `deploy/monitoring/rules/verify-archive.yml`). R1-only; component `archive`. Staleness alerts read `stellarindex_verify_archive_last_success_unix{tier=...}`, a textfile gauge advanced ONLY on a clean exit (`internal/ops/archive/verify_archive_textfile.go`); a never-written textfile also fires (`absent_over_time`).

| Tier | What it checks | Schedule | Alerts |
| ---- | -------------- | -------- | ------ |
| A (`-tier chain`) | `prev_hash` chain-link integrity | `verify-archive-tier-a.service`/`.timer`, nightly 03:23 UTC | `_unit_failed` (ticket), `_run_stale` (page) |
| B (`-tier checkpoint`) | Header-hash of each 64-ledger checkpoint vs local `/srv/history-archive` | `verify-archive-tier-b.service`/`.timer`, nightly 04:37 UTC | `_tier_b_unit_failed`, `_tier_b_run_stale` (ticket) |
| D (`-tier peers`) | Sampled peer archives' checkpoint hashes vs ours | root cron, Sunday 16:23 UTC | `_tier_d_run_stale` (ticket) |

A missing-file gap is the most common cause of failure; [`stellarindex_archive_files_missing`](archive.md#stellarindex_archive_files_missing) often co-fires.

Shared lock: Tier A, Tier B and manual runs share the `verify-archive` heavy-job lock, keyed on the first argument to `run-heavy-job.sh`. A manual run is refused (exit 75) while a timer run holds it; a timer fire queues up to 20h (`HEAVY_JOB_LOCK_WAIT`, unit shows `activating`) and fails with "still held after waiting". Find the holder:

```sh
fuser -v /run/lock/stellarindex-heavy-verify-archive.lock
```

Do not start manual runs while a timer is active; wrap manual Tier A/B runs as `/usr/local/sbin/run-heavy-job.sh verify-archive ...` (the name must match the unit; `scripts/ci/lint-verify-archive-lock-name.sh` enforces it). Tier D uses its own lock, `verify-archive-tier-d`.

```sh
ssh root@136.243.90.96
set -a; source /etc/default/stellarindex-ops; set +a   # creds for manual runs
```

Units run `Type=notify`, incrementally (`-from-last-verified`, `-state-file /var/lib/stellarindex/verify-archive-state.json`, `-safety-overlap 5000`) with `VERIFY_ARCHIVE_MAX_RUNTIME=0` (uncapped, bounded by `WatchdogSec=1h` of silence). Only a first-ever or state-lost run takes the ~13.8h full pass.

Corruption signals (`chain break`, `checkpoint anchor ... MISMATCH`, peers disagree): STOP. Do NOT auto-recover. Escalate per [the RCA](#root-cause-analysis-chain-break-or-anchor-mismatch).

## stellarindex_verify_archive_unit_failed

P3 (`severity: ticket`), `for: 5m`. The latest `verify-archive-tier-a.service` run exited `failed`.

**Check**

```sh
systemctl status verify-archive-tier-a.service
journalctl -u verify-archive-tier-a.service --since "yesterday" --no-pager | tail -50
# missing-file gap is the most common cause; both alerts fire together
journalctl -u archive-completeness.service --since "yesterday" --no-pager | tail -20
```

**Likely causes**

- `chain break at ledger L`: real corruption, escalate (Tier B analogue: `checkpoint anchor mismatch at ledger N`).
- `is missing`: missing file in galexie-archive; should also fire `stellarindex_archive_files_missing`. Trailing-edge misses are tolerated.
- `WATCHDOG` / `SIGTERM` (1h of silence) or `still held after waiting` (find the lock holder).
- `access denied` / `403`: credentials in `/etc/default/stellarindex-ops` rotated or wrong.

**Fix**

1. Re-run manually to confirm it reproduces (`-from 2`: ledger 1 has no predecessor):
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
2. `is missing`: backfill from the fallback chain (no `-config` flag; `-to` and `-write` REQUIRED, else dry run), then re-run verify-archive:
   ```sh
   # head = current network tip ledger (e.g. from /v1/diagnostics/cursors)
   /usr/local/sbin/run-heavy-job.sh archive-completeness-fix \
     /usr/local/bin/stellarindex-ops archive-completeness fix \
     -write -from 2 -to <head-ledger>
   # or re-fire the scheduled path (check, fix, re-check + textfile):
   systemctl start archive-completeness.service
   ```
3. `chain break` or Tier B `checkpoint anchor mismatch`: STOP, escalate per the RCA.
4. Verify the next scheduled run completes cleanly. A single MinIO-restart connection reset is not an escalation; two nights in a row is.

## stellarindex_verify_archive_run_stale

P2 (`severity: page`), `for: 10m`. `tier="chain"` has had no clean completion for 36h, or the series is absent. R2/R3 trust R1 for chain integrity (ADR-0016). Usually preceded by `_unit_failed` on earlier nights. A timer that fires but fails every run DOES trip this page.

**Check**

```sh
systemctl status verify-archive-tier-a.timer
systemctl list-timers verify-archive-tier-a.timer --all
journalctl -u verify-archive-tier-a.service --since "3 days ago" \
  | grep -E "Started|Finished|FAILED|Failed|exit-code"
```

Also check `archive_files_missing`: an upstream gap blocks verify.

**Likely causes**

- `inactive (dead)`, last trigger > 36h ago: timer disabled after maintenance.
- `active (waiting)` with a fresh trigger: every run FAILS; [unit_failed](#stellarindex_verify_archive_unit_failed) should also fire.
- `active (running)` for hours: hung run, or the expected ~13.8h full pass.

**Fix**

1. Timer disabled: `systemctl enable --now verify-archive-tier-a.timer`; verify the next trigger is < 24h ahead.
2. Runs failing: follow [unit_failed](#stellarindex_verify_archive_unit_failed) until one manual run is green; one clean run clears the alert.
3. Hung run: `journalctl -u verify-archive-tier-a.service -f`. Steady per-chunk progress: wait. Frozen output: let the watchdog SIGTERM it (or stop it) and investigate. Do NOT raise `-max-runtime`; uncapped is intentional.
4. Post on the status page and on-call channel. `flags.reduced_redundancy` has no producer until R2/R3 exist (L4.14 / L4.15 in docs/operations/v1-launch-plan.md).

**Escalate** Verify a clean run completes within 24h. Postmortem: why the `_unit_failed` ticket was not actioned; three nights of `verify-archive-tier-a.service` and `archive-completeness.service` logs.

## stellarindex_verify_archive_tier_b_unit_failed

P3 (`severity: ticket`), `for: 5m`. `verify-archive-tier-b.service` exited `failed`. Tier B catches single-source corruption that is still chain-consistent, which Tier A cannot see.

**Check**

```sh
systemctl status verify-archive-tier-b.service verify-archive-tier-b.timer
journalctl -u verify-archive-tier-b.service --since "yesterday" --no-pager | tail -60
# empty/stale mirror makes every checkpoint "missed"
ls /srv/history-archive/ledger | head; df -h /srv/history-archive
```

**Likely causes**

- `checkpoint anchor ... MISMATCH`: our header-hash disagrees with the mirror's; single-source corruption, escalate.
- `checkpoint anchor inconclusive — N missed, 0 matched`: the mirror lacks the checked range (sync gap, not corruption).
- `N checkpoint(s) missing from cross-anchor archive (with -fail-on-missed ...)`: a real hole inside the mirror's coverage.
- `context deadline exceeded` / watchdog; `still held after waiting`; `access denied` / `403` (creds in `/etc/default/stellarindex-ops`).

**Fix**

1. MISMATCH: STOP. Compare the checkpoint's header-hash with a peer archive (SDF / Lobstr / SatoshiPay) and escalate per the [RCA](#root-cause-analysis-chain-break-or-anchor-mismatch).
2. Inconclusive or all-missed: advance the mirror (the rs-stellar-archivist sync that fills `/srv/history-archive`), then re-run. Never pass `-fail-on-missed=false` (ADR-0017 X1.7): it hides partial holes behind `checkpoint anchor OK`.
   ```sh
   /usr/local/sbin/run-heavy-job.sh verify-archive \
     /usr/local/bin/stellarindex-ops verify-archive \
     -config /etc/stellarindex.toml \
     -tier checkpoint -archive-root /srv/history-archive \
     -from 2 -workers 8 -max-runtime 1h \
     -fail-on-missed
   ```
3. Verify the next scheduled run completes (state `active`/`inactive`, not `failed`).

## stellarindex_verify_archive_tier_b_run_stale

P3 ticket, `for: 10m`: `tier="checkpoint"` has no clean completion for 36h+ or the series is absent (timer disabled, or every run failed). Diagnose as [tier_b_unit_failed](#stellarindex_verify_archive_tier_b_unit_failed): `systemctl status verify-archive-tier-b.timer`, `journalctl -u verify-archive-tier-b.service`, is the `/srv/history-archive` mirror synced to the range?

Fix: `systemctl enable --now verify-archive-tier-b.timer`. During a long Tier A run a Tier B fire queues behind the shared lock (find the holder with `fuser -v /run/lock/stellarindex-heavy-verify-archive.lock`). Failing runs: use the unit_failed section.

## stellarindex_verify_archive_tier_d_run_stale

P3 (`severity: ticket`), `for: 30m`. No clean exit for `tier="peers"` in over 8 days, or the series is absent. Tier D (`stellarindex-ops verify-archive -tier peers`) samples 50 peer archives against ours, the failure mode Tier A/B cannot see (ADR-0016 section 7.4). It is a weekly cron entry (`stellarindex-verify-archive-tier-d`, `configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml`, gated on `verify_archive_tier_d_enabled`, pubnet only), not a systemd timer; staleness is the only signal.

**Check**

```sh
grep verify-archive-tier-d /etc/cron.d/stellarindex-verify-archive-tier-d
journalctl -t stellarindex-tier-d --since "-10 days" --no-pager | tail -80
# or, depending on syslog routing: grep stellarindex-tier-d /var/log/syslog
```

**Likely causes**

- `PEERS DISAGREE`: a sampled peer's hash disagrees with ours; possible fork. Compare against a second and third peer before concluding which side diverges.
- `SELF DIVERGES FROM PEER CONSENSUS` / `our archive ... diverges`: possible corruption of OUR mirror; STOP.
- `matched no consensus-verified checkpoint` / `missing from our archive`: wrong `-archive-root` or a coverage hole.
- No log entry: cron entry not installed, or the lock was held for the whole window.

**Fix**

1. Disagreement: STOP, do not auto-recover.
2. Cron missing: re-run `14-stellarindex-services.yml` (tag `ops-jobs`) with `verify_archive_tier_d_enabled: true`.
3. Manual re-run with the cron's exact command, ops environment loaded first, plus `-to <live seam ledger - 1>` if the cron renders one. Without `-textfile-output` a hand run cannot clear the alert:
   `HEAVY_JOB_CLASS=scheduled /usr/local/sbin/run-heavy-job.sh verify-archive-tier-d /usr/local/bin/stellarindex-ops verify-archive -config /etc/stellarindex.toml -tier peers -peer-samples 50 -from <hot floor> -textfile-output /var/lib/node_exporter/textfile_collector/verify_archive_tier_d.prom`
4. Verify `stellarindex_verify_archive_last_success_unix{tier="peers"}` advances.

## Root cause analysis: chain break or anchor mismatch

The trust anchor is in question. Candidates: (1) upstream archive corruption (pull the ledger from another archive: SDF / Lobstr / SatoshiPay); (2) mirror corruption in transit (re-fetch the range, confirm it verifies); (3) decoder bug (check recent decoder commits; reproduce under the previous binary).

Gather: `journalctl -u verify-archive-tier-a.service --since '24h ago'` (Tier B: `-u verify-archive-tier-b.service`); the offending and expected hash; another archive's same-range hashes; recent commits to `cmd/stellarindex-ops/`, `internal/ops/archive/` and `internal/ledgerstream/`.

## Operator hygiene: /tmp/va-*.log cleanup

Manual long-range runs produce multi-GB stdout; the binary does not create the log, so clean up. Always go through `run-heavy-job.sh`:

```sh
LOGFILE=$(mktemp /tmp/va-XXXXXX.log)
trap 'gzip -9 "$LOGFILE" >/dev/null 2>&1; mv "${LOGFILE}.gz" /var/log/stellarindex/ 2>/dev/null || rm -f "$LOGFILE" "${LOGFILE}.gz"' EXIT
/usr/local/sbin/run-heavy-job.sh verify-archive \
  stellarindex-ops verify-archive -config /etc/stellarindex.toml -from "$FROM" -to "$TO" > "$LOGFILE" 2>&1
```

## Related

ADR-0016, ADR-0017 (DAT-09), `docs/operations/archival-node-bringup.md`, [archive-files-missing](archive.md#stellarindex_archive_files_missing).
