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

## [v0.116.30] - 2026-10-20

No migrations. No `pkg/*` break. Apply the ansible role after the deploy: it renders `api.dashboard.resend_domain_id`. The deploy applies the alert-rule changes.

### Fixed

- **monitoring:** 34 counter alerts per rule tree also fire on a series' first appearance, so a one-off failure on an unseeded counter (born at N, where `increase()` reads 0) no longer goes unseen; the arm is muted until the TSDB holds its lookback (#3658).
- **obs:** per-source decode and orphan error series are exported at 0, and the launch-day F-0100 presence check names its 12 families so it can fail (#3657).
- **ansible:** the API config carries the Resend domain id, so the API pins click and open tracking off at boot (#3656).

### Changed

- **ci:** each stable release dispatches the mainnet explorer production deploy, still behind its approval gate (#3659).
- **docs:** the live-deployment smoke points at the public API (#3655).

## [v0.116.29] - 2026-10-19

Postgres migrations 0219-0221 (catalog-only or new tables; no replay). No `pkg/*` break. Apply the ansible role after the deploy: it installs `trades-by-account.timer`.

### Fixed

- **storage:** the SEP-41 supply delta read is bounded by close time so it prunes chunks (#3642).
- **api:** `/v1/pools` breaks a `last_price` tie on the raw asset spelling (#3649).
- **completeness:** reconciliation seeds Soroswap pairs from the lake instead of stellar-rpc (11 s vs 4m41s on the reference deployment) (#3653).
- **decimals-guard:** Soroban DEX pairs are enumerated from `pools_per_source_1h` (#3651).

### Added

- **timescale:** `lp_reserve_current`, a trigger-kept per-pool reserve table for classic supply sums (#3644); `trades.usd_basis` records which valuation route produced each trade's `usd_volume` (#3647).
- **ops:** a daily `trades_by_account` catch-up to the compression horizon (#3652).
- **ansible:** r1's anonymous rate limit drops to 600/min now that explorer builds send a key (#3648).

## [v0.116.28] - 2026-10-19

No Postgres migrations. No `pkg/*` break. ClickHouse: apply Step 1 of `deploy/clickhouse/account_op_types.sql` on every host before this binary (the sink refuses to start without it); Steps 3-5 (backfill, verify, coverage marker) follow the deploy.

### Fixed

- **api:** `/v1/pools` prefilters raw rows on the asset arrays before the alias fold, so the base/quote indexes apply (1.7 s to 0.7 s on the reference deployment) (#3640).
- **api:** `LatestTradesForPair` inlines its LIMIT so Postgres caches one plan instead of re-planning every call (#3639).
- **api:** a classic asset's change strip is served from its listing snapshot when it has no change-summary row (#3636).
- **ops:** the `stellarindex` Postgres role gets `pg_read_all_settings` (#3637).

### Added

- **clickhouse:** `account_op_types`, a per-account op-type index (source arm by materialized view, participant arm by the sink); `/v1/accounts/{g}/activity` reads its counts from it once the coverage marker exists (#3633, #3634, #3641).
- **explorer:** the account page shows the sponsorships in force (#3630); the prerender sends a build API key, never the browser (#3631).
- **dev:** `api-latency-sweep --pages` times explorer detail pages (#3632).
- **docs:** the off-site drill's measured multi-hour restore and a 6h run ceiling (#3638).

## [v0.116.27] - 2026-10-19

No migrations. No `pkg/*` break.

### Fixed

- **ops:** `verify-usd-volume` passes a day with no trades only when `ledger_ingest_log` proves it tradeless (every ledger logged, zero classic trade effects, no trade rows), instead of failing every quiet day on a low-traffic network (#3628).

### Added

- **ops:** `tag-tx-index -chunks` fills `tx_index` on historical trades chunk by chunk (decompress, tag, recompress) under the trades `-chunks` lock, with a per-chunk dry run (#3626).

## [v0.116.26] - 2026-10-19

No migrations. No `pkg/*` break.

### Fixed

- **api:** `/v1/contracts/{id}/transfers?from=…` on a high-volume token answers 200 with `searched_from` set to the widest window it finished, instead of a 503 when the full-history read runs out of time (#3620).
- **api:** the 60 s self-prewarm also warms the `/v1/assets` listing reads that home, /assets and /rwa fire on first load (#3623), and the home page's `/v1/markets?limit=3` slot (#3624).

### Changed

- **scripts:** `api-latency-sweep.sh --pages` covers the main explorer pages (#3622).
- **clickhouse:** the empty `stellar.account_entry_changes` and `stellar.asset_entry_changes` are retired from the schema files and readers (#3625).
- **deps:** explorer npm minor bumps (#3616), zizmor-action (#3615) and google.golang.org/api (#3606).
