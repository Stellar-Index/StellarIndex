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

## [v0.116.5] — 2026-10-16

3 commits since v0.116.4. One migration (0216, additive). No `pkg/*` break.

### Fixed

- **chainlink-stellar:** the decoder recognises the cache contract's admin events (StaleReport, FeedConfigSet, FeedAdminRemoved, Upgraded, ownership transfers) as no-ops, so completeness recognition stops reporting "unhandled topic" from genesis; migration 0216 admits `chainlink-stellar` in `divergence_observations_reference_check`, which rejected every observation with 23514 (#3536).
- **ansible:** postgres_exporter drops its per-table collectors; the scrape fell from 375k series and 4–11 s to under 1 s (#3534).
- **test:** the exporter bind postcondition test matches an ARGS bind followed by more flags (#3535).

## [v0.116.4] — 2026-10-16

1 commit since v0.116.3. No migrations. No `pkg/*` break.

### Fixed

- **aggregator:** the composite reference logs its `unavailable` cause at Info whenever it changes per pair and window, so `stellarindex_aggregator_composite_corroboration_unavailable` can be traced without a phase-2 freeze (#3531).

## [v0.116.3] — 2026-10-16

2 commits since v0.116.2. No migrations. No `pkg/*` break.

### Fixed

- **storage:** an unpriced trade where neither leg has any peg or XLM market is labelled `unroutable` instead of `no`, so cross-issuer junk pairs no longer drag the on-chain USD coverage ratio. XLM, the USD pegs and aliased assets are never labelled this way, and neither is anything while XLM/USD is unresolved, so a pricing outage still trips the alert (#3529).
- **ops:** `ch-holders-rollup` reports progress after each completed step, so a long rollup no longer trips `stellarindex_ops_job_no_progress` (#3528).

## [v0.116.2] — 2026-10-16

3 commits since v0.116.1. No migrations. No `pkg/*` break.

### Changed

- **storage:** the XLM/USD anchor reads its pair's freeze events once per query instead of once per minute row; output is unchanged and the 7-day sparkline query drops from ~210 ms to ~85 ms (#3525).
- **alerts:** `stellarindex_ingestion_source_insert_stale` judges projected sources on its 24h arm, since their freshness is the projector cursor; dispatcher-written sources keep the 1h budget (#3526).

### Fixed

- **indexer:** the hourly hashdb verify sweep reads with a 256-ledger prefetch buffer instead of the SDK's 10,000, which held ~5 GB and pushed the indexer into swap (#3524).

## [v0.116.1] — 2026-10-16

4 commits since v0.116.0. No migrations. No `pkg/*` break.

### Changed

- **api:** `/v1/assets` sparklines are cached per asset and `/v1/rwa/assets` caches its assembled view, so a cold listing page no longer recomputes every sparkline (#3520).
- **explorer:** the static export builds on one worker, bounding its concurrent API load (#3521).

### Fixed

- **explorer:** the asset header's SAC link renders on statically built pages (#3519).
- **ops:** the restore drill waits up to 6h for the nightly pgbackrest backup to finish before choosing which backup to restore, instead of restoring the previous night's and overrunning the WAL drain (#3522).
