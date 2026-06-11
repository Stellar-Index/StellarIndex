---
title: Runbook — archive-completeness-stale
last_verified: 2026-06-12
status: draft
severity: P2
---

# Runbook — `ratesengine_archive_completeness_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `ratesengine_archive_completeness_stale` (P2 at 26 h) / `_critical_stale` (P1 at 48 h on R1) |
| Severity | P2 / P1 |
| Detected by | Prometheus rule in `deploy/monitoring/rules/archive-completeness.yml` |
| Typical MTTR | 5 min if the cron silently failed; 1 h if the daemon itself is broken |
| Impact | Same as `archive-files-missing` once it's been long enough — `flags.reduced_redundancy` set on API responses, status page degrades. P1 variant fires only on R1 because R1 is the integrity leader for the fleet (per ADR-0017). |

## Symptoms

- `time() - archive_completeness_last_success_timestamp` > 26 h on any region (P2) or > 48 h on R1 (P1).
- `archive_completeness_runs_total` may or may not have incremented during the staleness window. If it has, runs are happening but failing; if it hasn't, the timer/service itself isn't firing.

## Quick diagnosis (≤ 5 min)

```sh
# 1. Is the timer scheduled?
ssh r1 'systemctl list-timers archive-completeness.timer'

# 2. Is the service unit healthy? Look at the last few invocations.
ssh r1 'systemctl status archive-completeness.service'
ssh r1 'journalctl -u archive-completeness.service --since="48 hours ago" -p err'

# 3. Did the last run exit non-zero? Why?
ssh r1 'journalctl -u archive-completeness.service --since="48 hours ago" | tail -50'

# 4. Is the binary itself working? Run the real verify subcommand
#    over a short trailing window (-to 0 resolves the tip from the
#    live ledgerstream cursor). See `deploy/systemd/archive-completeness.service`
#    for the exact flags the timer uses.
ssh r1 'ratesengine-ops archive-completeness verify -from 2 -to 0 -workers 8'
```

Common patterns:

- **Timer dropped on reboot** — `Persistent=true` should re-fire on next boot, but a hardware fault or systemd misconfig can break that. Re-enable.
- **Daemon crash mid-run** — usually a panic in repair-fetch when an upstream returns malformed data. Check the journalctl tail for the crash; the next run will resume.
- **Disk full** — daemon writes a JSON gap report to `/var/lib/galexie/`; if disk is full it can't and exits non-zero. Free space, re-run.
- **Stuck on a single file** — a particular file fails on every fallback source. The daemon should give up after the chain exhausts, but a bug could deadlock it. `kill -9` and re-run.

## Mitigation (≤ 15 min)

- [ ] **Step 1 — If the timer isn't scheduled, re-enable it.**

  ```sh
  ssh r1 'systemctl enable --now archive-completeness.timer'
  ```

- [ ] **Step 2 — Run the daemon manually and watch it complete.**

  ```sh
  ssh r1 'systemctl start archive-completeness.service'
  ssh r1 'journalctl -u archive-completeness.service -f'
  ```

- [ ] **Step 3 — If step 2 fails, run the verify subcommand manually and bisect the ledger range.**

  `archive-completeness verify` performs a single cross-anchor
  structural completeness check (there are no separate `-checks`
  modes — see `cmd/ratesengine-ops/main.go::archiveCompletenessVerify`).
  Write the JSON report to inspect which checkpoints are missing,
  then narrow the `-from`/`-to` window around the failing region:

  ```sh
  # Full trailing window, capturing the JSON gap report.
  ssh r1 'ratesengine-ops archive-completeness verify \
    -from 2 -to 0 -workers 8 \
    -output-file /tmp/completeness-report.json'

  # Inspect the missing-file list, then re-run scoped to a narrow
  # range to confirm a fix (replace LO/HI from the report).
  ssh r1 'jq ".missing" /tmp/completeness-report.json | head'
  ssh r1 'ratesengine-ops archive-completeness verify -from LO -to HI -workers 8'
  ```

  From the missing-checkpoint list, see [archive-completeness.md](../archive-completeness.md)
  for the bootstrap-procedure step that addresses the gap.

- [ ] **Verification:** after a successful manual run, `archive_completeness_last_success_timestamp` updates to "now" and the alert clears within the next eval cycle (default 1 min).

## Root cause analysis

- Capture `journalctl -u archive-completeness.service --since="7 days ago"` — was this a one-time miss or a degrading pattern?
- If timer dropped on reboot, capture `last reboot` history and the systemd journal around the boot to confirm `Persistent=true` did its job (or didn't).
- For repeated failures of the same check: capture the gap report JSON across the failed runs and diff them to see whether the gap is stable or growing.

## Known false-positive patterns

- **R2 / R3 alerted but R1 is fine.** R2/R3 scrape R1's metrics endpoint to compute `last_success_timestamp` for cross-anchor checks. If the cross-region metrics scrape itself is broken (firewall, DNS, etc.) R2/R3 see a stale timestamp even though R1 is healthy. Verify R1's local timestamp is fresh; if so this is a federation problem, not a completeness problem — escalate to the metrics-federation runbook.
- **Daemon ran but killed by oom-killer.** The fallback fetcher buffers responses in memory; on a malformed-response path it can occasionally use more than expected. Check `dmesg | grep oom` near the failure time. Mitigation: bump the systemd unit's `MemoryMax=` and re-run.

## Related

- [ADR-0017](../../adr/0017-archive-completeness-invariants.md) — invariants this alert protects.
- [archive-files-missing](archive-files-missing.md) — companion runbook for when the daemon ran cleanly but couldn't fill some files.
- [archive-completeness.md](../archive-completeness.md) — operational overview.
- Postmortems tagged `archive-completeness-stale` — `docs/operations/postmortems/`.

## Changelog

- 2026-06-12 — F-1330: replace fictional `archive-completeness check
  -range … -checks …` invocations with the real
  `archive-completeness verify -from … -to … -workers …` subcommand
  (no `-checks` modes exist; it is a single cross-anchor check).
- 2026-04-27 — initial draft alongside ADR-0017.
