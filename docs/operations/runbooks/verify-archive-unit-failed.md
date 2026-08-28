---
title: Runbook — verify-archive-unit-failed
last_verified: 2026-08-28
status: ratified
severity: P3
---

# Runbook — `stellarindex_verify_archive_unit_failed`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_verify_archive_unit_failed` |
| Severity | P3 (`severity: ticket` — no immediate customer impact) |
| Detected by | `configs/prometheus/rules.r1/verify-archive.yml` (group `stellarindex.verify_archive`, `severity: ticket`, `for: 5m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/verify-archive.yml`. |
| Typical MTTR | 30 min (diagnosis) – several hours (re-run) |
| Impact | None immediate. The API still serves correct data from existing bytes. Cross-region trust property degrades if multiple consecutive nights fail (see the `_run_stale` page-level alert). |

## Symptoms

- The most-recent run of `verify-archive-tier-a.service` (R1's nightly
  chain-link integrity check) exited in `failed` state.
- Sustained for 5 minutes — long enough to rule out transient
  systemd state during a manual restart.

## Quick diagnosis (≤ 5 min)

```sh
ssh root@136.243.90.96

# What state is the unit in?
systemctl status verify-archive-tier-a.service

# Why did the last run fail? Logs land in journald.
journalctl -u verify-archive-tier-a.service --since "yesterday" --no-pager | tail -50

# Has the daily archive-completeness run also been failing? A
# missing-file gap is the most-common cause of a verify-archive
# failure — both alerts will fire together.
journalctl -u archive-completeness.service --since "yesterday" --no-pager | tail -20
```

The journal output's last lines indicate the failure mode:

| Pattern | Cause |
| ------- | ----- |
| `chain break at ledger L` | Real corruption — escalate (see RCA). Emitted by `verify_archive_chunks.go:223-228`. Tier B's analogue is `checkpoint anchor mismatch at ledger N`. |
| `is missing` (the datastore's missing-object error) | Missing file in galexie-archive; should also fire `stellarindex_archive_files_missing`. Trailing-edge misses (files galexie hasn't written yet) are tolerated. |
| `WATCHDOG` timeout / `SIGTERM` | 1h of *silence* tripped `WatchdogSec` (r1 runs uncapped — `VERIFY_ARCHIVE_MAX_RUNTIME=0`; binary default 24h); investigate what the walk was stuck on |
| `access denied` / `403` | AWS / MinIO credentials in `/etc/default/stellarindex-ops` rotated or wrong |

`TODO(ash): the deploy/systemd unit copy says a 16h max-runtime — a
third value; ansible is the authority (deploy/systemd drift class,
cf. PR #253).`

## Mitigation (≤ 15 min)

- [ ] **Re-run manually with verbose logging** to confirm the failure mode is reproducible and not a transient blip:
  ```sh
  ssh root@136.243.90.96
  set -a; source /etc/default/stellarindex-ops; set +a
  /usr/local/sbin/run-heavy-job.sh va-manual \
    /usr/local/bin/stellarindex-ops verify-archive \
    -config /etc/stellarindex.toml \
    -from 2 \
    -tier chain \
    -workers 8 \
    -max-runtime 1h
  # To reproduce the scheduled unit's incremental window instead, add:
  #   -state-file /var/lib/stellarindex/verify-archive-state.json -from-last-verified
  ```
  A 1h budget on an 8-worker run gets enough coverage to confirm
  whether the original failure persists. Don't push the full
  run yet — the next scheduled timer will retry.
- [ ] **If the failure mode is `is missing`**: trigger a manual `archive-completeness fix` to backfill the missing file from the public-archive fallback chain (note: the subcommand has NO `-config` flag and `-to` is REQUIRED):
  ```sh
  ssh root@136.243.90.96
  # head = current network tip ledger (e.g. from /v1/diagnostics/cursors)
  /usr/local/sbin/run-heavy-job.sh archive-completeness-fix \
    /usr/local/bin/stellarindex-ops archive-completeness fix \
    -from 2 -to <head-ledger>
  # or simply re-fire the scheduled path (check → fix → re-check + textfile):
  systemctl start archive-completeness.service
  ```
  Re-run verify-archive afterwards.
- [ ] **If the failure mode is `chain break` (or a Tier B `checkpoint anchor mismatch`)**: STOP. This is the corruption-detection signal the system exists to surface — do NOT auto-recover. Escalate per RCA.
- [ ] **Verification**: the next scheduled `verify-archive-tier-a.service` run completes cleanly. Watch the unit state via Prometheus or `systemctl is-active`.

## Root cause analysis

**Chain-link mismatch is the heavy case.** The chain-integrity check is what makes our archive trustworthy as a cross-region anchor; a real mismatch means either:

1. **Upstream archive corruption** — Stellar's published history archive disagrees with what we mirrored. Rare but real (see incident NNN if logged). Action: pull the offending ledger from a different upstream archive (SDF / Lobstr / SatoshiPay) and compare.
2. **Mirror corruption during transit** — the bytes we hold differ from what the upstream serves. Action: re-fetch the offending range; confirm the new copy verifies.
3. **Decoder bug** — verify-archive itself is misreading. Action: check git log for recent decoder changes; reproduce against the same range under the previous binary.

Gather for the postmortem:

- Full journal: `journalctl -u verify-archive-tier-a.service --since '24h ago'`
- The reported chain-mismatch ledger pair — the offending hash + the expected hash
- Comparison against another archive's same-range hashes
- Recent commits to `cmd/stellarindex-ops/`, `internal/ops/archive/`
  (where the verify code lives) and `internal/ledgerstream/`

## Known false-positive patterns

- **Manual run started during scheduled run** — both runs race; one fails on a lock or duplicate work. Fix: don't start manual runs while the timer is active.
- **MinIO restart mid-run** — connection-reset surfaces as a chain-walk failure. The next scheduled run completes cleanly. Don't escalate unless the failure repeats two nights in a row.

## Related

- The page-level alert `stellarindex_verify_archive_run_stale` —
  fires when the unit hasn't completed cleanly in 36h+, indicating
  this ticket-level alert wasn't actioned in time.
- ADR-0016 — per-region trust model that this nightly run anchors.
- `docs/operations/archival-node-bringup.md` §"Per-region trust + verification model"
- The `archive-files-missing.md` runbook — adjacent failure mode that
  often co-fires with this alert.

## Changelog

- 2026-08-28 — re-verified against HEAD. `archive-completeness fix`
  example didn't parse (no `-config` flag; `-to` is required) —
  replaced with the working invocation + the
  `systemctl start archive-completeness.service` shortcut. Dead
  "8h cap" row replaced with the WatchdogSec=1h silence-timeout
  reality (r1 runs uncapped, VERIFY_ARCHIVE_MAX_RUNTIME=0;
  TODO(ash) on the deploy/systemd 16h third value). Grep patterns
  updated to the real messages (`chain break` /
  `is missing` / `checkpoint anchor mismatch`);
  `archive_files_missing` → full name
  `stellarindex_archive_files_missing`. Manual re-run wrapped in
  run-heavy-job.sh with the incremental state-file option noted.
  Rule citation → `rules.r1/verify-archive.yml`; commands use r1
  shapes; `internal/ops/archive/` added to RCA pointers.
- 2026-04-29 — initial draft alongside the L4.12 systemd-timer ship.
