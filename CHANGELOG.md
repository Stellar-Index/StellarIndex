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

## [v0.116.16] - 2026-10-17

### Changed

- `stellarindex_trade_inserts_total` labels a live trade left unpriced by a failed USD resolver read `usd_volume_populated="error"` instead of `"no"`; it still counts against on-chain USD-volume coverage (#3565).

## [v0.116.15] - 2026-10-17

### Fixed

- `/v1/ohlc`, `/v1/vwap` and `/v1/twap` no longer return 422 all-filtered when a tight cluster of dust prints shrinks the outlier band below an ordinary hourly move; the band is floored at ±1% of the median, matching the aggregator's local filter (#3563).

## [v0.116.14] — 2026-10-17

1 commit since v0.116.13. No migrations. No `pkg/*` break.

### Fixed

- **divergence:** the synthetic USD cross gains the exchangeratesapi FX leg, so the reflector-cex base leg pairs with a non-Reflector rate; BTC/EUR and BTC/GBP no longer drop below quorum when the sparse redstone and chainlink legs go stale. The refresh reads the cached VWAP across asset aliases, so the native/fiat:* pairs refresh instead of serving a frozen row (#3561).

## [v0.116.13] — 2026-10-17

1 commit since v0.116.12. No migrations. No `pkg/*` break.

### Fixed

- **ops:** `projector-replay -refresh-only` refreshes the price CAGGs outside the 30 s setup budget; every view was cancelled 30 s in, before any window refreshed (#3559).

## [v0.116.12] — 2026-10-17

2 commits since v0.116.11. No migrations. No `pkg/*` break.

### Fixed

- **explorer:** the asset and pair tip streams open only after the price poll confirms a fresh, non-thin price, so thin or stale markets no longer trigger refused (404) stream requests (#3556, #3557).
- **explorer:** exchange, aggregator and protocol links render only when the target page exists; the embed widget makes no request without an asset; SDEX no longer shows a false "Lake analytics unavailable"; RWA basis prose no longer says "priced by" for an admission-only oracle feed (#3557).

