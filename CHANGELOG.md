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

## [v0.142.0] - 2026-11-27

Tested against Stellar protocol 27. One additive migration, 0239 (schema version 239): the new tables `lending_revenue_usd_daily` and `lending_revenue_usd_coverage`, applied automatically by the deploy; it also re-creates `apply_api_role_grants()` so the API role can insert and update both. No `pkg/*` break (only a spec-contract test under `pkg/client` changed). No ClickHouse DDL change, so no operator DDL. Operators: the lending revenue rollup worker is new and off by default (`[lending_revenue_rollup] enabled`; ansible `stellarindex_lending_revenue_rollup_enabled`, default false). Until it is enabled, the `revenue` windows on `/v1/lending/protocols` read `not_valued` on every host. To enable: deploy with migration 0239 applied, set the variable; the self-fill takes about 5.5 h, and until it finishes the windows are lower bounds naming the days not yet valued. New ticket alerts `stellarindex_lending_revenue_rollup_stalled` and `stellarindex_lending_revenue_rollup_sweep_mismatch` cover the worker; do not enable it on a host that serves readers without them.

### Added

- **api:** `GET /v1/lending/pools/{pool}/revenue` serves a Blend V1, Blend V2 or BLNT pool's interest per reserve and hour or UTC day from `stellar.lending_interest_hourly`: interest accrued, its split (supplier share, backstop take, BLNT protocol fee), supplier credit, backstop drawn, supplier losses and the unexplained gain, in the reserve's base units as decimal strings, never summed across reserves. A partial sum sets `lower_bound` and names what was excluded (#4426).
- **api:** the same endpoint values each reserve and bucket in USD at each hour's price (`usd` object, `totals_usd`, `price_max_age_hours`), using the `/history` price rule: the hourly VWAP at or before the hour, at most 6 h old, with the scam-issuer gate and thin-market guard. An hour with an amount and no price is counted and makes the sum a lower bound; nothing valued is null with a reason, never "0.00" (#4427).
- **api:** `GET /v1/lending/pools/{pool}/bad-debt` serves per reserve what moved to the backstop, what the backstop covered by auction, what was socialised to suppliers, what the backstop repaid and what is outstanding, in d-tokens, underlying and USD, with a `reconciled` check and a residual when it does not close (#4428).
- **api:** migration 0239 adds `lending_revenue_usd_daily` (exact USD per pool, reserve and UTC day, revenue legs as unscaled NUMERIC, NULL when nothing was priced) and `lending_revenue_usd_coverage`, and a worker in `stellarindex-api` that keeps them, off by default behind `[lending_revenue_rollup] enabled`. It runs every 10 minutes under an advisory lock, re-values changed, stale and revision-mismatched days within a per-cycle budget, and sweeps all stored days once a day to compare against a fresh walk. `/revenue` bodies are byte-identical to before (#4429).
- **api:** `/v1/lending/protocols` gains a `revenue` block per protocol over 1d, 7d, 30d and all time, summed from `lending_revenue_usd_daily`: interest accrued, supplier share, backstop take, protocol fee (BLNT, `fee_rate_inferred`), backstop drawn and default loss, with no summed revenue field. A window missing any pool, day, price or revision is a lower bound naming it; with the rollup off the block is present, null and `not_valued`. Blend keeps V1 and V2 in one row with a per-version split (#4432).
- **monitoring:** alerts `stellarindex_lending_revenue_rollup_stalled` (no success or start for 30 minutes) and `stellarindex_lending_revenue_rollup_sweep_mismatch` (a daily sweep difference), ticket severity, in both rule trees, promtool-tested; a new `started_unix` gauge covers a worker that never succeeded (#4432).
- **explorer:** the lending pool page shows an interest and revenue panel (USD over the range control, split by leg, stacked daily chart, per-reserve table) and a bad-debt panel (five parts, reconciliation bar or residual). Null figures render a dash with the reason, and "at least" appears exactly where `lower_bound` is set; reasons are typed over the generated enums (#4431).
- **explorer:** the lending page shows an interest and revenue panel per protocol with a window switcher; while a window is `not_valued` it reads "Revenue totals are not computed yet." (#4433).
- **explorer:** `/lending/blend` and `/lending/blnt` are pages of their own, with protocol totals, that protocol's revenue windows, its pools table and a references footer; `/lending` keeps the overview charts and combined pools table with one card per protocol. `/lending/[pool]` and the sitemap now keep only contract ids (#4434).

### Changed

- **api:** `reserveUSDWalk` now tells the read-time verdicts (scam issuer, operator withholding, thin market) apart from price-data refusals and read failures, and the walk budget is a parameter (30 s for the worker, 2 s on the request path); `/revenue` responses are unchanged (#4429).

### Fixed

- **aggregator:** a VWAP window whose raw trade read hits `MaxTradesPerWindow` now covers the whole window: minutes the `trade_sums_1m` view has not materialised yet (up to 15) are summed from raw trades with the view's own arithmetic. Before, a trade burst shorter than the view's refresh lag served the newest slice flagged `truncated`. It never covers less than the capped read, and the truncation log line names the reason (#4436).
- **api:** the `/v1/accounts/{account}/earnings` note and spec now state per protocol where claimed backstop emissions land: on Blend V2 the claim pays backstop LP tokens deposited in the same transaction, so they count in `net_flow` and `accrued` excludes them; on BLNT `accrued` includes them; Blend V1 is unverified. No served number changed. `docs/protocols/blend.md` and `blnt.md` record the lake census verdicts of 2026-11-27 (#4437).
- **tests:** `TestPriceTipStream_DivergenceBudgetDiscriminatesAtItsEdge` and `TestContractDetailCached_ExpiredWithinBoundServedClean` no longer depend on scheduling; no production code changed (#4425, #4430).
- **docs:** `docs/protocols/blend.md` and `blnt.md` inventory Blend and BLNT emission events and name three facts still unverified (the V2 backstop claim's unit, which V1 pool WASMs emit claim events, and whether event emission rate equals the stored one); documentation only (#4435).

## [v0.141.0] - 2026-11-26

Tested against Stellar protocol 27. One additive migration, 0238 (schema version 238): the new table `sushiswap_v3_manager_events`, applied automatically by the deploy. No `pkg/*` break (no change under `pkg/`). ClickHouse DDL changed: `deploy/clickhouse/lending_interest.sql` adds the columns `default_loss`, `default_versions`, `interest_drawn`, `gulped` and `reconcile_backstop_loss` to `stellar.lending_interest_hourly` (`si-apply-scope: operator`). Operators: apply it on every host, test nets included, BEFORE the deploy, since a new binary against a table without the columns fails the whole insert and writes nothing. After the deploy, re-derive lending interest from ledger 51499541 and run the full proof (derive revision 4); rows of an older revision prove INCOMPLETE until then. Run the Sushi position-manager catch-up, `projected-rebuild -source sushiswap_v3 -from 61487383 -only-kinds sushiswap_v3.manager`, as a dry run first and then with `-write` and `-to` pinned to the dry run's value; until it runs, `sushiswap_v3` completeness drops and the manager target reports a leading gap. New ticket alert `stellarindex_lending_interest_unexplained_versions` fires while a proof run still counts versions no event explains, which includes the 8 Blend V2 `defaulted_debt` versions until the re-derive completes.

### Added

- **sushiswap_v3:** migration 0238 adds `sushiswap_v3_manager_events` (hypertable, compression, no retention): kind, token id, owner or recipient, liquidity and both amounts, with a check per kind. The position manager's `mint`, `burn`, `increase_liquidity`, `decrease_liquidity` and `collect` events decode into it, about 10,300 since ledger 61,487,383; `upgraded` and `migrated` are recognised and write nothing. The manager is a curated contract of the `sushiswap_v3` gate, matched by exact id before the pool registry, so a pool event and a manager event of the same name are told apart by contract identity. Any other topic or shape from the manager is a counted decode error naming the topic. The owner is the holder at mint or burn: both manager code hashes have transfer and approve functions, so it is not necessarily the current holder. The audit log covers 58 of the 81 announced pools (#4419).
- **blend:** `blend.DefaultedBRate` reproduces a V2 `defaulted_debt` write-off exactly (accrue, then subtract `ceil(ceil(d_tokens * d_rate / S) * S / b_supply)` per event, floored at 0), matching all 8 production versions from lake XDR. `stellar.lending_interest_hourly` gains `default_loss` and `default_versions`; `supply_residual` minus `default_loss` is the unexplained supplier residual. Alert `stellarindex_lending_interest_unexplained_versions` is new (#4416).
- **blend:** derive revision 4 stores each explained version's event amounts by cause in `interest_drawn` (at most 0), `gulped` (at least 0) and `reconcile_backstop_loss` (at most 0), in reserve base units, so `residual_explained` equals their sum exactly. An event whose sign its cause cannot produce explains nothing. Each column covers explained versions only and is a lower bound; attributed V1 fills, causes inside unexplained versions and events netting to zero in a reconciling version are not classified (#4422).

### Changed

- **blend:** a V2 `defaulted_debt` cut of the suppliers' `b_rate` is explained, and recorded as supplier loss outside interest, only when the stored `b_rate` equals accrual followed by the pool's write-off rule for a single ordered version whose transaction has no undecodable event. Anything else stays unexplained, as do BLNT `defaulted_debt` and V1 retroactive `bstop_rate`. The derive revision rises to 3 and then 4, and rows of an older revision prove INCOMPLETE rather than unexplained (#4416, #4422).
- **aggregate:** the quote-surface check that refuses a pivot leg for triangulation and composite corroboration compares stablecoin and real-USD prints per time slot (window/24, at least 1 minute) rather than as two whole-window VWAPs, so a price trend under shifting volume no longer reads as a de-peg. It decides alone only when eligible slots hold at least 95 % of stablecoin volume and both surfaces clear the volume floors; otherwise the verdict is the larger of the aligned and whole-window gaps. Replayed over 697 rolling 24h production windows, false alarms fall from 34 to 7. All arithmetic is exact `big.Int`/`big.Rat`; `stellarindex_aggregator_quote_surface_bps` keeps its name and sign, and the `leg_quote_surface` log field gains the alignment and coverage (#4421).
- **redstone:** the `earnUSDC_PPS_FUNDAMENTAL/USD` feed, published since 2026-10-30, is on a kept-raw list: it writes the same raw row as before, no longer counts as an unknown symbol, and never reaches VWAP, a pair leg or a supply key. `oracle_updates` has no feed dimension, so mapping it would interleave two prices of the earnUSDC vault share in one series (#4420).

### Fixed

- **storage:** a rollup partition holding more than one build is no longer served. After a restart inside `old_parts_lifetime` (480 s) of a `REPLACE PARTITION`, the replaced parts came back beside the new ones and `account_op_type_counts` partition 65 served doubled counts for about 35 minutes. The counts rollups (op types, movements) now read such a partition from the source with `FINAL`, and the job replaces it without `-shrink-ok`; a genuine shrink in a single build still refuses. The `contracts_census_daily` directory query takes one row per (day, contract) before summing. `ErrCountsShrink` no longer says the lake lost rows. The restart runbook gains a pre-restart and a post-restart check. Open: `asset_stats_daily` has the same exposure and no served reader (#4417).
- **indexer:** `TestRecordHashdb_DurationExcludesMarshal` reads a fake clock instead of sleeping, so it no longer fails on a loaded machine (#4418).

## [v0.140.0] - 2026-11-26

Tested against Stellar protocol 27. One additive migration, 0237 (schema version 237): the new empty table `aquarius_pools`, applied automatically by the deploy. No `pkg/*` break (no change under `pkg/`). No ClickHouse DDL change, so no operator DDL. Operators: the stored `aquarius_liquidity` rows for the 63 concentrated Aquarius pools (ledger 62,339,683 onward) keep their old, mis-ordered amounts until the liquidity-only `projected-rebuild -only-kinds aquarius.liquidity` procedure in `docs/operations/runbooks/projector.md` is run; the decoder fix alone does not rewrite them. No served route reads those amounts or shares, and pool reserves and TVL are unchanged.

### Added

- **aquarius:** migration 0237 adds `aquarius_pools (contract_id, pool_type, first_ledger, observed_at)`, the raw `add_pool` type of each pool. The live indexer writes it, the boot warm reads it, and rebuild and audit tools only read it, so a pool announced after the in-code census keeps its liquidity layout across restarts. A live type that contradicts the in-code list is not written (ERROR and a failure counter); a stored row that contradicts it leaves only that pool's liquidity refused. The ingestion-sink runbook has the recovery SQL (#4411).
- **ops:** `projected-rebuild -only-kinds <kind,...>` limits a run to the named decoder output kinds, so a decoder fix to one kind no longer rewrites every trade's `usd_volume`. Checkpoints carry the kind list, an empty or undeclared kind is refused before any work, aggregates refresh only when a trade kind is written, and the summary reports written and failed counts per kind and the run's `derive_generation`. For aquarius it lists every gated pool whose layout is not established. The projector runbook gains the liquidity-only correction procedure (#4413).
- **supply:** skip reasons `topic_not_token_shaped` (no alert) and `topic_unparseable` (alerts, including an unreadable `topic[0]`); `ch-cap67-movements` moves a contract's topic-shape skips to `token_topic_not_token_shaped` (alerts) when the same window decoded a movement for it (#4412).

### Changed

- **supply:** an event with too few topics no longer counts under `not_token_shaped`, so pool mints stop counting toward the supply skip alerts; both skip alerts exclude `topic_not_token_shaped` in both rule trees. The two judged-non-token pool entries are removed, since the topic rule skips their burns first. `supply_flows` rows are unchanged (#4412).
- **ops:** the Aquarius router build `2f8e7d7f` (upgraded in place at ledger 65,663,788) is recorded as audited in the WASM audit record, with four post-upgrade router events and the matching pool-side events as fixtures; the daily wasm-drift run no longer reports it (#4409).

### Fixed

- **aquarius:** concentrated pools emit `deposit_liquidity` and `withdraw_liquidity` as `[liquidity, amount0, amount1]`, not `[amounts..., shares]`, and were stored shares-last. They now decode in their own order and store shares NULL, and an event whose layout is unknown is refused. Layouts come from the `add_pool` type or two in-code lists that match the census of 395 pools (332 classic, 63 concentrated); boot logs a WARN for any warmed pool in neither. The audit record corrects the earlier sign-off of the four concentrated builds. Stored rows are corrected by the runbook procedure above (#4410).
- **monitoring:** `stellarindex_textfile_producer_stale` no longer fires on the heartbeats of one-off jobs without a timer (`ch_backfill`, `supply_seed_sac_balances`, `supply_seed_claimable_balances`); unknown and scheduled producers still alert (#4414).

## [v0.139.0] - 2026-11-26

Tested against Stellar protocol 27. No migrations (schema version stays 236). No `pkg/*` break (no change under `pkg/`). No ClickHouse DDL change. Operators: the role now takes the USD and fiat peg lists from `stellarindex_usd_pegged_classic_assets` and `stellarindex_fiat_pegged_classic_assets` and enables oracle, external and price sources only on pubnet; a host that relied on the pubnet lists being rendered on another network must set them explicitly.

### Added

- **api:** `/v1/pools`, `/v1/markets` and the asset routes record why a response was degraded (`refresh_failed`, `refresh_overrun`, `refresh_refused`, `partial_read`, `check_unread`, `section_dropped`, `source_stale`, `provisional`, `request_cancelled`) in `stellarindex_api_degraded_responses_total{route,reason}` (#4403).
- **ops:** `stellarindex-ops cagg-history` measures, for every continuous aggregate, the start of its materialised history against the start it should have, and writes `stellarindex_cagg_history_start_seconds`, `_expected_start_seconds` and `_gap_seconds`; `data-freshness.sh` calls it in place of its SQL block (#4404).
- **ops:** `stellarindex_api_cache_fill_duration_seconds{cache,result}` times detached cache fills (#4402).
- **ansible:** a test net's local Prometheus can load alert rules (`local_prometheus_rules_enabled`, off by default); the five served-value rules moved to `served-values.yml` (#4399).
- **ansible:** per-network USD and fiat peg variables, with a preflight assert (#4400).
- **aggregate:** `aggregate.TWAPFromBuckets`, an exact time-weighted mean over closed buckets; no caller yet (#4401).
- **ci:** a weekly reminder when the `directory-sync` pin falls behind the public directory; the release procedure names the docs publish step (#4395).

### Changed

- **api:** a degraded asset-detail body lives 3 s in the detail cache, not 120 s (#4397).
- **aggregator:** source contributions are written in a fixed order and stamped from the orchestrator clock, so the same trades give the same rows (#4401).
- **config:** a pair listed twice in `aggregate.pairs` is rejected at load (#4402).
- **explorer:** the static build re-asks a partial answer once, inside a five-minute budget shared by the whole build, and warms asset keys first (#4394, #4396).
- **ops:** `directory-sync` is pinned to the 2026-11-25 public directory (#4393).

### Fixed

- **api:** an asset list, asset detail, `/v1/markets` or `/v1/pools` response built over a cached value that is merely past its bound is flagged `stale` only. `degraded` is set for a failed, overrunning or refused refresh, an unmeasured read or an unchecked directory. A cached asset-list replay whose rows refresh failed is now flagged degraded (#4405).
- **api:** `/v1/price/batch` rows walk the quote asset's aliases as `/v1/price` does, so an XLM-quoted batch matches the single-price answer (#4398).
- **supply:** a `mint`, `burn`, `clawback` or `transfer` event without the address topics a token event carries is classified `not_token_shaped` before its amount is read, in the live sink, `ch-cap67-movements` and `ch-supply -seed-flows`. A pool's one-topic `mint` no longer raises the supply-skipped alerts, and no `supply_flows` row is written for such an event (#4406).
- **ci:** test-net explorer builds use their own network's API key (#4392).
