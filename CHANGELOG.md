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

