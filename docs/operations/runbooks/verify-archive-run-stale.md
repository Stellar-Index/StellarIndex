---
title: Runbook — verify-archive-run-stale
last_verified: 2026-08-28
status: ratified
severity: P2
---

# Runbook — `stellarindex_verify_archive_run_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_verify_archive_run_stale` |
| Severity | P2 (`severity: page`) |
| Detected by | `configs/prometheus/rules.r1/verify-archive.yml` (group `stellarindex.verify_archive`, `severity: page`, `for: 10m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/verify-archive.yml`. |
| Typical MTTR | 1 hour (diagnose + re-enable timer or kick off fresh run) |
| Impact | Cross-region trust degrades. R2/R3 trust R1 for chain-integrity (ADR-0016); the longer R1 goes without a clean nightly verify, the further from "byte-identical bytes everywhere" the fleet drifts. After 48h consider the degradation comms call for R2/R3 (see Mitigation — no config flag exists to flip). |

## Symptoms

- The `verify-archive-tier-a.timer` last-trigger timestamp is more
  than 36h ago (24h cadence + 12h cushion).
- Sustained for 10 minutes — rules out a node_exporter scrape blip.
- Likely accompanied by `stellarindex_verify_archive_unit_failed`
  on each preceding night — the ticket-level alert that should
  have caught this earlier.

> **NOTE (changed 2026-08-30):** the expr now measures the last CLEAN
> COMPLETION, via `stellarindex_verify_archive_last_success_unix` — a
> gauge the verify-archive binary writes to its node_exporter textfile
> and advances only on a clean exit.
>
> It previously measured the timer's last *trigger*, updated on every
> firing regardless of how the service exited, so "every run failed but
> the timer still fires" did NOT trip this page — the exact scenario the
> alert description names. `_unit_failed` (ticket) was the only signal
> for it. Both now catch it, from different directions.

## Quick diagnosis (≤ 5 min)

```sh
ssh root@136.243.90.96

# Is the timer enabled and active?
systemctl status verify-archive-tier-a.timer

# What's the last-trigger timestamp + the next scheduled run?
systemctl list-timers verify-archive-tier-a.timer --all

# Has the service been failing? Look across the recent runs.
journalctl -u verify-archive-tier-a.service --since "3 days ago" \
  | grep -E "Started|Finished|FAILED|Failed|exit-code"
```

Three branches:

| State | Cause | Action |
| ----- | ----- | ------ |
| `inactive (dead)` and last-trigger > 36h ago | Timer disabled (operator forgot after a maintenance window) | Re-enable: `systemctl enable --now verify-archive-tier-a.timer` |
| `active (waiting)` and last-trigger fresh, yet this alert fired | EXPECTED since 2026-08-30: the timer is firing but every run is FAILING, so the last-success gauge has not advanced. This is the case the alert was blind to before | `journalctl -u verify-archive-tier-a.service` for the failure; [`verify-archive-unit-failed.md`](verify-archive-unit-failed.md) should also be firing |
| `active (running)` for hours | Current run is hung — or is the (expected) ~13.8h full pass | Watch per-chunk progress in the journal; see the hung-run bullet below |

## Mitigation (≤ 15 min)

- [ ] **If the timer is disabled**, re-enable it:
  ```sh
  ssh root@136.243.90.96 systemctl enable --now verify-archive-tier-a.timer
  ```
  This is the entire fix for the most-common cause (a missed re-enable after maintenance). Verify the next-trigger timestamp is < 24h ahead.
- [ ] **If runs are failing**, follow the [`verify-archive-unit-failed.md`](verify-archive-unit-failed.md) runbook to bring a single manual run back to green. The timer fires automatically nightly; one clean run is enough to clear this alert.
- [ ] **If a run is hung**, capture per-chunk progress and decide whether to wait or kill:
  ```sh
  ssh root@136.243.90.96 journalctl -u verify-archive-tier-a.service -f
  ```
  Context first: r1's ansible-managed unit runs `Type=notify` with
  `WatchdogSec=1h` and `VERIFY_ARCHIVE_MAX_RUNTIME=0` (uncapped),
  walking incrementally with `-from-last-verified`,
  `-state-file /var/lib/stellarindex/verify-archive-state.json` and
  `-safety-overlap 5000`, under `run-heavy-job.sh`. Steady-state
  nightly runs are a short tip-window walk; only a first-ever or
  state-lost run takes the ~13.8h full pass. So: if per-chunk
  progress is steady, **wait** — `WatchdogSec` kills on 1h of
  *silence*, not on duration. If output is frozen, let the watchdog
  SIGTERM it (or stop it yourself) and investigate what it was stuck
  on. Do NOT raise `-max-runtime` — the uncapped setting is
  intentional (the Task #13 mid-pass-cap incident), and runtime isn't
  the limiter.
- [ ] **Communicate degradation**: while this alert is firing, R2/R3 trust in R1's verification anchor is degrading. Today the degradation call is a comms action (status page / on-call channel), not a config flip — no `reduced_redundancy` config value exists; `ReducedRedundancy` is a wire-envelope field with zero producers (the L4.10 per-region trust wiring is unbuilt). `TODO(ash): wire L4.10 or drop this step.`
- [ ] **Verification**: a clean run completes within 24h; the alert clears.

## Root cause analysis

The page-level alert means R1's nightly chain-integrity anchor has been broken for over a day. Postmortem questions:

1. **Why did the ticket-level `_unit_failed` alert not get actioned?** Was the ticket missed, or was the failure self-fixing for too long before this page fired? Adjust SLA if needed.
2. **Was there a coincident upstream issue?** Cross-reference public Stellar archive incidents.
3. **Did `archive-completeness` also drift?** A missing-file gap can starve verify-archive indefinitely; both timers should be in sync.

Gather:

- Three nights of `journalctl -u verify-archive-tier-a.service`
- Same window of `archive-completeness.service` logs
- Public Stellar status archive snapshots
- The full `last_verified` chain on relevant ADRs (we may have invalidated a documented invariant)

## Known false-positive patterns

- **Planned long maintenance window** that disabled the timer for > 36h. Post-maintenance, re-enable + accept one staleness alert. Not a real issue.
- **Clock skew on the Prometheus side** — `time()` reads slightly different from the unit-state `last-trigger` source. Unusual; investigate node_exporter health if it persists.

## Related

- `verify-archive-unit-failed.md` — the ticket-level alert that
  should have caught the underlying issue before this page fires.
- `verify-archive-tier-b.md` — the Tier B siblings
  (`verify-archive-tier-b.{service,timer}`, `_tier_b_*` alerts)
  cover the single-source-corruption mode Tier A is blind to.
- ADR-0016 — per-region trust model.
- ADR-0017 — archive completeness invariants.
- `docs/operations/archival-node-bringup.md` §"Per-region trust + verification model"

## Operator hygiene — `/tmp/va-*.log` cleanup (F-0008)

Manual `stellarindex-ops verify-archive` invocations against a long
range produce multi-GB stdout that operators typically capture with
shell redirection, e.g.:

```sh
stellarindex-ops verify-archive --from 2 --to 0 > /tmp/va-full.log 2>&1
```

(The flag floor is `--from 2` — ledger 1 has no predecessor.)

The captured logs survive the run. The 2026-05-26 audit found
~4 GB of orphaned `/tmp/va-*.log` files on r1 after a single
backfill-investigation pass. They aren't created by the binary
(so the binary can't `defer os.Remove`), but they're cleanup the
operator is in the best position to do.

**Recommended pattern when running ad-hoc verify-archive scans:**

```sh
LOGFILE=$(mktemp /tmp/va-XXXXXX.log)
trap 'gzip -9 "$LOGFILE" >/dev/null 2>&1; mv "${LOGFILE}.gz" /var/log/stellarindex/ 2>/dev/null || rm -f "$LOGFILE" "${LOGFILE}.gz"' EXIT
/usr/local/sbin/run-heavy-job.sh va-manual \
  stellarindex-ops verify-archive --from "$FROM" --to "$TO" > "$LOGFILE" 2>&1
```

(Manual long-range scans are heavy jobs — always go through
`/usr/local/sbin/run-heavy-job.sh` per the CLAUDE.md heavy-job rule.)

That way the log either lands under `/var/log/stellarindex/` (where
the rc.83-era logrotate config picks it up — F-0009 closure) or is
deleted on exit. The default behaviour (orphan in `/tmp`) is what
the audit flagged.

The scheduled `verify-archive-tier-a.service` does NOT need this —
its stdout goes to the systemd journal and is rotated by
`journald`'s own retention.

## Changelog

- 2026-08-30 — the expr no longer keys on the timer's last trigger. It
  reads `stellarindex_verify_archive_last_success_unix`, written by the
  verify-archive binary and advanced only on a clean exit, so a job that
  fails every night can no longer look fresh (wave-D ALERT-10). The
  Symptoms NOTE and state-table branch 2 are rewritten accordingly —
  branch 2 is now a REACHABLE, expected state rather than a
  "shouldn't happen". Requires the binary carrying the new gauge to be
  deployed before the page is trustworthy; until then the series is
  absent and the alert cannot fire.

- 2026-08-28 — re-verified against HEAD. Unreachable state-table
  branch 2 replaced — the expr keys on the timer's last TRIGGER
  (updated every firing regardless of service exit), so
  every-run-failing keeps this alert silent; NOTE added under
  Symptoms. Hung-run guidance rewritten for r1's actual unit
  (Type=notify + WatchdogSec=1h, VERIFY_ARCHIVE_MAX_RUNTIME=0
  uncapped, incremental `-from-last-verified` state-file walk under
  run-heavy-job.sh) — "increase -max-runtime" advice removed.
  `reduced_redundancy` config flip replaced with the comms action +
  TODO(ash) (L4.10 unwired). Hygiene examples: `--from 0` → `--from 2`
  (flag floor) and wrapped in run-heavy-job.sh. Rule citation →
  `rules.r1/verify-archive.yml`; commands use r1 shapes; Tier B
  siblings added to Related.
- 2026-04-29 — initial draft alongside the L4.12 systemd-timer ship.
- 2026-05-28 — added "Operator hygiene — /tmp/va-*.log cleanup"
  section (F-0008 closure).
