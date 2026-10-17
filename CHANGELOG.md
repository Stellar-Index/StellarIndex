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

## [v0.116.19] - 2026-10-17

Migrations 0217 and 0218, both additive. No `pkg/*` break.

### Fixed

- **aggregate:** a liquid pair's window VWAP no longer mixes legs that cover different spans. `TradesInRange` caps each leg at its newest 10,000 rows (XLM/USD 24h was 83 bps off). A capped leg now fills the rest of its window from the new `trade_sums_1m` continuous aggregate (migration 0217), and the local outlier filter weighs trades, not rows (#3575).
- **supply:** classic supply freshness is anchored on a processed-ledger watermark (migration 0218) instead of the oldest of four component tables. A quiet claimable-balance table no longer makes the supply gate reject current classic snapshots as dormant (#3576).
- **ops:** galexie resumes from the newest partition instead of a full recursive bucket listing, and fails loudly when the listing fails. Job locks created by root are chowned to the service user (#3578).
- **ops:** `ch-live-catchup` is installed on every ClickHouse host, r1 included, so it no longer runs a stale copy as the `default` user (#3579).

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
