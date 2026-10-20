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

## [v0.116.34] - 2026-10-20

No migrations. No `pkg/*` break. No config change. After the deploy, watch `go_sql_wait_count_total{db_name="serving"}` and the API's new-session rate in `pg_stat_database` during the next explorer build.

### Fixed

- **timescale:** every Postgres pool keeps idle connections up to its open cap, so a burst no longer closes and reopens connections whose fresh backends re-plan hypertable chunks cold (r1 was opening 493 sessions a minute) (#3701).

### Added

- **api:** the serving pool exports `database/sql` stats as `go_sql_*{db_name="serving"}` (#3699).

## [v0.116.33] - 2026-10-20

No migrations. No `pkg/*` break. No config change. Operator step after the deploy: run the `deploy/clickhouse/contract_event_shapes.sql` runbook (DDL, per-partition backfill, verify, coverage marker); until its marker row exists, recognition keeps the raw scan.

### Fixed

- **api:** `/v1/price` remembers for a minute that a pair has no trades older than the 7-day window, so a burst of native-quoted `/v1/price/batch` misses stops re-planning the 400-day trades pass on every call (165 ms planning per arm on r1) (#3684).
- **api:** `/v1/assets` ranks a narrow set before the value joins (#3676).
- **aggregate:** the asset-character roll waits until the stored roll is due (#3677).
- **timescale:** per-venue pools aggregate on stored columns before the alias fold (#3678); source stats fold orientation over stored-column groups (#3681).
- **priceless:** the popular-priceless sweep takes one trades pass (#3679).
- **aquarius:** latest reserves narrow `DISTINCT ON` to each pool's last ledger (#3680).
- **healthchecks:** binary heartbeats ping every 40 s so jitter stays inside the 60 s period (#3682).
- **clickhouse:** recognition reads covered partitions from the MV-fed `stellar.contract_event_shapes` rollup instead of a raw scan of `contract_events` (#3683).

## [v0.116.32] - 2026-10-20

No migrations. No `pkg/*` break. No config change.

### Fixed

- **api:** the `/v1/price` last-trade fallback tries a 7-day window before its 400-day bound, so a fresh connection plans ~2 trades chunks per arm instead of ~58 (3 ms vs 205-240 ms on r1) and a burst of new connections no longer stalls price reads (#3674).
- **deploy:** the binary swap hard-links the backup and renames the new build into place, so the live path is never absent and a timer firing mid-deploy no longer fails with 203/EXEC (#3673).

## [v0.116.31] - 2026-10-20

No migrations. No `pkg/*` break. No config change.

### Fixed

- **api:** the first page of `/v1/markets` and `/v1/pools` at any limit up to 200 is sliced from one cached 200-row head slot, so the prewarm scans each listing once instead of 15 times after a restart (#3669).
- **sep41:** the observer watermark scans the last 7 days of supply events before falling back to the whole table, and a fold with no new events fills in a missing delta-read time bound, so both reads stop planning all 144 chunks (#3671).

### Changed

- **ci:** the mainnet explorer deploy is dispatched by `deploy.yml` after a stable r1 deploy succeeds, not by `release.yml` at the tag, so its prerender crawl no longer hits r1 while it restarts (#3670).
- Long comment blocks condensed across `internal/`, `cmd/`, `pkg/client` and the test files (#3662–#3667); actionlint's shellcheck pass is on (#3661).

## [v0.116.30] - 2026-10-20

No migrations. No `pkg/*` break. Apply the ansible role after the deploy: it renders `api.dashboard.resend_domain_id`. The deploy applies the alert-rule changes.

### Fixed

- **monitoring:** 34 counter alerts per rule tree also fire on a series' first appearance, so a one-off failure on an unseeded counter (born at N, where `increase()` reads 0) no longer goes unseen; the arm is muted until the TSDB holds its lookback (#3658).
- **obs:** per-source decode and orphan error series are exported at 0, and the launch-day F-0100 presence check names its 12 families so it can fail (#3657).
- **ansible:** the API config carries the Resend domain id, so the API pins click and open tracking off at boot (#3656).

### Changed

- **ci:** each stable release dispatches the mainnet explorer production deploy, still behind its approval gate (#3659).
- **docs:** the live-deployment smoke points at the public API (#3655).
