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

## [v0.116.40] - 2026-10-21

No migrations. No `pkg/*` break. No config change.

### Fixed

- **timescale:** the USD-volume pricing endpoint folds SAC legs to their classic form before its same-issuer test, matching the trade labeller (#3730).

### Changed

- **ci:** the config-apply gate's refusal lists the substantive surfaces it found (#3732).
- **docs:** the API runbook says a restart that clears `ch_schema_probe_absent` is not a fix (#3731).

## [v0.116.39] - 2026-10-20

No migrations. No `pkg/*` break. No config change; the alert-rule edits are comments only.

### Fixed

- **timescale:** unpriced trades between the declared SAC wrappers of one issuer's credits are labelled `unroutable`, like the same trades between the classic forms; a weekly wash burst on three such wrappers read as an on-chain USD-coverage gap (#3728).
- **alerts:** the textfile-stale catch-all excludes `ops_job_tag_tx_index.prom`, which has its own staleness rule (#3726).

### Changed

- **release:** the cut-release hint names the hyphenated heading the changelog uses (#3725).
- **docs:** the outlier trim-fraction runbook names the thin-pair trend case (#3724).

## [v0.116.38] - 2026-10-20

No migrations. No `pkg/*` break. No config change. After the deploy, watch the latency SLO burn during the next crawler burst.

### Fixed

- **api:** asset detail caches the lake token-supply sum per contract for 5 minutes and serves the last good value on a ClickHouse error; under a crawl, the uncached `supply_flows FINAL` sum dominated ClickHouse serving load (#3721).
- **alerts:** `dust_dropped_high` fires on the share of a venue's trades dropped as dust, not the absolute rate, so a volume burst no longer pages (#3720).

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
