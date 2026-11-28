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

## [v0.147.0] - 2026-11-28

Tested against Stellar protocol 27. No migration (schema version stays 241). No `pkg/*` break. No ClickHouse DDL change. New config section `[lending_health]` (`enabled = false` by default; role variable `stellarindex_lending_health_enabled`). API: new route `GET /v1/lending/pools/{pool}/health`, which answers 404 `lending-health-not-enabled` until the flag is set. One new ticket alert, `stellarindex_lending_health_fill_failing` (346 ticket rules).

### Added

- **api:** `GET /v1/lending/pools/{pool}/health` (off by default) serves per-account health factors for a Blend pool, computed on read and priced only on the pool's own oracle at one ledger. An account is called `liquidatable` only when every reserve it holds has an oracle price no older than 24 h, no user-liquidation auction entry exists for it, it is not the pool or its backstop, and the class is the same at both ends of an interest-accrual bound; otherwise it is `healthy`, `bad_debt`, `in_auction`, `liquidation_exempt`, `indeterminate` or `unpriced` with a named reason. Reserve config, reserve state, positions, auctions and the pool instance are read from ledger entries, and anything modified after the evaluation ledger is `state_newer`. Totals cover priced accounts only and are lower bounds naming what is excluded. A present auction entry always counts as live, so an expired auction can keep an account `in_auction` but never makes it `liquidatable`. BLNT pools answer `health_formula_unverified`; V1 accounts with pending interest are `accrual_unbounded`. Pages are cut from one cached view per fill; the cursor carries the view's ledger and `view_changed` says when two pages came from different ledgers (#4473).
- **api:** pool reserves can be priced on the pool's own oracle. For the two audited `oracle-aggregator` programs (v2.0.0 and v1.0.0, byte-identical to upstream releases) the adapter's `lastprice` is reproduced from its on-chain config and the Reflector feed rows already ingested, cross-checked against the feed's own on-chain round history. Every case where the contract would return nothing is a named verdict (`oracle_price_stale`, `oracle_deviation`, `oracle_feed_incomplete`, `oracle_state_newer`, `oracle_unsupported`, …), never an older or market price. Used only by the health route (#4471).
- **blend:** `docs/protocols/blend.md` gains a "Yields" section pinning every yield definition to upstream source, and `internal/sources/blend` gains exact realised-rate math (`RealisedAPR`, `ComposeNetYield`, `Rankable`). A realised rate is the growth of the on-chain claim between two accruals; a rate too coarse for its window returns `ErrLowResolution`, and a reserve that is frozen, fully utilised or carrying backstop-held liabilities is not rankable. Nothing serves it yet (#4474).
- **explorer:** the Blend and BLNT protocol pages show a token emissions panel from `/v1/lending/protocols`: one row per stream (never summed), a USD / token toggle, "at least" for lower bounds and the reason in words for a figure that is withheld (#4472).
- **monitoring:** `stellarindex_lending_health_fill_failing` (ticket) fires on sustained failures to read a pool's positions for the health route (#4473).

### Fixed

- **monitoring:** `stellarindex_textfile_producer_stale` no longer fires for the weekly lending-interest proof job's textfile, which has its own 8-day alert. A new lint fails when a timer that runs less often than daily is not declared beside the rule (#4470).

## [v0.146.0] - 2026-11-28

Tested against Stellar protocol 27. Migration 0241 (schema version 241): two `boolean NOT NULL DEFAULT false` columns on `completeness_snapshots`; a metadata-only change the previous binary keeps working against. No `pkg/*` break. No ClickHouse DDL change; the ClickHouse server gains a `config.d` file that takes effect at its next restart (the role does not restart it). API: `/v1/lending/protocols` gains an `emissions` block per protocol and `/v1/coverage` an optional `absent_by_design` boolean; `GET /v1/accounts/{g_strkey}` and `/v1/lending/protocols` change when they are served `no-store`.

### Added

- **api:** `/v1/lending/protocols` carries an `emissions` block per protocol (windows `1d`, `7d`, `30d`, `all_time`), summed per stream from the stored emissions rollup. Streams are never added together. A window not fully covered is a lower bound naming what is excluded, with separate flags for USD and token amounts; a pool with no coverage is unknown, never zero; each stream's history verdict and the emission token's identity are re-checked on every read, so an unverified history or token turns an otherwise empty day into a lower bound or a refusal instead of an exact zero. Until the emissions rollup is enabled on a host, every figure reads `not_valued` (#4463).
- **explorer:** the lending pool page charts emissions over time, one chart per stream on a shared scale, with a USD / token toggle. Unpriced and unindexed buckets are drawn hatched with their reason, and the summary line reads "at least" when any bucket is partial. The shared segmented control scrolls inside its own box on narrow screens (#4462).
- **ops:** `stellarindex_sources_absent_by_design` (role variable, empty by default) lets a network declare sources it does not have. A declared `domain/source` that is empty is not reported stale by the data-freshness watchdog. A declared `sdex` is accepted as complete only when substrate and recognition are verified, the raw operations tables match the ledger header counts, and a whole-range lake read finds no trade; only `sdex` may be declared, and an unknown name fails the run. A declared source that holds rows raises `stellarindex_data_source_absent_declared_but_present` (ticket). `/v1/coverage` reports `absent_by_design` from the stored snapshot (#4459, #4466).
- **ops:** ClickHouse system log tables get explicit retention (`clickhouse_system_log_retention_days`; `query_log` 14 days, `trace_log` 3, `processors_profile_log` 1, others 7 to 30) and the role drops renamed `system.<log>_<n>` leftovers by an exact allow-list (#4461).
- **blend:** the health-factor formula is pinned to upstream source for V1 and V2 in `docs/protocols/blend.md`, and `internal/sources/blend` gains exact integer health math (`AccountHealth`, `Classify`, `HFFloor4`, `InBand`, `ShockThreshold`). Nothing serves it yet (#4468).
- **storage:** lending positions are read with collateral and non-collateral supply as separate maps (their sum is unchanged), and a reader lists a pool's live auction keys. No served number changes (#4464).

### Changed

- **api:** a degraded revenue or emissions block on `/v1/lending/protocols` makes the response degraded and `no-store`; a failed read is no longer cached as a settled answer. Settled exclusions (`withheld`, `days_not_valued`, `price_missing`, `valuation_stale`) stay cacheable (#4463, #4467).
- **api:** `GET /v1/accounts/{g_strkey}` reports a background refresh the shared gate turned away as `refresh_refused` for that response only; it is no longer recorded as a failed refresh. A recorded failed refresh is now degraded and `no-store` with reason `refresh_failed`, as on the other explorer caches (#4460).

### Fixed

- **ops:** a job heartbeat file that cannot be written is logged once per run, and the lending state and interest units exit non-zero after their work is durable, so `stellarindex_systemd_unit_failed` fires within minutes. Previously the write error was dropped and only `stellarindex_textfile_producer_stale` fired a day later (#4465).

## [v0.145.0] - 2026-11-28

Tested against Stellar protocol 27. Migration 0240 (schema version 240): two new tables, `lending_emissions_usd_daily` and `lending_emissions_usd_coverage`, and their API-role grants; nothing existing is altered. No `pkg/*` break. No ClickHouse DDL change. No API shape change: one `usd_reason` description is widened.

### Added

- **api:** the lending emissions USD daily rollup worker stores, per pool and UTC day, the same USD the emissions history endpoint serves for that day, with per-pool coverage. It runs on the shared day-rollup skeleton, is off unless `stellarindex_lending_emissions_rollup_enabled` is set, and nothing reads its rows yet. A day whose read lands off the UTC day, or whose emission-token decimals read failed, is not stored and is retried; a pool with no coverage row is unknown, never zero. Alerts `stellarindex_lending_emissions_rollup_stalled` and `stellarindex_lending_emissions_rollup_sweep_mismatch` (tickets) in both rule trees (#4456).

### Changed

- **api:** the day-rollup cycle log carries `pools_skipped`; a pool whose planning read fails is a contained per-pool failure rather than a failed cycle (#4456).

### Fixed

- **api:** a token-decimals read that fails is no longer served or stored as `decimals_unknown`. It now reads `price_read_failed`, the response is degraded and not cached, and the lending revenue rollup leaves the day unstored and retries it. Affects `/v1/lending/pools/{pool}/revenue`, `/reserves`, reserve and pool history, backstop and auction USD, `/emissions`, and the TVL exclusion on `/v1/lending/protocols`. Reserve history reads the scale once, so a null `decimals` can no longer sit beside an exact USD value. Revenue rows already stored with a false `decimals_unknown` are rewritten by the daily sweep within a day; `stellarindex_lending_revenue_rollup_sweep_mismatch` may raise its ticket during that period, where it means a stored row was corrected (#4457).

## [v0.144.0] - 2026-11-27

Tested against Stellar protocol 27. No migration (schema version stays 239). No `pkg/*` break. No ClickHouse DDL change. No API contract change: several responses now carry the correct UTC day or month where a non-UTC ClickHouse server zone shifted it.

### Added

- **ci:** `scripts/ci/lint-ch-day-zone.sh` fails a ClickHouse read that takes its calendar day from the server's time zone (zone-less `today()`, `yesterday()`, a calendar function of `now()`, `toDateTime()` of a Date); `chzone:ok <reason>` exempts a line and is watched by the baseline-growth guard (#4451).
- **ci:** the shared integration ClickHouse container runs in `Europe/Berlin` and the harness refuses any other server zone, so every lake-reading integration test meets the condition production runs under (#4451).

### Changed

- **monitoring:** `stellarindex_zfs_pool_fill_90pct_within_7d` is a ticket, not a page; `stellarindex_zfs_pool_free_critical` and `stellarindex_zfs_pool_critical_space` remain the pages. With no page left in it, the `zfs_pool_fill_trend` inhibit family is removed (#4448).
- **ops:** pgBackRest `repo1` (local) keeps one full backup set, down from two; the off-site `repo2` is unchanged. Local restore depth is one chain, about 8 days (#4448).
- **api:** the lending revenue rollup contains a failing pool: other pools store their rows and coverage, the failed pool's days wait in a capped retry queue, planning state still commits, and the cycle still reports failure so `stellarindex_lending_revenue_rollup_stalled` fires. Cycle logs carry `queued`, `empty`, `reserve_days` and `retry_dropped` (#4450).

### Fixed

- **api:** the lending revenue rollup stored no rows while advancing coverage on a host whose ClickHouse server zone is not UTC: a `Date` decodes at server-zone midnight, so no interest day matched the worker's UTC days. Days are now read as UTC-tagged instants; coverage cannot move past a day that has interest hours and no stored row; coverage written at an older valuation or derive revision is not trusted by `/v1/lending/protocols` and is re-proved by the fill. `lendingRevenueValuationRev` is 2 (#4450).
- **storage:** eight ClickHouse readers returned a day or month early on a non-UTC server zone: contract deployment months and `active_90d_through`, account cohort months, the first-supply-event lookup (missed a first mint in the last UTC hours of a day), protocol daily series, breakdown and roster ("complete days" were cut at server-zone midnight), protocol `last_seen`, the contract daily series, asset trustline history and the contracts census rollup start. Stored lake days were already UTC; only reads changed (#4451).
- **aggregator:** the wide-minute re-read (`TradesInMinutes`) no longer depends on table statistics to use the pair index: the pair travels as columns of a materialised minute list, so the planner cannot price the probe from each asset's own frequency and fall back to a time-only scan reading every pair's rows (#4452).
- **ops:** on a network whose history archive is empty by design (first ledger at or below 2 and an archive listing that succeeds with zero objects), the archive tip-lag and Tier-A verification units no longer fail, and data-freshness reports an empty source stale only when the chain itself is older than the threshold (`stellarindex_data_freshness_empty` gauge) (#4449).
- **api:** an explorer cache whose background refresh the refresh gate turned away no longer records that as a failed refresh. The entry keeps its real last outcome, that one response carries `degraded_reason=refresh_refused`, and the next request tries again; before, the entry stayed marked failed until a later refresh succeeded and every serve in between read `refresh_failed`. Six caches: contract detail, holders, contracts directory, operations directory, network throughput and contract stats. The `stellarindex_api_degraded_responses_sustained` description now names the counters that explain a refusal (#4454).

## [v0.143.0] - 2026-11-27

Tested against Stellar protocol 27. No migration (schema version stays 239). No `pkg/*` break (one test line under `pkg/client`). No ClickHouse DDL change. API additions only: one new endpoint and new optional fields on it.

### Added

- **api:** `GET /v1/lending/pools/{pool}/emissions/history` serves a Blend V1, Blend V2 or BLNT pool's emissions per hour or UTC day in token units: the emission schedule per reserve line, pool claims in total and by reserve, the backstop allocation and BLNT backstop tiers. Each stream is bounded by its own writer's progress, and emitted and claimed amounts are separate layers that are never summed (#4441).
- **api:** the same endpoint values each stream and bucket in USD (`usd` object, `price_max_age_hours`) with the revenue price rule: the hourly VWAP at or before the hour, at most 6 h old. A bucket not fully priced is a lower bound or null with a `usd_reason` (`no_price_within_bound`, `withheld`, `amount_unread`, `reads_disagree`, `hourly_read_failed`, `emission_token_unread`, `emission_token_unverified`), never zero. Daily buckets are valued hour by hour from a read that shares one snapshot with the daily amounts, and a served USD figure never exceeds what the served token amount supports. Blend V1 claims carry no USD; the BLNT schedule's USD is marked `approximate` (#4444).
- **monitoring:** counter `stellarindex_aggregator_window_wide_minutes_unresolved_total{pair,window}` separates a window left partial by over-spread minutes from one that hit the row cap (#4445).
- **ci:** `check-selftest-runners.sh` fails a self-test that runs a mutating git command on the real repository without first building a throwaway one (#4443).

### Changed

- **api:** the per-day USD valuation and the rollup cycle behind the lending revenue worker are shared code (`usd_day_valuer.go`, `day_rollup.go`) so a second rollup supplies only its own reads and rows. Revenue output is byte-identical; the worker's cursor, exclusion and gauge paths gained unit tests (#4442).

### Fixed

- **aggregator:** a capped VWAP window no longer stays partial when more than ten of its older minutes have prints spread wider than the outlier band. Those minutes are now re-read raw in packed, set-based reads (`TradesInMinutes`, one index probe per minute, at most ten reads of `MaxTradesPerWindow` rows per leg and tick), not one query per minute capped at ten. Before, every tick re-read the same ten minutes and the window stayed flagged truncated; on production the XLM/USD 24 h window covered about 8.5 hours. A window whose over-spread minutes exceed the row budget stays flagged with its true `covered_from` (#4445).
- **ci:** six gate self-tests (doc links, attribution, agents file, docs, runbook annotations, external channels) run in a throwaway copy of the tree. They used to write fixtures into the real working tree, edit and restore tracked files, and one made and reset a real commit; a parallel lane saw a fixture directory vanish mid-scan and the release pre-tag check failed (#4440, #4443).
- **tests:** explorer unit tests drain react-query notifications before each test ends, so a late notification no longer reaches a torn-down environment (#4439).
