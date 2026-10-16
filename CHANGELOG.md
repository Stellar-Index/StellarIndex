# Changelog

All notable changes to Stellar Index will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to SemVer (`vX.Y.Z`) — for binary releases
as well as the `pkg/*` compatibility promise. See
[docs/architecture/semver-policy.md](docs/architecture/semver-policy.md)
for the rationale.

Every release lists the Stellar protocol version it was tested
against.

Only the five most recent releases are kept here; older sections
live at their tags (`git show v0.89.0:CHANGELOG.md`) and on GitHub
Releases. `[Unreleased]` is written at the release cut from commit
subjects, not per PR — see CONTRIBUTING.md §Changelog.

---

## [Unreleased]

## [v0.116.10] — 2026-10-16

2 commits since v0.116.9. No migrations. No `pkg/*` break.

### Fixed

- **redstone:** `earnUSDC_FUNDAMENTAL/USD` is registered (vault contract in USD) instead of landing as a `raw:` row, and the bare `earnUSDC_FUNDAMENTAL` feed is quoted in USDC, the reserve it is published in (#3552). Existing rows from ledger 64254176 are corrected by a `projector-replay`.
- **alerts:** the daily-publisher silence rule no longer fires when restarts re-emit the same fix and flatten the events counter (#3551).

## [v0.116.9] — 2026-10-16

1 commit since v0.116.8. No migrations. No `pkg/*` break.

### Fixed

- **completeness:** the weekly SDEX census streams three 25k-ledger windows at a time, so a full served-range pass fits inside the unit's 6h timeout instead of being killed and discarded (#3549).

## [v0.116.8] — 2026-10-16

1 commit since v0.116.7. No migrations. No `pkg/*` break.

### Fixed

- **indexer:** every enabled source's event counter starts at 0, so the first event after a restart is counted by `rate()`; a lazily created counter hid it and the 6h–30h `source_stopped`/low-volume windows reported a live source as silent (#3545).

## [v0.116.7] — 2026-10-16

3 commits since v0.116.6. No migrations. No `pkg/*` break.

### Fixed

- **spectra:** the decoder recognises the PT maturity events `yield_claimed` (restates `yield_updated` and the IBT transfer) and `rates_stored_at_expiry` as zero-row kinds, so completeness recognition stops failing spectra from the first maturity at ledger 64,972,231 (#3542).
- **restore-drill:** the restore runs with its own scratch `--spool-path`; pgbackrest restore wipes its spool's archive dir, which deleted the live cluster's archive-push acknowledgements and stalled WAL archiving for 45 min (#3543).
- **ops:** a scheduled heavy job whose lock wait runs out behind another scheduled job defers (exit 75) instead of failing its unit (#3541).

## [v0.116.6] — 2026-10-16

1 commit since v0.116.5. No migrations. No `pkg/*` break.

### Fixed

- **exchangeratesapi:** the poller runs hourly, the plan's refresh, instead of every 60 s, which overran the plan's 10,000-call monthly quota; it gets its own 2 h stale alert, as tiingo has (#3538).
