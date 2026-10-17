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

## [v0.116.18] - 2026-10-17

### Fixed

- `/v1/price` for a pair with no recent 1-minute bucket bounds its last-trade fallback to the 400-day lookback the primary path uses; the unbounded walk planned every `trades` chunk and took 0.5-1.8 s per miss. A pair idle longer than 400 days now answers not-found rather than withheld (#3571).
- **ops:** `ch-backfill` reads archive ranges trimmed to the cold tier through to it, so a re-derive over them needs no rehydrate first (#3570).

### Changed

- **alerts:** comet is no longer on the low-volume DEX silence rule; its pools can sit dormant for days, and projector lag and completeness already watch it (#3569).

## [v0.116.17] - 2026-10-17

### Fixed

- `stellarindex_trade_inserts_total` no longer counts a trade refused by the thin-market gate as `usd_volume_populated="no"` when its rate came from a cached miss that outlived the hourly substance verdict; bursts on thin pairs had pulled the on-chain USD-volume coverage ratio under its alert threshold (#3567).

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

