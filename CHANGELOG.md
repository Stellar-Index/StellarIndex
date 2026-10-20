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

## [v0.116.37] - 2026-10-20

No migrations. No `pkg/*` break. No config change.

### Fixed

- **pricingguard:** the scam gate's flagged-SAC index (about 180k rows) is rebuilt in the background once it expires, serving the previous index meanwhile, and is built at API startup; previously the request that found it expired waited about 220 ms for the rebuild, which drove the price-latency SLO burn after every deploy (#3713).
- **ops:** `ch-rebuild` reads SDEX ops in 25k-ledger windows on the full-range and gap paths too; a one-query read of 1M ledgers hit ClickHouse's 10 GiB query cap (#3716).

## [v0.116.36] - 2026-10-20

No migrations. No `pkg/*` break. No config change.

### Added

- **ops:** `ch-rebuild -credit-refusals` (dry run only) charges each decoded trade to the USD credibility budget in `-write`'s batch order and prints how many token/token prints it would refuse and their USD total (#3710)

### Fixed

- **timescale:** asset lookup by slug or id spells `COALESCE(slug, code)` out per column, so each arm reads its index instead of a parallel scan of `classic_assets` (62-75 ms to 1-14 ms on r1) (#3707).
- **timescale:** the sparklines' XLM/USD leg is read once per 30 s and shared across assets, re-read at once when `freeze_events` changes (7d history 107 ms to about 1 ms per cold asset page on r1) (#3708).

## [v0.116.35] - 2026-10-20

Postgres migration 0222 (new tables and a trigger; no replay). No `pkg/*` break. No config change.

### Fixed

- **timescale:** the pools raw prefilter writes only the asset and source predicates for filters that are set, so a cached generic plan uses the `pools_per_source_1h` asset indexes (228 ms to 83 ms on r1) (#3703).
- **timescale:** the SEP-41 supply fold is bounded below by its checkpoint's close time, so planning prunes `sep41_supply_events` chunks (88 ms to 0.5 ms planning); the folded set is unchanged (#3703).
- **timescale:** SAC balance sums read the trigger-kept `sac_balance_current` table (migration 0222) instead of a full-history DISTINCT ON (3.5 s per call on r1) (#3704).

## [v0.116.34] - 2026-10-20

No migrations. No `pkg/*` break. No config change. After the deploy, watch `go_sql_wait_count_total{db_name="serving"}` and the API's new-session rate in `pg_stat_database` during the next explorer build.

### Fixed

- **timescale:** every Postgres pool keeps idle connections up to its open cap, so a burst no longer closes and reopens connections whose fresh backends re-plan hypertable chunks cold (r1 was opening 493 sessions a minute) (#3701).

### Added

- **api:** the serving pool exports `database/sql` stats as `go_sql_*{db_name="serving"}` (#3699).
