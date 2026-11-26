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

## [v0.138.0] - 2026-11-25

Tested against Stellar protocol 27. No migrations (schema version stays 236). No `pkg/*` break (no change under `pkg/`). No ClickHouse DDL change; an existing host needs `stellar.lending_interest_hourly` from `deploy/clickhouse/lending_interest.sql` (`si-apply-scope: operator`) if it has not been applied since v0.136.0. New metric: `stellarindex_api_degraded_responses_total`. `stellarindex_lending_interest_unexplained_versions` changes meaning (see Fixed).

### Changed

- **api:** `/v1/price/batch` reads the first stage of a request's data as set statements before its rows run: the cached triangulated price, the latest closed 1m bucket and the guard's trailing buckets. A prefetched answer is used only within 2 s and only for the exact key it was read for; anything else is read as before. Requests under 8 ids are not prefetched and only the first 200 ids are. Responses are unchanged (#4387).
- **api:** a `fiat:USD` `/v1/price/batch` also prefetches each asset's 24h-ago anchor bucket and its guard baseline, keyed on the anchor's minute, beneath the per-minute change-24h cache. Responses are unchanged (#4388).

### Added

- **api:** `stellarindex_api_degraded_responses_total{route,reason}` and a `degraded_reason` field on the request log line say why a response was flagged degraded: `refresh_failed`, `refresh_overrun`, `provisional`, `section_dropped` or `other` (#4390).

### Fixed

- **lending:** the interest pass window line reports explained, attributed and unexplained versions separately, with up to five samples of unexplained rows. `stellarindex_lending_interest_unexplained_versions` and the proof report no longer count attributed versions (a Blend V1 interest-auction fill, whose event carries no amount) as unexplained (#4389).

## [v0.137.0] - 2026-11-25

Tested against Stellar protocol 27. No migrations (schema version stays 236). No `pkg/*` break (no change under `pkg/`). Operators of an EXISTING ClickHouse host: `deploy/clickhouse/lending_interest.sql` and `deploy/clickhouse/lending_state.sql` (both `si-apply-scope: operator`) changed in comments and runbook text only, with no DDL change, so nothing needs re-applying before deploying. New operator flag `-interest-max-versions` (default 150,000) on `ch-entry-history`. The hourly interest pass is uninitialised on every host and derive revision 2 replaces revision 1; a host that had initialised it would re-derive from the floor. New metrics: `stellarindex_lending_interest_undecodable_events`, `stellarindex_api_price_batch_prefetch_total`.

### Added

- **lending:** `ch-entry-history -lending-only` has an interest pass that reads every stored reserve version from the lake, folds it into exact hourly rows and writes `stellar.lending_interest_hourly`. It stays idle until an operator runs `-interest-init`, never starts a backfill from the timer, and refuses a pool whose contract code is not listed with its arithmetic (#4373, #4375).
- **lending:** each interest residual is classified against the pool's own same-transaction events (interest-auction fill, gulp, BLNT reconcile loss) and marked explained only on exact equality; an event that cannot be decoded leaves its transaction's residuals unexplained and is counted in `stellarindex_lending_interest_undecodable_events`. Derive revision 2: revision-1 rows prove INCOMPLETE (#4380).
- **lending:** `ch-entry-history -lending-only -prove -interest` is a read-only proof of the stored hourly interest rows against the lake. Optional daily and weekly proof units (off by default) and the alerts `stellarindex_lending_interest_lagging`, `stellarindex_lending_interest_proof_failed` and `stellarindex_lending_interest_proof_stale` cover it (#4375, #4378).
- **api:** a request-scoped price read memo and Redis prefetch seams for `/v1/price/batch`, counted in `stellarindex_api_price_batch_prefetch_total{seam,result}`; a prefetched answer older than 2 s is read again. Nothing fills the memo yet, so responses are unchanged (#4381).
- **timescale:** set forms of six per-key price readers, each returning what its single-key read would; no handler calls them yet (#4384).

### Changed

- **lending:** the interest pass no longer refuses where an operator had no way out: the seed proof runs in chunks of at most 250,000 ledgers, `-interest-max-versions` bounds the derive and the proof and the refusal names it, and a BLNT interest fill with a missing or mistyped `auction_type` is treated as undecodable instead of skipped (#4383).
- **lending:** every interest mode holds the state lock shared and the state pass holds it exclusive, so state rows cannot change under a reading interest pass. A `lending_state` replay with `-from` arms an interest redo that must be re-derived before the pass runs, and seeds are proved against the lake (#4377).
- **explorer:** the static build writes each answered `/v1/price/batch` chunk to disk and reuses it in the second render pass, so each chunk is requested once per build (#4376).

### Fixed

- **api:** an old but healthy cached explorer page is flagged `stale` only, not `degraded`; `degraded` is set for a failed, refused or overrunning refresh, a provisional payload or a failed panel. This covers contract detail, holders, the contracts directory, contract stats and the operations directory; price, supply, market cap, TVL and balance caches are unchanged (#4382).
- **lending:** a `lending_state` replay no longer lets a concurrent interest run seed a reserve from a row before the deleted range, which counted one hour's rate movement twice (#4377).
- **lending:** the interest re-derive delete cutoff uses millisecond server time, matching `derived_at` (#4383).
