# W34 — Verify-archive Type=notify lifecycle

## Scope

The verify-archive operator workflow that ensures every ledger
in `[from, current_tip]` is hash-chain-verified:

- `cmd/ratesengine-ops/main.go::verifyArchive`
- `cmd/ratesengine-ops/verify_archive_chunks.go`
- `cmd/ratesengine-ops/verify_archive_state.go`
- `deploy/systemd/verify-archive-tier-a.service` +
  `.timer`
- Type=notify lifecycle + WatchdogSec
- state-file persistence semantics
- per-chunk resume across restarts
- the 2026-05-25 trailing-edge bug
  (`project_62_diagnosis_2026_05_25` memory) + its rc.81 fix
  (TolerateTrailingMissing in walker config)
- ADR-0017 tier A/B/C/D invariants

## Inputs

- service unit + timer unit (`systemctl cat`)
- `cmd/ratesengine-ops/verify_archive_*.go`
- ADR-0017
- `docs/operations/archive-completeness.md`
- memory `project_62_diagnosis_2026_05_25`
- the state file at `/var/lib/ratesengine/verify-archive-state.json`

## Checks

| # | Check | Method |
| --- | --- | --- |
| W34.1 | systemd unit uses `Type=notify` (not the wall-clock `TimeoutStartSec`) | unit file |
| W34.2 | `WatchdogSec=1h` (or similar) controls liveness; the binary signals READY=1 + WATCHDOG=1 periodically | unit file + code |
| W34.3 | State file shape: `tiers.<tier>.{last_verified_ledger, last_verified_at, in_progress: { from, to, workers, started_at, updated_at, chunks: [...] }}` | actual JSON |
| W34.4 | `-from-last-verified` reads the state file and resumes from `last_verified_ledger + 1 - safety_overlap` | code |
| W34.5 | Per-chunk resume: in_progress.chunks[i].done = true is honoured across restarts (the current run skips done chunks) | rc.75 / rc.76 commits |
| W34.6 | `-resume-from-hash` validates the prior boundary hash before proceeding (no silent rebase) | code |
| W34.7 | rc.81 commit `d3b4d492` sets `TolerateTrailingMissing=true` in the walker's lsCfg | grep |
| W34.8 | On trailing-edge missing-file: walker treats as walk-complete (WARN log only, no error) | code + tests |
| W34.9 | Tier A = chain-link integrity (hash chain ledger N → ledger N+1) | code |
| W34.10 | The bootstrap (first-ever) run is a full pass; subsequent runs are incremental | code path |
| W34.11 | `CPUWeight=20`, `IOWeight=20` yield to API + postgres during daytime | unit file |
| W34.12 | Prometheus textfile metric `verify_archive_*` exists + scraped | metric path |
| W34.13 | Alert exists for verify-archive-stale (didn't fire in 24h) | rule file |
| W34.14 | Alert exists for verify-archive-error (last run failed) | rule file |
| W34.15 | Runbook documents bootstrap recovery (state file lost / corrupted) | runbook |
| W34.16 | LIVE: timer is currently enabled on r1 (post-rc.81) | r1 probe |
| W34.17 | LIVE: most recent run did NOT trip on trailing-edge | journal grep |
| W34.18 | The "in_progress" record honours the prior `to` (pinned tip per rc.75 commit `e4fe66d7`) so per-chunk resume actually works | code |
| W34.19 | Hardening sandbox flags (`ProtectSystem=full`, `PrivateTmp=true`, `NoNewPrivileges=true`, etc.) match the documented minimum | unit file |
| W34.20 | NEW: per the 2026-05-25 incident — bootstrap walked 62.64M ledgers clean, FAILED on trailing-edge — the rc.81 fix is verified in-place via R1 probe (W28.7-13) | cross-ref |

## Closure criteria

Every check terminal. Findings on:

- any state-file corruption path (the file is the only resume
  state; any way to corrupt it without detection is critical)
- any case where Type=notify times out before the binary
  signals READY=1 (would mask boot success)
- any tier (A/B/C/D) without a corresponding alert + runbook
