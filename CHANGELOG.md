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

## [v0.116.12] — 2026-10-17

2 commits since v0.116.11. No migrations. No `pkg/*` break.

### Fixed

- **explorer:** the asset and pair tip streams open only after the price poll confirms a fresh, non-thin price, so thin or stale markets no longer trigger refused (404) stream requests (#3556, #3557).
- **explorer:** exchange, aggregator and protocol links render only when the target page exists; the embed widget makes no request without an asset; SDEX no longer shows a false "Lake analytics unavailable"; RWA basis prose no longer says "priced by" for an admission-only oracle feed (#3557).

## [v0.116.11] — 2026-10-17

1 commit since v0.116.10. No migrations. No `pkg/*` break.

### Changed

- **wasmaudit:** the redstone adapter's current WASM (`f0ccdffc…`, live since ledger 62520450) is audited and in the replay-gate manifest, so redstone replays from that ledger are allowed (#3554).

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
