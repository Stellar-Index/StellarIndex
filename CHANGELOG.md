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

## [v0.116.0] — 2026-10-15

4 commits since v0.115.1. No migrations. No `pkg/*` break.

### Added

- **explorer:** the asset header shows the asset's identity and its SAC address with copy buttons; a classic asset's SAC page redirects to the classic page, and search resolves a SAC address or a bare issuer to its asset (#3514).

### Changed

- **api:** `/v1/assets?asset_class=all` lists a SAC-wrapped asset once, folding the SAC row into its classic row and counting its 24h volume exactly once; volume that cannot be read or confirmed sets `volume_lower_bound` (#3514).
- **ops:** ch-lake-backup waits up to 20h for the gap between heavy chunks instead of deferring whenever a heavy job holds the lock (#3516).

### Fixed

- **api:** `/v1/oracle/latest` no longer serves the retired Ethereum chainlink feed's last price (#3517).
- **ansible:** the host keeps a zfs module for every kernel it can boot, so a reboot onto a new kernel finds its pool (#3515).

## [v0.115.1] — 2026-10-15

1 commit since v0.115.0. No migrations. No `pkg/*` break.

### Added

- **monitoring:** `stellarindex_usd_volume_xlm_bound_skipped` tickets when verify-usd-volume could not judge a day's XLM-base/quote bounds; the run publishes `stellarindex_verify_usd_volume_xlm_bound_judged` (#3512).

### Fixed

- **ops:** verify-usd-volume builds the day's XLM/USD rate from `prices_1h`, so the 05:12 UTC run judges the XLM bounds instead of skipping them while `prices_1d` is still unmaterialized (#3512).
- **monitoring:** `stellarindex_usd_volume_estimated_tier_high` alerts on the estimated tier's share of valued volume (> 5%) instead of a $10M bar it crossed every day (#3512).

## [v0.115.0] — 2026-10-15

6 commits since v0.114.0. No migrations. No `pkg/*` break. Removes the `[chainlink]` config section (minor, pre-v1).

### Operator actions

- **ingest:** after deploy, run `stellarindex-ops projector-replay -source chainlink-stellar -from 64442968` to fill the new source from its first on-chain round (#3507).
- **config:** the `[chainlink]` Ethereum section and `CHAINLINK_RPC_URL` are no longer read; a leftover key warns at boot instead of failing (#3508).

### Added

- **sources:** `chainlink-stellar` ingests Chainlink Data Feeds published on Stellar, gated on the feed contracts' identity; it is the Chainlink divergence reference (#3507).

### Removed

- **sources:** the Ethereum Chainlink poller, its metrics, alerts and runbook sections (#3508).
- **explorer:** the blog (#3505).

### Fixed

- **explorer:** a route switch no longer crashes a page with `Cannot read properties of undefined`; one React Query key now holds one response shape (#3504).
- **explorer:** link hovers no longer log 404s for the `__PAGE__` prefetch files (#3509).
- **explorer:** status, methodology and service-level pages use the standard page shell; the footer matches the content width; the network switcher stays on screen; the sidebar search box is darker (#3505, #3506).
