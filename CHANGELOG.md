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

## [v0.116.42] - 2026-10-21

No migrations. No `pkg/*` break. No config change.

### Fixed

- **api:** `/v1/price`'s last-trade fallback for a dormant pair (no closed 1m bucket in 14 days, no trade in 7) runs its 400-day walk on a 4-connection pool warmed at startup, instead of on whichever fresh serving connection a burst opened. After the v0.116.41 restart six such reads each spent ~1s planning ~58 chunks per arm at once and re-fired the latency burn. Served values are unchanged (#3737).

## [v0.116.41] - 2026-10-21

No migrations. No `pkg/*` break. No config change.

### Fixed

- **api:** the `change_24h_pct` anchor is read once per asset per minute and shared, with concurrent misses single-flighted; the explorer build's burst of identical `/v1/price/batch` rows on a freshly restarted pool no longer re-reads `prices_1m` per row (8 of 595 calls crossed 200 ms after the v0.116.40 restart). Served values are unchanged (#3735).

### Security

- **build:** v0.116.40 moved to Go 1.27.3 for GO-2026-6809 (`crypto/x509` trusted compromised intermediates; reachable through passkey registration) (#3734).

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
