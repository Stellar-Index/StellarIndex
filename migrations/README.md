# Database migrations

TimescaleDB / PostgreSQL schema migrations, `golang-migrate` format.

Numbering is four-digit sequential. Each migration has a matching
`up.sql` / `down.sql` pair. `down.sql` must fully reverse `up.sql`
where possible; for irreversible operations (e.g. dropping a
hypertable chunk), the `down.sql` contains a comment explaining the
asymmetry.

## Running

Through the `stellarindex-migrate` binary (preferred):

```sh
make db-migrate-status    # what's applied
make db-migrate-up        # apply everything pending
make db-migrate-down      # roll back one
```

Direct via `golang-migrate` CLI:

```sh
migrate -path migrations -database "${STELLARINDEX_POSTGRES_DSN}" up
migrate -path migrations -database "${STELLARINDEX_POSTGRES_DSN}" down 1
```

## Rules

1. **Never edit a migration that has run in production** (this
   includes staging). Add a new migration instead. The one narrow,
   written-down exception — correcting a WRONG COMMENT — is spelled out
   in [Amending a shipped migration](#amending-a-shipped-migration)
   below; it never touches an up-migration's SQL.
2. **Numbering must be dense** — no gaps, no duplicates.
   (Historical exception, recorded 2026-08-04: 0075, 0077, 0078, 0079
   and 0084 are unused. They were never present in the tree and nothing
   was ever deleted — the rule applies going forward.)
3. **Changes to TimescaleDB features** (hypertables, compression,
   continuous aggregates) must be done with the extension's API
   (`create_hypertable`, `add_compression_policy`,
   `refresh_continuous_aggregate`) — not by touching the internal
   `_timescaledb_*` schemas directly.
4. **Every migration that creates a continuous aggregate** also adds
   its refresh policy + retention policy in the same file. A CAGG
   without a refresh policy is a silent bug.
5. **Amounts are always `NUMERIC`** (arbitrary precision). Never
   `bigint` — breaks i128 per ADR-0003.
6. **IDs follow canonical wire form** as text: `<code>-<issuer>` for
   classic, `C…` for Soroban, `native` for XLM. See
   `internal/canonical/asset.go`.
7. **Migrations are applied as the `stellarindex` app role, never as
   a superuser.** Always go through `stellarindex-migrate` /
   `STELLARINDEX_POSTGRES_DSN` (the DSN under "Running" above) — that
   DSN is the `stellarindex` role, so every object a migration
   creates is owned by `stellarindex` and the application has full
   access to it by construction. This is why a bare
   `CREATE TABLE …` needs no explicit `GRANT … TO stellarindex` — on
   a correctly-applied deploy (R2/R3/fresh) the app *is* the owner.
   Applying a migration manually as the `postgres` superuser
   instead makes the object superuser-owned and the app loses
   access to it. That happened to `source_entry_counts` (migration
   0035) on r1 on 2026-05-19 — `permission denied for table
   source_entry_counts (42501)` from the indexer's always-on entry
   tally and from `stellarindex-ops seed-entry-counts`. Hot-fixed in
   place with `ALTER TABLE source_entry_counts OWNER TO stellarindex`
   (the canonical shape — matches `trades` and every other table).
   Do **not** "fix" this class of issue with a follow-up GRANT
   migration: run as the `stellarindex` role it cannot `GRANT`/
   `ALTER` on a superuser-owned object (errors in exactly the
   anomaly case), and on a correctly-owned object it is a redundant
   self-grant no-op. The fix is operational (apply as the app
   role), not schema.

8. **Ratio aggregates use the single-division exact form.** A
   volume-weighted price in a CAGG is
   `sum(quote_amount) / sum(base_amount)` — one division at the
   end, exact under NUMERIC. Never the per-row form
   `sum((quote/base) * base) / sum(base)`: each per-row division
   rounds at NUMERIC division scale, so the result is inexact by
   construction. The legacy `prices_*` CAGGs (migration 0002) used
   the per-row form; measured on r1 2026-07-02 the divergence is
   ≤ 1.0e-16 relative (40,565 1h-bucket comparisons) — below the
   12-decimal wire truncation, so not worth a re-materialization on
   its own. Migration 0147 (which re-materialized anyway for the
   deterministic open/close tie-break) switched them to the exact
   form, so ALL price CAGGs now comply. Note
   also the 0002 CAGGs materialize a `twap` column that is an
   equal-weight mean (`avg(quote/base)`), NOT time-weighted, and is
   read by nothing — the served TWAP is computed on demand from raw
   trades (`internal/aggregate/twap.go`). Do not start reading that
   column; treat it as dead.

9. **Every up-migration must be additive and old-binary-safe.** The
   previous released binary has to keep running correctly against the
   new schema — a new nullable column, a new table, a new index are
   fine; dropping/renaming a column, narrowing a type, or tightening a
   constraint in the same release the code stops using the old shape
   is not, because the deploy pipeline applies `migrate up` BEFORE the
   new binary installs (`docs/operations/deploy-workflow.md`), and if
   that binary then fails its health probe the rollback restores only
   the **binary** — never the schema (CS-099,
   `docs/audit-2026-06-30/01-cold-system-findings.md`). Breaking
   changes go through the two-release deprecation dance: release N
   adds the new shape alongside the old one (old binary still
   reads/writes the old shape); release N+1 switches the code over;
   release N+2, once nothing depends on it, drops the old shape.
   `down.sql` files exist for local/dev iteration — they are NOT a
   production rollback lever, and this repo does not auto-run
   `migrate down` on a failed deploy (down-migrations can be
   data-destructive, and the pipeline has no way to know whether
   anything already depends on what it would be reverting).

## Amending a shipped migration

Until 2026-09-02 the repo carried two contradictory precedents for this,
both on `main`: `scripts/ci/lint-docs.sh` recorded that even a
comment-only edit to a shipped migration "CANNOT BE CORRECTED" and froze
two wrong headers permanently, while `febf720a` edited nine shipped
`.down.sql` files and refreshed the checksum baseline, citing
`lint-migration-immutability.sh`'s own "deliberately editing → refresh
the baseline" header. Every header nit therefore had two valid
dispositions and nobody could say which was the rule. The rule is:

> **A shipped migration's UP body is immutable; its DOWN body and its
> header COMMENTS may be corrected through the baseline-refresh path
> (`lint-migration-immutability --write`) with a CHANGELOG line; anything
> stored in the database (`COMMENT ON`, defaults) needs a new migration.**

Why each clause holds:

- **UP body immutable.** golang-migrate keys a migration only on its
  `NNNN_` integer and never content-hashes the file, so an environment
  that ran the old text stays permanently diverged from a fresh database
  built from the new text — and nothing complains. That is the whole
  point of the checksum gate.
- **Header comments correctable.** A `--` comment above `BEGIN;` is not
  executed. Editing one cannot make an applied database diverge from a
  fresh one; it can only make the file agree with reality. Freezing a
  comment buys no safety and costs every future reader — the two frozen
  self-citations (`0096` said "0095 up", `0125` said "0124 up") sent
  readers to an unrelated migration for two months. Corrections are
  still VISIBLE: the checksum line moves, so the diff shows it.
- **DOWN body correctable.** A `down.sql` is a local/dev iteration lever
  (rule 9); this repo does not auto-run `migrate down` on a failed
  deploy, and no down in this tree has ever run against production. A
  down that does not faithfully reverse its up is a latent trap, and the
  fix for it is to correct the down, not to ship a new migration whose
  own down would inherit the same bug.
- **Stored strings need a new migration.** `COMMENT ON` text and column
  DEFAULTs live in the DATABASE catalog, not in the file. Editing the
  original migration changes only what a FRESH database gets; the string
  an operator reads through `\d+` on an existing deployment stays wrong
  forever. Re-issuing it in a new migration is the only correction that
  reaches both. Migration 0151 is the worked example.

Mechanics for a correctable edit — all three in ONE commit:

1. Edit the header comment / down body.
2. `./scripts/ci/lint-migration-immutability.sh --write` (the baseline
   moves in the same diff, so the change is reviewable, not silent).
3. A `CHANGELOG.md` line under `## [Unreleased]` saying which migration
   and which claim was wrong.

Not covered by this rule, and still forbidden: renumbering, deleting, or
re-purposing a shipped migration; changing a shipped `up.sql`'s SQL for
ANY reason, including a bug in it (ship a corrective migration instead).

## Conventions

- Statement terminators on their own line; always semicolon-end.
- `CREATE … IF NOT EXISTS` where idempotent; otherwise plain `CREATE`
  so a rerun after manual poking fails loudly.
- Comments above the statement (not inline) and explain the *why*.
- Timestamp columns are `timestamptz`, stored + served in UTC.
- Transactions: each migration runs in its own transaction by default
  (golang-migrate), and there is NO supported way to disable it.
  `-- migrate:no-transaction` is a dbmate/goose directive that
  golang-migrate SILENTLY IGNORES — v4 has no directive parsing and
  executes the file verbatim (corrected 2026-08-04; the old text
  invited an author to write non-transactional DDL believing they had
  isolation they do not have). Anything that genuinely cannot run
  inside a transaction — CREATE INDEX CONCURRENTLY, VACUUM — belongs in
  an operator runbook step, not a migration.
- Do NOT end a migration with an explicit `COMMIT;` followed by more
  DDL. golang-migrate wraps nothing of its own, so atomicity rests
  entirely on Postgres's implicit transaction block — and a `COMMIT;`
  ends it, making the tail non-atomic and the migration only partially
  applicable. Migration 0030 does this and is the corpus's one
  non-atomic up (cold audit 2026-08-04).

## Current migrations

Sequential index of what each migration adds (read the `.up.sql`
header for the full motivation). Update this table when a new
migration lands.

| Number | File | Adds |
| --- | --- | --- |
| 0001 | [`0001_create_trades_hypertable.up.sql`](0001_create_trades_hypertable.up.sql) | Core `trades` hypertable, retention policy, primary indexes |
| 0002 | [`0002_create_price_aggregates.up.sql`](0002_create_price_aggregates.up.sql) | Continuous aggregates (1m/15m/1h/4h/1d/1w/1mo) + refresh + retention. **CAVEAT**: `twap` column is `avg(quote/base)` — arithmetic mean of trade prices, NOT a time-weighted average. True TWAP needs inter-trade durations the CAGG definitions don't capture; computed in Go via `internal/aggregate/twap.go` instead |
| 0003 | [`0003_create_oracle_updates_hypertable.up.sql`](0003_create_oracle_updates_hypertable.up.sql) | `oracle_updates` hypertable for Reflector / Redstone / Band observations + compression + retention |
| 0004 | [`0004_relax_trades_ledger_for_offchain.up.sql`](0004_relax_trades_ledger_for_offchain.up.sql) | Relaxes the `trades.ledger > 0` constraint so off-chain sources (Binance / Kraken / etc) can stamp `ledger = 0` |
| 0005 | [`0005_create_asset_supply_history.up.sql`](0005_create_asset_supply_history.up.sql) | `asset_supply_history` hypertable per ADR-0011 — append-only per-asset supply snapshots backing the F2 fields on `/v1/assets/{id}` |
| 0006 | [`0006_create_discovered_assets.up.sql`](0006_create_discovered_assets.up.sql) | `discovered_assets` table for SEP-41 auto-discovery; every contract emitting a transfer / mint / burn / clawback event lands here for operator triage |
| 0007 | [`0007_create_volatility_baseline.up.sql`](0007_create_volatility_baseline.up.sql) | `volatility_baseline_1m` per-pair statistical baseline per ADR-0019 Phase 2 — robust median + MAD baseline used by the anomaly-freeze policy |
| 0008 | [`0008_add_multi_window_baseline.up.sql`](0008_add_multi_window_baseline.up.sql) | Adds 1d + 7d baseline columns to `volatility_baseline_1m` per ADR-0019 §"Multi-window safeguard against frog-boiling" |
| 0009 | [`0009_create_blend_auctions.up.sql`](0009_create_blend_auctions.up.sql) | `blend_auctions` hypertable — one row per observed Blend auction event (new_auction, etc.) |
| 0010 | [`0010_create_account_observations.up.sql`](0010_create_account_observations.up.sql) | `account_observations` hypertable per ADR-0021 — one row per AccountEntry-delta touching an operator-watched account, backs Algorithm 1 (XLM) reserves |
| 0011 | [`0011_create_trustline_observations.up.sql`](0011_create_trustline_observations.up.sql) | `trustline_observations` hypertable per ADR-0022 — backs Algorithm 2 classic-credit supply: Σ trustline component |
| 0012 | [`0012_create_claimable_observations.up.sql`](0012_create_claimable_observations.up.sql) | `claimable_observations` hypertable per ADR-0022 — backs Algorithm 2: Σ claimable-balance component |
| 0013 | [`0013_create_lp_reserve_observations.up.sql`](0013_create_lp_reserve_observations.up.sql) | `lp_reserve_observations` hypertable per ADR-0022 — backs Algorithm 2: Σ LP-reserve component |
| 0014 | [`0014_create_sac_balance_observations.up.sql`](0014_create_sac_balance_observations.up.sql) | `sac_balance_observations` hypertable per ADR-0022 — backs Algorithm 2: Σ SAC-wrapped contract balance component |
| 0015 | [`0015_create_sep41_supply_events.up.sql`](0015_create_sep41_supply_events.up.sql) | `sep41_supply_events` hypertable per ADR-0023 — backs Algorithm 3 SEP-41 supply: Σ mint − Σ burn − Σ clawback per contract |
| 0016 | [`0016_create_soroswap_pairs.up.sql`](0016_create_soroswap_pairs.up.sql) | Persists the pair_contract → (token0, token1) mapping that the |
| 0017 | [`0017_create_wasm_history.up.sql`](0017_create_wasm_history.up.sql) | WASM history made into a queryable, postgres-resident first-class |
| 0018 | [`0018_create_freeze_events.up.sql`](0018_create_freeze_events.up.sql) | Per ADR-0019. The anomaly engine in `internal/aggregate/freeze` |
| 0019 | [`0019_create_divergence_observations.up.sql`](0019_create_divergence_observations.up.sql) | `internal/divergence/worker.go` periodically compares our VWAP |
| 0020 | [`0020_create_decoder_stats_5m.up.sql`](0020_create_decoder_stats_5m.up.sql) | The dispatcher exposes per-source counters via `dispatcher.Stats()`: |
| 0021 | [`0021_create_tvl_and_mev.up.sql`](0021_create_tvl_and_mev.up.sql) | Two tables that share a migration because they're both populated |
| 0022 | [`0022_create_change_summary_5m.up.sql`](0022_create_change_summary_5m.up.sql) | The single endpoint that powers every multi-window delta strip |
| 0023 | [`0023_create_classic_asset_registry.up.sql`](0023_create_classic_asset_registry.up.sql) | The classic-asset registry. Today every classic-asset trade is in |
| 0024 | [`0024_create_classic_asset_stats_5m.up.sql`](0024_create_classic_asset_stats_5m.up.sql) | Per-asset summary stats refreshed every 5 minutes. Aggregator |
| 0025 | [`0025_create_routers_and_attribution.up.sql`](0025_create_routers_and_attribution.up.sql) | Soroswap router + attribution tables — tracks which router contract emitted each multi-hop swap so attribution is per-router |
| 0026 | [`0026_create_source_contributions_and_sdex_offers.up.sql`](0026_create_source_contributions_and_sdex_offers.up.sql) | `source_contributions` + `sdex_offers` — per-source weight history and per-offer SDEX state for the deep-SDEX feed |
| 0027 | [`0027_platform_v1_schema.up.sql`](0027_platform_v1_schema.up.sql) | Platform v1 schema — accounts / users / sessions / API keys / Stripe subscriptions / dashboard webhook store, the dashboard's authority surface |
| 0028 | [`0028_create_fx_quotes.up.sql`](0028_create_fx_quotes.up.sql) | `fx_quotes` hypertable — long-form persisted ECB / exchangerates / polygon-forex FX history backing `/v1/chart` fiat:fiat past 7d |
| 0029 | [`0029_drop_unused_blend_jsonb_gin_indexes.up.sql`](0029_drop_unused_blend_jsonb_gin_indexes.up.sql) | Drops two unused JSONB GIN indexes on `blend_auctions` — the auction query path uses btree on the typed columns |
| 0030 | [`0030_asset_supply_history_unique_constraint.up.sql`](0030_asset_supply_history_unique_constraint.up.sql) | Promotes `asset_supply_history_asset_ledger_idx` from UNIQUE INDEX → UNIQUE CONSTRAINT (`DROP INDEX` + `ADD CONSTRAINT … UNIQUE (asset_key, ledger_sequence, time)`) so the supply-snapshot writer's `ON CONFLICT` clause matches it on Timescale hypertables. Decompresses chunks + disables compression around the DDL and restores the 0005 compression settings after the swap (F-1261 codex audit-2026-05-13 — mirrors the 0004/trades pattern). F-1205 follow-up |
| 0031 | [`0031_remove_trades_retention.up.sql`](0031_remove_trades_retention.up.sql) | Removes the 90-day retention policy on `trades` and the 30-day retention on `prices_1m` / `prices_15m` — operator wants every raw trade preserved forever (postgres data dir is on a 1.5 TB ZFS volume with room for a decade of raw trades) |
| 0032 | [`0032_seed_soroswap_router.up.sql`](0032_seed_soroswap_router.up.sql) | Pre-seeds `routers` with the operator-vetted Soroswap router contract id (`auto_discovered = false`), so the router-attribution observer can populate the dispatcher's `ContractCallDecoder` match-set at startup |
| 0033 | [`0033_seed_defindex_vaults.up.sql`](0033_seed_defindex_vaults.up.sql) | Pre-seeds `routers` with the three Phase-A DeFindex autocompound vaults (`kind = 'aggregator-vault'`); hand-curated until factory-event vault discovery ships |
| 0034 | [`0034_oracle_price_aggregates.up.sql`](0034_oracle_price_aggregates.up.sql) | Continuous aggregates for `oracle_updates` (7-tier grain set, sister to 0002) — first/last/min/max/count per `(source, asset, quote, bucket)`, preserving per-oracle identity rather than collapsing to a single VWAP |
| 0035 | [`0035_create_source_entry_counts.up.sql`](0035_create_source_entry_counts.up.sql) | `source_entry_counts` — an always-on, per-source running tally of ingested entries (trades + oracle_updates) so `/v1/diagnostics/ingestion` has a coverage number that stays cheap to read even during an all-time backfill |
| 0036 | [`0036_create_pools_per_source_cagg.up.sql`](0036_create_pools_per_source_cagg.up.sql) | `pools_per_source_1h` continuous aggregate — durable 1h-bucket backing for `/v1/pools`, eliminating the cold full-`trades`-scan the handler used to pay (#25) |
| 0037 | [`0037_trades_pair_source_ts_index.up.sql`](0037_trades_pair_source_ts_index.up.sql) | `trades_pair_source_ts_idx (base_asset, quote_asset, source, ts DESC, ledger DESC)` — covering index for `Store.LatestTradePerSource` / `/v1/observations`; turns its `DISTINCT ON (source)` from an O(rows_in_pair) scan+sort into an O(num_sources) skip-scan. On a populated node build it `CONCURRENTLY` by hand first (see the `.up.sql` header) — the in-transaction build would block ingest. #30 |
| 0038 | [`0038_create_cctp_events.up.sql`](0038_create_cctp_events.up.sql) | `cctp_events` hypertable — one row per observed Circle CCTP v2 bridge event (deposit_for_burn / mint_and_withdraw / message_sent / message_received) on Stellar. Promoted typed columns (amount, fee, token, counterparty_domain) + a jsonb `attributes` blob for the event-type-specific remainder. Class=bridge, never VWAP. #40 |
| 0039 | [`0039_create_rozo_events.up.sql`](0039_create_rozo_events.up.sql) | `rozo_events` hypertable — one row per observed Rozo v1 intent-bridge event (payment / flush) on Stellar. Fully typed (amount, destination always present; from_addr/memo payment-only; token flush-only) — no jsonb blob, v1 Payment is simple enough. Class=bridge, never VWAP. #41 |
| 0040 | [`0040_remove_oracle_updates_retention.up.sql`](0040_remove_oracle_updates_retention.up.sql) | Removes the 90-day retention policy on `oracle_updates` (sister to 0031 for `trades`) — every raw oracle observation is now preserved indefinitely. The 0034 CAGGs are unchanged; the migration header documents the per-grain `refresh_continuous_aggregate` calls that re-backfill them over the full raw range. #14 |
| 0041 | [`0041_create_soroban_events.up.sql`](0041_create_soroban_events.up.sql) | `soroban_events` hypertable per ADR-0029 — raw-event landing zone capturing every Soroban contract event the dispatcher routes, with topics + body + op_args stored as raw XDR for future per-source decoder backfills (`INSERT … SELECT` rather than MinIO re-walks). PK leads with `ledger_close_time` (TS103 lesson); compression after 7 days segmented by `contract_id`. |
| 0042 | [`0042_create_comet_liquidity.up.sql`](0042_create_comet_liquidity.up.sql) | `comet_liquidity` hypertable — one row per `(ledger, tx_hash, op_index, event_kind, token)` covering Balancer-v1 `join_pool` / `exit_pool` / `deposit` / `withdraw` events under the `POOL` namespace. join/exit are loop-emitted per token; PK includes `token` so per-token rows don't collide. Direction column ('add'/'remove') keeps Sum(amount) ordered. PK leads with `ledger_close_time` (TS103); compression after 7 days segmented by `contract_id`. Historical fill via `soroban_events` (0041). #26 |
| 0043 | [`0043_create_soroswap_skim_events.up.sql`](0043_create_soroswap_skim_events.up.sql) | `soroswap_skim_events` hypertable — one row per observed Soroswap pair-contract `skim` event (Uniswap-v2-style claim of excess pool balance above reserves). Closes the "every emitted topic gets classified" gap — previously skim was declared in `events.go` but unreachable through `classify()`. Not a trade; never feeds VWAP. PK leads with `ledger_close_time` (TS103); compression after 7 days segmented by `contract_id`. Historical fill via `soroban_events` (0041). #28 |
| 0044 | [`0044_create_phoenix_liquidity_and_stake.up.sql`](0044_create_phoenix_liquidity_and_stake.up.sql) | `phoenix_liquidity` + `phoenix_stake_events` hypertables — covers Phoenix's 4 non-swap event topics (`provide_liquidity`, `withdraw_liquidity`, `bond`, `unbond`). Like Phoenix swap, each is emitted as N field-events that the decoder correlates and rebuilds. NULL token addresses on withdraw rows (contract doesn't re-emit token identities — downstream joins to the most recent provide row); NULL shares_amount on provide rows (LP-share mint goes through SEP-41). PK leads with `ledger_close_time` (TS103); compression after 7 days segmented by `contract_id`. Historical fill via `soroban_events` (0041). #27 |
| 0045 | [`0045_create_blend_money_market.up.sql`](0045_create_blend_money_market.up.sql) | `blend_positions` + `blend_emissions` + `blend_admin` hypertables — covers the 18 Blend event topics that were declared in `events.go` but never matched by the auction-only `classify()`. positions: supply / withdraw / supply_collateral / withdraw_collateral / borrow / repay / flash_loan. emissions: gulp / claim / reserve_emission_update / gulp_emissions / bad_debt / defaulted_debt. admin: set_admin / update_pool / queue_set_reserve / cancel_set_reserve / set_reserve / set_status / deploy. Closes the "every emitted topic gets classified" gap that the user-reported defindex-vs-blend volume discrepancy surfaced (project_every_event_principle, 2026-05-25). PK leads with `ledger_close_time` (TS103); compression after 7 days segmented by `contract_id`. Historical fill via `soroban_events` (0041). #25 |
| 0046 | [`0046_cursors_add_first_ledger.up.sql`](0046_cursors_add_first_ledger.up.sql) | The density-coverage projection in /v1/diagnostics/ingestion unions |
| 0047 | [`0047_sep41_transfers.up.sql`](0047_sep41_transfers.up.sql) | Materialises every SEP-41 `transfer` event (and the `approve` / |
| 0048 | [`0048_source_coverage_snapshots.up.sql`](0048_source_coverage_snapshots.up.sql) | ADR-0031: source_coverage_snapshots is the data-derived coverage |
| 0049 | [`0049_create_soroswap_router_swaps.up.sql`](0049_create_soroswap_router_swaps.up.sql) | Captures one row per Soroswap router invocation (= one call to |
| 0050 | [`0050_create_defindex_flows.up.sql`](0050_create_defindex_flows.up.sql) | One row per decoded DeFindex protocol flow event. Captures both |
| 0051 | [`0051_ledger_ingest_log.up.sql`](0051_ledger_ingest_log.up.sql) | One row per ledger we have fully processed, written POST-persist |
| 0052 | [`0052_completeness_snapshots.up.sql`](0052_completeness_snapshots.up.sql) | One row per source carrying the completeness WATERMARK: the highest |
| 0053 | [`0053_blend_pk_granularity.up.sql`](0053_blend_pk_granularity.up.sql) | blend_positions / blend_emissions / blend_admin keyed rows on |
| 0054 | [`0054_blend_positions_event_index.up.sql`](0054_blend_positions_event_index.up.sql) | (asset, user_address) to event_index. |
| 0055 | [`0055_defindex_flows_event_index.up.sql`](0055_defindex_flows_event_index.up.sql) | defindex_flows keyed on (…, op_index, layer) with no per-event discriminator, |
| 0056 | [`0056_soroswap_router_swaps_call_sig.up.sql`](0056_soroswap_router_swaps_call_sig.up.sql) | The PK was (ledger_close_time, ledger, tx_hash, op_index) — no per-call |
| 0057 | [`0057_sep41_supply_events_event_index.up.sql`](0057_sep41_supply_events_event_index.up.sql) | The PK was (contract_id, ledger, tx_hash, op_index, observed_at) — no |
| 0058 | [`0058_blend_auctions_event_index.up.sql`](0058_blend_auctions_event_index.up.sql) | blend_auctions was skipped by the 0053 Blend coarse-PK fix (which covered |
| 0059 | [`0059_comet_liquidity_event_index.up.sql`](0059_comet_liquidity_event_index.up.sql) | The PK was (ledger_close_time, contract_id, ledger, tx_hash, op_index, |
| 0060 | [`0060_phoenix_event_index.up.sql`](0060_phoenix_event_index.up.sql) | (F-1324). |
| 0061 | [`0061_protocol_contracts.up.sql`](0061_protocol_contracts.up.sql) | ADR-0035 (factory-anchored contract gating). Factory-anchored Soroban |
| 0062 | [`0062_widen_hot_chunk_intervals.up.sql`](0062_widen_hot_chunk_intervals.up.sql) | that were created with a 1-day interval. |
| 0063 | [`0063_create_blend_backstop_events.up.sql`](0063_create_blend_backstop_events.up.sql) | One row per observed Blend Backstop contract event on Stellar. The |
| 0064 | [`0064_create_dex_volume_cagg.up.sql`](0064_create_dex_volume_cagg.up.sql) | Backs the DEX/AMM protocol-page bespoke analytics block |
| 0065 | [`0065_magic_link_token_attempts.up.sql`](0065_magic_link_token_attempts.up.sql) | Backs the brute-force guard on the email-code sign-in path |
| 0066 | [`0066_create_supply_cagg.up.sql`](0066_create_supply_cagg.up.sql) | Daily last-known supply per asset_key — the supply leg of crypto |
| 0067 | [`0067_mev_arbitrage_dedup.up.sql`](0067_mev_arbitrage_dedup.up.sql) | The v1 MEV detector (internal/aggregate/mev) flags ATOMIC ARBITRAGE: |
| 0068 | [`0068_create_source_volume_cagg.up.sql`](0068_create_source_volume_cagg.up.sql) | Backs the source page's activity chart (24h + 7d windows of per-hour |
| 0069 | [`0069_source_volume_realtime_agg.up.sql`](0069_source_volume_realtime_agg.up.sql) | Migration 0068 created the CAGG with TimescaleDB's default |
| 0070 | [`0070_cctp_mint_and_forward_check.up.sql`](0070_cctp_mint_and_forward_check.up.sql) | Board #31 follow-up: the CctpForwarder's fifth event decodes as of |
| 0071 | [`0071_create_usage_daily.up.sql`](0071_create_usage_daily.up.sql) | Backs the dashboard's per-endpoint usage analytics |
| 0072 | [`0072_align_router_registry_name.up.sql`](0072_align_router_registry_name.up.sql) | source name used by trade attribution. |
| 0073 | [`0073_api_key_scopes.up.sql`](0073_api_key_scopes.up.sql) | Adds the coarse route-family scope list a key can be confined to |
| 0074 | [`0074_mev_new_kinds.up.sql`](0074_mev_new_kinds.up.sql) | The MEV worker now ships the detectors 0067 left reserved: |
| 0076 | [`0076_pools_per_source_realtime_agg.up.sql`](0076_pools_per_source_realtime_agg.up.sql) | Migration 0036 created the CAGG with TimescaleDB's default |
| 0080 | [`0080_create_price_alerts.up.sql`](0080_create_price_alerts.up.sql) | `price_alerts` table — customer-configurable price-threshold alerts (BACKLOG #60 / RFP §6). Owner-scoped by `account_id` (FK to `accounts`, ON DELETE CASCADE, mirrors `customer_webhooks`). Columns: `base_asset`/`quote_asset` (canonical wire form), `condition` (above/below), `threshold` (NUMERIC per ADR-0003), `cooldown_seconds`, `enabled`, `last_fired_at`. The aggregator's price-alert evaluator (`internal/pricealerts`) sweeps enabled rows against the latest closed 1m VWAP and enqueues account-scoped `price.alert` deliveries into the existing `webhook_deliveries` queue. Additive + old-binary-safe (rule 9). |
| 0081 | [`0081_create_twap_aggregates.up.sql`](0081_create_twap_aggregates.up.sql) | /v1/chart?price_type=twap). |
| 0082 | [`0082_create_status_notices.up.sql`](0082_create_status_notices.up.sql) | The "incident tooling" half of the admin Phase 1.5 surface |
| 0083 | [`0083_sep41_transfers_ledger_idx.up.sql`](0083_sep41_transfers_ledger_idx.up.sql) | sep41_transfers has no ledger-led index (unlike sep41_supply_events, |
| 0085 | [`0085_create_sep41_supply_rollup.up.sql`](0085_create_sep41_supply_rollup.up.sql) | mint/burn/clawback checkpoint for the SEP-41 Algorithm-3 supply read. |
| 0086 | [`0086_create_protocol_events_24h_rollup.up.sql`](0086_create_protocol_events_24h_rollup.up.sql) | Backs the `events_24h` column on GET /v1/protocols and |
| 0087 | [`0087_create_asset_volume_24h_rollup.up.sql`](0087_create_asset_volume_24h_rollup.up.sql) | Backs the `volume_24h_usd` column on the GET /v1/assets listing |
| 0088 | [`0088_sep41_supply_rollup_genesis_baseline.up.sql`](0088_sep41_supply_rollup_genesis_baseline.up.sql) | Why this exists (incident 2026-07-06 follow-up). The SEP-41 Algorithm-3 |
| 0089 | [`0089_create_aquarius_liquidity_reserves.up.sql`](0089_create_aquarius_liquidity_reserves.up.sql) | Aquarius is the highest-volume Soroban AMM but until now the |
| 0090 | [`0090_create_sorocredit_credit_tables.up.sql`](0090_create_sorocredit_credit_tables.up.sql) | New protocol domain (Stellar Index had NO lending/credit-position |
| 0091 | [`0091_aquarius_liquidity_pool_tokens_idx.up.sql`](0091_aquarius_liquidity_pool_tokens_idx.up.sql) | Partial covering index `(contract_id, token_index, ledger_close_time DESC) WHERE token IS NOT NULL` on `aquarius_liquidity` — the `PoolTokens` resolver's `DISTINCT ON (contract_id, token_index) … ORDER BY …` had no matching index prefix, forcing a full materialize-and-sort of a forever-retained hypertable inside the `/v1/protocols/{name}` request path (review 2026-07-08 finding 2; same class as the 2026-06-19 protocol-detail runaway query). Additive, index-only. |
| 0092 | [`0092_cctp_governance_events_check.up.sql`](0092_cctp_governance_events_check.up.sql) | Extends `cctp_events_event_type_check` 5 → 10 values for the governance/admin topics (ownership_transfer, ownership_transfer_completed, admin_changed, remote_token_messenger_added, token_pair_linked) — #89b admin-topic audit. Follows migration 0070's precedent; additive + old-binary-safe (rule 9). |
| 0093 | [`0093_create_nonstandard_decimals_assets.up.sql`](0093_create_nonstandard_decimals_assets.up.sql) | `nonstandard_decimals_assets` table — the read-side control table backing the dex-nonstandard-decimals READ-TIME serving guard (confirmed production bug 2026-07-08: token `CC2RB…` declares `decimals()=9`, skewing its aquarius pair's served price 100x). Small plain table (not a hypertable), PK `asset`, `CHECK (decimals <> 7)`. `internal/decimalsguard` (aggregator) upserts on confirmation; `internal/api/v1.NonstandardDecimalsCache` (API, ~60s refresh) mirrors it and declines `/v1/price`, `/v1/vwap`, `/v1/history`, `/v1/ohlc` for any pair with a listed leg. Additive + old-binary-safe (rule 9). |
| 0094 | [`0094_cctp_full_governance_events_check.up.sql`](0094_cctp_full_governance_events_check.up.sql) | Extends `cctp_events_event_type_check` 10 → 26 values, closing the full topic census (admin_change_started, attester_enabled, attester_manager_updated, denylisted, denylister_changed, fee_recipient_set, max_message_body_size_updated, min_fee_controller_set, pauser_changed, rescuer_changed, set_burn_limit_per_message, set_token_controller, signature_threshold_updated, swap_minter_config_set, token_decimal_config_added, un_denylisted) — #89c full-census audit, retires the docs/protocols/cctp.md known-gap note. Follows migration 0092's precedent; additive + old-binary-safe (rule 9). |
| 0095 | [`0095_blend_backstop_rw_zone_events_check.up.sql`](0095_blend_backstop_rw_zone_events_check.up.sql) | blend_backstop_events.event_kind. |
| 0096 | [`0096_create_blend_emitter_events.up.sql`](0096_create_blend_emitter_events.up.sql) | `blend_emitter_events` hypertable — one row per observed event from the Blend **Emitter** contract (protocol-emissions plumbing, distinct from `blend` pools and `blend_backstop`): `distribute` (recurring BLND emission to a backstop), `drop` (one-shot airdrop, variable-length recipient list — fanned out one row per recipient via a `recipient_index` discriminator, same lesson as `aquarius_reserves`' `token_index`), `q_swap` / `swap` (backstop-swap timelock queue/execute). Gated on contract identity (`distribute` collides with `blend_backstop`'s own `distribute` topic). No published price, never VWAP. PK leads with `ledger_close_time` (TS103); compression after 7 days segmented by `(contract_id, event_kind)`. `BackfillSafe=false` pending a wasm-history audit across the Emitter's 3 observed WASM uploads. See `internal/sources/blend_emitter/README.md`. |
| 0097 | [`0097_blend_v1_pool_factory_events_check.up.sql`](0097_blend_v1_pool_factory_events_check.up.sql) | blend_emissions.event_kind / blend_admin.event_kind. |
| 0098 | [`0098_phoenix_stake_reward_events.up.sql`](0098_phoenix_stake_reward_events.up.sql) | phoenix_stake_events.action; make user_addr + amount nullable. |
| 0099 | [`0099_create_aquarius_rewards_events.up.sql`](0099_create_aquarius_rewards_events.up.sql) | `aquarius_rewards_events` hypertable — the Aquarius rewards-gauge / liquidity-mining surface (ROADMAP #89 topic census, 2026-07-10): 12 event kinds (`pool_state`, `claim_reward`, `set_rewards_config`, `position_update`, bare `deposit`, `claim_fees`, `rewards_gauge_claim`, bare `claim`, `rewards_gauge_schedule_reward`, `set_rewards_state`, `rewards_gauge_add`, plus the router-side `config_rewards`) in one table with an `event_kind` discriminator + promoted `user_address`/`amount` columns + jsonb `attributes` remainder (blend_admin's 0045 pattern; i128s inside jsonb are decimal strings per ADR-0003). Pool-scoped kinds gate on the registered-pool set; `config_rewards` on the router trust root. Never VWAP. PK leads with `ledger_close_time` (TS103) and includes `event_index` from day one (F-1324 lesson). Historical fill via `projector-replay -source aquarius`. See `internal/sources/aquarius/README.md`. |
| 0100 | [`0100_create_aquarius_admin.up.sql`](0100_create_aquarius_admin.up.sql) | `aquarius_admin` hypertable — the Aquarius governance/upgrade surface (same #89 census): 8 event kinds (`apply_upgrade`, `commit_upgrade`, `set_privileged_addrs`, `apply_transfer_ownership`, `commit_transfer_ownership`, `enable_emergency_mode`, `disable_emergency_mode`, `pool_gauge_switch_token`) with `event_kind` + promoted `admin`/`target` columns + jsonb `attributes` (blend_admin pattern; `event_index` in the PK from the start). Gated on the canonical router trust root only — the flagged parallel deployment + unidentified sibling contracts fail-closed (visible ADR-0033 recognition gap, not silent attribution). Never VWAP. |
| 0101 | [`0101_soroswap_router_swaps_call_path.up.sql`](0101_soroswap_router_swaps_call_path.up.sql) | Adds `call_path text[]` / `call_depth smallint` / `call_kind text` to `soroswap_router_swaps` (ROADMAP #11): the tx auth-tree position of each captured router call — `call_path` is the ordered contract chain from the top-level invocation down to the router (always ends in `contract_id`), `call_kind` is `top_level`/`sub_invocation`. NULLable (pre-#11 rows carry NULL); all-or-none enforced by a CHECK that also ties kind↔depth↔cardinality. NOT in the PK and NOT in `call_sig` (auth-tree duplicates must still dedup, 0056). Partial-position columns backfill via DELETE + `ch-rebuild -sources soroswap-router -contract-calls -write -from 50746272` (queued r1 heavy job — see the migration header + `internal/sources/soroswap_router/README.md`). Additive + old-binary-safe (rule 9). |
| 0102 | [`0102_create_sac_balance_seed_provenance.up.sql`](0102_create_sac_balance_seed_provenance.up.sql) | `sac_balance_seed_provenance` — one row per watched SAC-wrapper contract auditing the most recent `supply seed-sac-balances` pass (`source` = `current_state` \| `full_history`, `holders_seeded`, `min_ledger_seen`/`max_ledger_seen`, `seeded_at`). PHO/BLND/EURC/KALE follow-up (incident 2026-07-06 / ROADMAP #14): `current_state` reads `stellar.ledger_entries_current`, which has a coverage floor (~ledger 62,000,000 — the MV never processed ch-backfilled rows inserted before it existed); the new `full_history` source (`clickhouse.StreamSACBalanceSeedsFullHistory`) reads `stellar.ledger_entry_changes` directly (complete to genesis, ADR-0034) to recover dormant pool-held SAC balances the floor hides. Pure audit trail — not consumed by the supply computation. Additive + old-binary-safe. |
| 0103 | [`0103_discovery_oracle_broadening.up.sql`](0103_discovery_oracle_broadening.up.sql) | Broadens `discovered_assets` (0006) for the generic-oracle discovery extension (docs/architecture/generic-oracle-sep-onboarding.md §3(b)): adds `discovery_kind` (`sep41` \| `oracle_event` \| `oracle_call`, DEFAULT `sep41`) and widens the `first_seen_event` CHECK to admit the oracle-suggestive symbol set (`price`, `oracle`/`Oracle`/`ORACLE`, `REDSTONE`, `StandardReference`, `relay`, …) alongside the original four SEP-41 values. Same table, same sighting-only discipline — no parallel storage surface. Additive + old-binary-safe (DROP+re-ADD CHECK, same idiom as 0070/0092/0094). |
| 0104 | [`0104_seed_soroswap_aggregator_exec.up.sql`](0104_seed_soroswap_aggregator_exec.up.sql) | Seeds the `routers` registry (0025) with the ONE evidence-verified aggregator wrapper contract (`CD45PQFH…`, real mainnet `exec()` call at ledger 62,029,020 — see `internal/sources/soroswap_router/real_bytes_test.go`), `auto_discovered=true`, fail-closed on everything unverified. Enables call-path-aware `routed_via` attribution. Additive + old-binary-safe. |
| 0105 | [`0105_create_classic_movements.up.sql`](0105_create_classic_movements.up.sql) | `classic_movements` hypertable (ADR-0047 Phase 1) — pre-P23 classic-asset movements reconstructed from the ClickHouse raw lake (`stellar.operations`/`operation_results`), never Horizon or a MinIO walk. `movement_kind` (10 values) + `provenance` (`classic_derived` / reserved `cap67_event`) discriminators admit all ten D1 kinds up front so Phases 2-4 need no schema churn; Phase 1's `internal/sources/classicmovements` decoder writes `payment`/`create_account` only. Natural key `(ledger, tx_hash, op_index, leg_index)`, `asset` as canonical asset_id string, `amount` NUMERIC. Historical-only — the writer (`stellarindex-ops classic-movements-backfill`) hard-clamps below the P23 boundary (ledger 58762517); never live-wired. Write-path only (no serving endpoint yet); retention deliberately deferred pending the first real backfill's row-size measurement. Not a live gap-detector target (see `internal/storage/timescale/gap_targets_test.go`'s `excludedFromGapDetector` entry) — coverage is a static recognition-switch test instead (ADR-0047 D4.2). **SUPERSEDED by [ADR-0048](../docs/adr/0048-serve-by-query-shape.md) D2 (2026-07-10):** this table stays applied but is now intentionally **UNPOPULATED** — the pre-P23 movement archive moved to ClickHouse-native `stellar.account_movements` (`deploy/clickhouse/tier1_schema.sql`, `internal/storage/clickhouse/account_movements.go`), feed-shaped (two rows per movement, direction discriminator). `stellarindex-ops classic-movements-backfill` no longer opens a Postgres connection at all. `internal/storage/timescale/classic_movements.go` (the writer this migration backs) is kept in-tree for reference; a future cleanup migration drops this table once the ClickHouse path is proven. |
| 0106 | [`0106_sep41_transfers_address_idx.up.sql`](0106_sep41_transfers_address_idx.up.sql) | Two partial indexes on `sep41_transfers` — `(from_addr, ledger DESC) WHERE from_addr IS NOT NULL` and the `to_addr` mirror — backing `timescale.Store.ListSEP41TransfersByAddress` (ADR-0048 D5): an address-scoped, cross-contract read the existing `sep41_transfers_contract_{from,to}_idx` (0047, `contract_id`-prefixed) and `sep41_transfers_ledger_idx` (0083, ledger-only) can't serve without a full scan. Backs the Postgres "recent tail" half of `GET /v1/accounts/{g}/movements`'s merge with ClickHouse's `stellar.account_movements`. `IF NOT EXISTS`, no `CONCURRENTLY` — same "applied by hand on r1 ahead of the migration run" convention as 0083. |
| 0107 | [`0107_positions_view_user_indexes.up.sql`](0107_positions_view_user_indexes.up.sql) | Two user-leading partial indexes backing `GET /v1/accounts/{g}/positions` (the "DeFi positions" view): `blend_positions (user_address, ledger_close_time DESC)` and `blend_backstop_events (user_address, ledger_close_time DESC) WHERE user_address IS NOT NULL` — neither table had a user-leading index (`blend_positions_pool_user_asset_idx` is pool-leading, 0053; blend_backstop_events had none at all, 0063), so a per-user fold across every pool would seq-scan. The other four tables the positions fold reads (`phoenix_stake_events`, `defindex_flows`, `credit_positions`, `aquarius_rewards_events`) already carry a suitable user-leading index from their own migrations. `IF NOT EXISTS`, no `CONCURRENTLY` — same convention as 0106. |
| 0108 | [`0108_completeness_lake_complete.up.sql`](0108_completeness_lake_complete.up.sql) | (notes/DECISION-genesis-complete-verdict-2026-07-16.md, Option B). |
| 0109 | [`0109_derive_generation_guard.up.sql`](0109_derive_generation_guard.up.sql) | derived-value tables (trades, oracle_updates, asset_supply_history) |
| 0110 | [`0110_derive_generation_guard_protocol.up.sql`](0110_derive_generation_guard_protocol.up.sql) | 3 CORE served-tier tables to the PROTOCOL projector tables. Adds |
| 0111 | [`0111_observation_intra_ledger_seq.up.sql`](0111_observation_intra_ledger_seq.up.sql) | hypertables so the FINAL intra-ledger change deterministically wins the |
| 0112 | [`0112_cctp_rozo_event_index.up.sql`](0112_cctp_rozo_event_index.up.sql) | (audit-2026-07-16 C2-13a). |
| 0113 | [`0113_drop_classic_movements.up.sql`](0113_drop_classic_movements.up.sql) | **Drops the dead `classic_movements` hypertable** (0105) — the cleanup migration the 0105 row promised (audit-2026-07-16 C2-18 / DAT-03). ADR-0048 D2 (2026-07-10) moved the pre-P23 classic-movement archive to ClickHouse-native `stellar.account_movements`; since then `stellarindex-ops classic-movements-backfill` writes ONLY to ClickHouse and opens no Postgres connection, so this table had no live writer or reader and was intentionally UNPOPULATED. The now-caller-less Go store (`internal/storage/timescale/classic_movements.go`) is removed in the same change. **DESTRUCTIVE + operator-reviewed — applies post-Phase-0 only.** No data is lost (table was empty by design); `.down.sql` recreates the exact 0105 schema (table + hypertable + indexes + compression policy) for reversibility. `DROP TABLE IF EXISTS`, no CASCADE (no CAGG/view/FK references it). |
| 0114 | [`0114_soroban_events_topics_overflow.up.sql`](0114_soroban_events_topics_overflow.up.sql) | (audit-2026-07-16 C2-11 — the >4-topic decode-loss). |
| 0115 | [`0115_ohlc_extremes_notional_floor.up.sql`](0115_ohlc_extremes_notional_floor.up.sql) | **Notional floor on the OHLC extremes of the seven `prices_*` CAGGs** (audit B11-F1, `docs/operations/finding-dust-trades-set-chart-extremes.md`). `open/high/low/close` become `COALESCE(agg(price) FILTER (WHERE usd_volume >= 0.01), agg(price))` so an economically-meaningless fill can no longer set a bar's extreme — production served XLM/USD low `0.1333` (= 1/7.5) for the 2026-07-17 06:00 bar off ONE 2↔15-stroop path-payment remainder worth $0.00000027, against a real low of `0.1822`. VWAP/TWAP/volume/trade_count stay unfiltered (dust is still a trade that happened); the COALESCE fallback keeps an all-dust bucket reporting an extreme rather than NULL. Everything else is preserved verbatim: columns + order, per-grain refresh policies, `_pair_bucket_idx` indexes, `materialized_only` (captured and re-applied), and NO retention (0031/ADR-0034). `twap_1h`/`twap_1d` (0081) are hierarchical over `prices_1m` so they are dropped and recreated unchanged. The serve-layer 2×-VWAP band (`combinedOutlierBandRatio`) is removed in the same change — the floor subsumes it. **⚠ DEPLOY REQUIRES A FULL OPERATOR RE-MATERIALIZATION of all nine views (~1.1 TB, hours); the migration is `WITH NO DATA` and does NOT do it.** |
| 0116 | [`0116_completeness_target_floors.up.sql`](0116_completeness_target_floors.up.sql) | been verified from, so a bottom-edge truncation stops erasing its own |
| 0117 | [`0117_asset_supply_history_sac_wrapped.up.sql`](0117_asset_supply_history_sac_wrapped.up.sql) | Adds nullable `sac_wrapped_stroops NUMERIC` to `asset_supply_history` — Algorithm 2's `SACWrapped` component, broken out of the folded `total_supply` so the supply cross-check can run the escrow-vs-minted leg (`SACWrapped <= sac_total`) that audit E4/N-F3(b) identified as structurally impossible without it. `ClassicComputer.Compute` populates it; `CrossCheckSubsetBound` reads it via `LatestSupply` and now reports two bounds instead of one. **NULL is load-bearing** — it is the CS-087 "unchecked" state (a pre-0117 row, or a non-classic algorithm with no such component) and must NOT be defaulted to 0, because `0 <= sac_total` holds vacuously and would publish a green check that verified nothing. No CHECK constraint by design (see the header): adding one to a compressed hypertable needs 0101's decompress-every-chunk dance, and non-negativity is already enforced by `supply.validateClassicComponents` plus an explicit guard in `Store.InsertSupply`. No backfill possible (`SACWrapped` is not recoverable from `total_supply`); the column self-populates from the next aggregator refresh. Additive + old-binary-safe (rule 9). |
| 0118 | [`0118_stripe_event_dead_letter.up.sql`](0118_stripe_event_dead_letter.up.sql) | Adds nullable `dead_lettered_at` / `dead_letter_reason` / `dead_letter_resolved_at` to `stripe_event_log` — the durable "customer paid, nothing provisioned" state (audit-2026-07-23 C3-016). Both terminal webhook failures (a paid session for an identifier holding no keys; every per-key upgrade failing) previously ended at one log line + `processed_at` + 200, and the processed-mark's `SET processed_at = now(), error = NULL` **erased** the error the upgrade loop had just recorded. `error IS NOT NULL` can't carry the state: it is also set on transient still-retrying failures and is cleared by that same mark. Open set (= the alert query) is `dead_lettered_at IS NOT NULL AND dead_letter_resolved_at IS NULL`, backed by a partial index; a later delivery that finally provisions stamps `dead_letter_resolved_at` and closes it. Handler leaves `processed_at` NULL on a dead-letter so a manual Stripe re-send re-runs the work (the F-1322 reprocessable contract). Additive + old-binary-safe (rule 9). |
| 0119 | [`0119_freeze_events_lifecycle.up.sql`](0119_freeze_events_lifecycle.up.sql) | Adds nullable `hold_until` / `extensions_used` / `escalated` / `corroborated` to `freeze_events` — a durable home for the ADR-0019 freeze LIFECYCLE, so the extension ladder survives losing Redis. Pre-0119 the ladder lived only in the aggregator's memory and in the Redis marker's JSON, and `internal/aggregate/orchestrator/phase2_freeze.go` reads a MISSING marker under a live freeze as the ADR-0019 operator force-unfreeze — so a Redis flush did not merely forget the ladder, it **released every live freeze**, including ones that had climbed the full 2-hour ladder to `escalated` ("stays active until manual unfreeze") and already paged a human; a restart after the flush instead re-froze from `extensions_used=0`, restarting the escalation clock. `freeze.Writer.MarkHold` now mirrors the ladder on every transition and `LoadState` falls back to it on a marker miss, bounded by `hold_until + grace` so a long-dead aggregator cannot resurrect a stale freeze; `recovered_at IS NOT NULL` keeps `stellarindex-ops freeze-unfreeze` authoritative as the override. `fired_at` is deliberately NOT duplicated — `frozen_at` already is it. Additive + old-binary-safe (rule 9); ADD COLUMN nullable on the compressed hypertable rewrites no chunk, and pre-0119 rows read NULL = "no durable ladder" and degrade to the old behaviour. |
| 0120 | [`0120_intra_ledger_seq_walk_version.up.sql`](0120_intra_ledger_seq_walk_version.up.sql) | **Comment-only AMENDMENT to migration 0111's `intra_ledger_seq` contract** (audit-2026-07-23 C2-032 / C2-023 / C2-040 / R-A01-1). No column added, dropped or rewritten; no row touched. 0111's column comments — which an operator reads straight off the live schema with `\d+ account_observations` — described a PER-TRANSACTION walk (fee changes, tx-changes-before, operations, tx-changes-after) and claimed it matched `entry_change_reader.go`. Both were false: stellar-core commits a ledger in LEDGER-WIDE PHASES (all fees, then all apply-phase meta, then post-apply refunds), which is what the SDK's `ingest.LedgerChangeReader` state machine reproduces. The comments now state the real order plus the invariant a corrective re-derive depends on. Additive by construction + old-binary-safe (rule 9). |
| 0121 | [`0121_stripe_event_claim.up.sql`](0121_stripe_event_claim.up.sql) | Adds nullable `claimed_at` + a partial in-flight index to `stripe_event_log` — the durable CLAIM that stops two concurrent deliveries of one paid Stripe event from both being processed (audit-2026-07-23 C3-039). F-1322 made a row with `processed_at IS NULL` reprocessable (correctly: a first delivery that failed used to be dup-acked forever), which removed the only thing arbitrating between two LIVE deliveries — `processed_at` is stamped at the END of the handler's work, so during the work both a fresh insert and an existing-unfinished read returned nil and both callers provisioned. `AppendStripeEvent` now runs the claim inside a tx under `pg_advisory_xact_lock(hashtext('stripe_event:'||id))` (the 'apikey:'/'webhook:'/'pricealert:' pattern) and returns `ErrEventInFlight` → HTTP 409, deliberately NOT a 200 dup-ack, because acking would drop the event out of Stripe's retry queue while the in-flight processor may still die. The claim is RELEASED by `MarkStripeEventFailed`/`MarkStripeEventDeadLettered` (so a retry or an operator re-send re-claims at once) and superseded by `processed_at` on success; a claim older than the Go-side lease (`postgresstore.stripeEventClaimLease`, 5 min) is re-claimable, which is what recovers a SIGKILLed processor. Additive + old-binary-safe (rule 9). |
| 0122 | [`0122_login_code_lockouts.up.sql`](0122_login_code_lockouts.up.sql) | New `login_code_lockouts` table — a DURABLE per-email failed-verify counter for the dashboard 6-digit email-code sign-in (audit-2026-07-23 C3-032). `magic_link_tokens.attempts` is durable but per-MINT (a new `/v1/auth/login` starts at 0), and the only thing bounding re-mints was `auth.RedisLoginThrottle` — Redis-backed, so a FLUSHALL, a fail-over or a restart cleared it. Standing budget was ~25 code guesses/hour/email indefinitely ≈ 0.22 probability over a year of grinding one address. Policy lives in Go (`dashboardauth.maxDurableCodeFailures` = 10 per 24 h, 24 h lockout ≈ 3.7e-3/year); the row carries `failed_count`/`window_started_at`/`locked_until`. Gates the CODE path only — the magic LINK in the same email keeps working, so a long lockout costs no availability and cannot be used to lock a victim out of their own account. Cleared on any successful sign-in (proof of ownership retires the suspicion). **Retention:** the table's key is ATTACKER-CHOSEN — `/v1/auth/verify-code` is unauthenticated, so a wrong guess against a synthetic address inserts a row no successful sign-in can ever clear (nobody owns it), bounded only by the anonymous rate limit ≈ a slow remote table-fill on a disk-fixed host. `internal/logincodereaper` (hourly, 48 h, live locks exempt at any age) bounds it, over the `login_code_lockouts_updated_at_idx` index; retention deliberately exceeds the 24 h counting window so a sweep can never truncate a live window. Row count is published on `stellarindex_login_code_lockout_rows` so growth is visible before the disk page. Additive (new table) + old-binary-safe (rule 9). |
| 0123 | [`0123_trades_account_ts_indexes.up.sql`](0123_trades_account_ts_indexes.up.sql) | The per-address trades endpoint reads |
| 0124 | [`0124_freeze_reason_other.up.sql`](0124_freeze_reason_other.up.sql) | mapFreezeReason (internal/storage/timescale/freeze_events.go) used to |
| 0125 | [`0125_projection_dirty_windows.up.sql`](0125_projection_dirty_windows.up.sql) | the completeness verifier is forced to re-reconcile it (the 2026-07-31 |
| 0126 | [`0126_twap_sample_count.up.sql`](0126_twap_sample_count.up.sql) | **Adds `sample_count` (per-direction MINUTE COVERAGE) to the `twap_1h` / `twap_1d` CAGGs** (audit finding M-B; MNY-06 on the TWAP surface). SDEX stores each market in BOTH orientations, and `TWAPPointsInRange` folded the two directions with a TRADE-COUNT-weighted mean of `{twap, 1.0/twap_flipped}` — but 0081's twap is `avg(prices_1m.twap)`, equal per elapsed MINUTE, so the only correct merge weight is each direction's minute coverage (`count(*)` of contributing `prices_1m` buckets), which the CAGGs did not store; count-weighting was exact only when trade count tracked coverage and wrong by an unbounded factor otherwise, and equal-weighting regresses the healthy case. A CAGG SELECT can't be ALTERed, so the two hierarchical TWAP views are DROP+CREATEd with ONE appended column; column list/order, `time_bucket` grouping, `_pair_bucket_idx` indexes, policies, `WITH NO DATA` and NO-retention are otherwise verbatim from 0081/0115. Unlike 0115 the seven `prices_*` views are UNTOUCHED — only the two small TWAP roll-ups over `prices_1m` are recreated. The serve-layer fold moves into Go (`aggregates.go::combineDirTWAP`, exact `big.Rat`, no `1.0/twap` rounding). Old-binary-safe (rule 9): the recreated views are a strict column SUPERSET, so the previous binary's named-column read keeps working (inline `-- migration-compat:ok` on the drops). **⚠ DEPLOY: `WITH NO DATA` — after applying, the operator re-materializes `CALL refresh_continuous_aggregate('twap_1h', NULL, now());` then `('twap_1d', NULL, now());` (after `prices_1m` is whole; minutes, not the 0115 hours).** |
| 0127 | [`0127_create_soroswap_liquidity.up.sql`](0127_create_soroswap_liquidity.up.sql) | One row per observed Soroswap pair-contract `deposit` \| `withdraw` |
| 0128 | [`0128_create_aquarius_reserves_sync.up.sql`](0128_create_aquarius_reserves_sync.up.sql) | Aquarius pools emit a `reserves_sync` event IN ADDITION to |
| 0129 | [`0129_create_aquarius_protocol_fee.up.sql`](0129_create_aquarius_protocol_fee.up.sql) | Aquarius pools emit two treasury/governance events that were |
| 0130 | [`0130_create_aquarius_kill_switches.up.sql`](0130_create_aquarius_kill_switches.up.sql) | Aquarius pools emit circuit-breaker toggle events that were |
| 0131 | [`0131_create_phoenix_initialize.up.sql`](0131_create_phoenix_initialize.up.sql) | Phoenix pool contracts emit two `initialize` events at deploy — one |
| 0132 | [`0132_create_phoenix_admin_events.up.sql`](0132_create_phoenix_admin_events.up.sql) | Phoenix pool contracts emit four admin-rotation governance events |
| 0133 | [`0133_fix_api_key_prefix_check.up.sql`](0133_fix_api_key_prefix_check.up.sql) | Migration 0027 pinned the prefix regex to the PRE-REBRAND namespace: |
| 0134 | [`0134_populate_classic_asset_slugs.up.sql`](0134_populate_classic_asset_slugs.up.sql) | Backfill the classic_assets.slug disambiguator 0023 documented but never wrote (194k NULLs → code-issuer8; identity incident 2026-08-04) |
| 0135 | [`0135_full_issuer_slugs.up.sql`](0135_full_issuer_slugs.up.sql) | Classic slugs become the full asset_id — issuer-prefix abbreviations are vanity-grindable (~2^32/8 chars); full form is self-certifying |
| 0136 | [`0136_account_directory.up.sql`](0136_account_directory.up.sql) | account_directory: third-party curated address labels (stellar-expert/public-directory, MIT) synced by `stellarindex-ops directory-sync`; display-only, not a trust surface |
| 0137 | [`0137_disarm_comet_replay_doublecount.up.sql`](0137_disarm_comet_replay_doublecount.up.sql) | Disarm the comet replay double-count: drop comet_liquidity's mig-0059 event_index=0 legacy rows (replay writes true indexes → different PKs); REQUIRES `projector-replay -source comet -from 51499000` after deploy |
| 0138 | [`0138_defindex_flows_harvest_direction.up.sql`](0138_defindex_flows_harvest_direction.up.sql) | Admits `direction = 'harvest'` into `defindex_flows` (sources-decode audit 2026-08-04, finding 4). The BlendStrategy `harvest` event was recognised-and-dropped on the premise its body "has never been observed on-chain" — the lake disproves it (1,018 harvests; body `{amount, from, price_per_share}`, the exact `{from, amount}` shape `decodeFlow` already reads plus one unread field). Vault NAV reconstructed from deposit+withdraw alone under-counts by the full harvested yield. CHECK-widening only; additive + old-binary-safe (rule 9). |
| 0139 | [`0139_aquarius_fee_token.up.sql`](0139_aquarius_fee_token.up.sql) | Adds the claimed token to `aquarius_protocol_fee`, **correcting migration 0129's premise** (sources-decode audit 2026-08-04, finding 5). 0129 asserted the token identity was positional and not in the body — prescribing "join a recent trade for the pool to resolve it" — and shipped no token column. The lake refutes both halves: every sampled `claim_protocol_fee` carries an ScvAddress at topic[1] (the claimed token), and ledger 63,698,651 has one tx claiming TWO different tokens from one pool with near-identical amounts, so the join-a-trade workaround mis-attributes and any per-pool `SUM(amount)` mixes denominations. Additive + old-binary-safe (rule 9). |
| 0140 | [`0140_webauthn_credentials.up.sql`](0140_webauthn_credentials.up.sql) | New `webauthn_credentials` table — one row per registered passkey for dashboard sign-in, keyed to the platform user. Additive only, no changes to existing objects. The private key never exists server-side (`public_key` is the COSE-encoded VERIFICATION key). `sign_count` is the WebAuthn clone-detection counter, not a token amount, so it is `bigint` for uint32 headroom rather than NUMERIC — migrations rule 5 governs money values, and this is not one. |
| 0141 | [`0141_fx_quotes_derive_generation.up.sql`](0141_fx_quotes_derive_generation.up.sql) | Brings `fx_quotes` into the INV-3 generation-guard family (audit-2026-08-14 MR-1). `fx_quotes.rate_usd` is the denominator of every fiat-quoted `usd_volume`, yet its upsert was the ONLY money-value derived writer that 0109/0110 skipped: `ON CONFLICT DO UPDATE` with the derived value OUTSIDE any generation guard — pure arrival-order last-writer-wins. An operator CORRECTION written by `scripts/ops/fx-history-backfill` (`source='frankfurter-historical'`) over a key the live worker owns (`source='massive'`) was silently reverted by the next daily refresh: the exact "a correction is not durable" class INV-3 exists to close. |
| 0142 | [`0142_widen_derive_generation_bigint.up.sql`](0142_widen_derive_generation_bigint.up.sql) | Widens `derive_generation` from `int4` to `bigint` on the six protocol tables 0127-0132 created (audit-2026-08-14 W1-migrations-3). Every other `derive_generation` in the tree is already bigint — the three core tables (0109), the 25 protocol tables (0110), and `fx_quotes` (0141); these six drifted to `integer` while their own headers claimed parity with 0110. A latent BREAK rather than a cosmetic mismatch, since the value is a monotonically-increasing generation counter. |
| 0143 | [`0143_sessions_token_hash.up.sql`](0143_sessions_token_hash.up.sql) | Stores dashboard sessions keyed by a HASH of a random cookie token rather than by the raw primary key (audit-2026-08-14 W1-auth-passkey-2). The cookie carried `sessions.id` verbatim and the middleware looked the row up by that same PK, making sessions the ONE credential stored UNHASHED — `api_keys` and `magic_link_tokens` both store only sha256 of their secret. Any leak of the rows (an off-box backup, a read-replica exposure, a support export, one future log line including `session.id`) handed an attacker live sessions. **⚠ DEPLOY: forces a one-time re-login of every dashboard user** — existing rows cannot be converted, since the plaintext token they would hash was never stored. |
| 0144 | [`0144_account_observer_watermark.up.sql`](0144_account_observer_watermark.up.sql) | `account_observer_watermark` singleton (id=1) — the account observer's TRUE processed-ledger watermark (F-1320/R-002/CS-102 tail). The XLM supply freshness anchor (`Store.MaxAccountObservationLedger`) read MAX(ledger) from `account_observations`, which only rows on a watched-account balance CHANGE — so it went stale during quiet periods and the gate false-rejected (continuous `supply_refresh_error_dominant`, frozen `as_of`) while masking a genuinely-dead observer. The live indexer now advances `processed_ledger` every ledger it drives the observer over (monotonic upsert); a quiet observer stays fresh, a dead one freezes it and the gate fails closed past the horizon. Dedicated table (not `ingestion_cursors`, which feeds LatestKnownLedger/density/gap/cursor-lag; not a per-ledger heartbeat in `account_observations`, which would bloat it). Additive + old-binary-safe (rule 9). |
| 0145 | [`0145_credit_events_treasury_updated_check.up.sql`](0145_credit_events_treasury_updated_check.up.sql) | Admits `treasury_updated` into `credit_events.event_type` (2026-08-18 ADR-0033 recognition audit). One sorocredit shape `classify()` never matched — the main contract's `TreasuryUpdated` config event, 1 real lake event at ledger 63,847,367 — was dropped end-to-end, tripping `recognition_ok=FALSE` for the source. Body is `Vec[Address old, Address new]` (the protocol treasury pointer rotation), captured verbatim into `attributes["body"]` exactly like `BeaconUpdated` / `CollateralHashUpdated`: no promoted column, no invented semantics. CHECK-widening only; additive + old-binary-safe (rule 9). |
| 0146 | [`0146_create_defindex_fees.up.sql`](0146_create_defindex_fees.up.sql) | New `defindex_fees` hypertable (W5.2). The vault-layer `("DeFindexVault","dfees")` event was `classify()`'d but recognised-and-dropped — no decoder, no table — since Phase B, blocked on a real on-chain body sample per the do-not-invent discipline. That sample is now captured and proven from the r1 lake blobs: `body = Map{distributed_fees: Vec[(token Address, amount i128)]}`. Additive (new table) + old-binary-safe (rule 9). |
| 0147 | [`0147_ohlc_deterministic_tiebreak.up.sql`](0147_ohlc_deterministic_tiebreak.up.sql) | **Deterministic open/close tie-break in the nine price CAGGs**, plus the exact single-division VWAP that 0115 invited. Served OHLC open/close were `first/last(quote_amount/base_amount, ts)`, and on-chain sources stamp `ts` with the ledger close time (whole seconds) — so every bucket holding two trades from the same ledger (routine) had a TIE that TimescaleDB resolved by physical scan order, meaning two re-materializations of the same data could serve different bars. Everything else is preserved exactly from 0115/0126: column lists and order, the dust-floor FILTER + COALESCE on all four extremes (B11-F1), policies, indexes, no retention (0031), the `materialized_only` capture/restore, and the `twap_1h`/`twap_1d` dependents in their current 0126 form. **⚠ DEPLOY REQUIRES A FULL OPERATOR RE-MATERIALIZATION of all nine views — `WITH NO DATA`, the 0115 pattern; a pair/grain serves NO bars until refreshed.** The recent-first order, and the hierarchical `twap_*` refresh AFTER `prices_1m` is whole, are spelled out in the migration header. Already executed on r1 2026-08-22 (v0.40.0) — `docs/operations/notes/2026-08-22-price-p95-tail-post-0147.md` — but a FRESH database applying migrations from zero must still run it. |
| 0148 | [`0148_divergence_observations_synthetic_cross.up.sql`](0148_divergence_observations_synthetic_cross.up.sql) | Widens `divergence_observations.reference` CHECK to admit `synthetic-usd-cross` (PR #149's second lens for non-USD-fiat pairs). Verification-panel fix: without it every flushObservations INSERT for the new source fails the 0019 CHECK, and the audit-trail row for the reference that certifies unattended freeze releases could never exist. Compressed hypertable (segmentby includes `reference`) → 0101 decompress-every-chunk dance; policy recompresses. Keep `internal/api/v1/anomalies.go::divergenceReferences` in lockstep. |
| 0149 | [`0149_create_asset_volume_character_rollup.up.sql`](0149_create_asset_volume_character_rollup.up.sql) | New `asset_volume_character` worker-maintained rollup. Backs the `volume_character` label + account-structure signals on `GET /v1/assets/{id}` and (#30 follow-up) the `GET /v1/assets` listing. The live derivation is a per-asset trailing-14d account-structure roll over the `trades` hypertable — maker/taker live only in Timescale, not the ClickHouse lake — producing distinct makers/takers, top unordered-(maker,taker) pair share, self-cross share, issuer-side share, market-styled share, and a derived `market | operational | concentrated` character. Additive (new table) + old-binary-safe (rule 9). |
| 0150 | [`0150_add_trades_signer.up.sql`](0150_add_trades_signer.up.sql) | Adds `trades.signer` — the transaction source account (fee-payer) behind an AMM/Soroban swap. The AMM decoders (comet/soroswap/aquarius/phoenix) set `taker` to the on-chain caller and leave `maker` empty, so a router-driven swap has no EOA attribution (the taker is the router contract). The tx source account is that missing initiator, and it is NOT re-derivable from the lake events the projector replays. Additive (nullable column) + old-binary-safe (rule 9). |
| 0151 | [`0151_catalog_comment_corrections.up.sql`](0151_catalog_comment_corrections.up.sql) | Re-issues twelve wrong `COMMENT ON` strings (#357 F4/F5/F6, #358 0001/0003/item-7/0108, #346 F9): `soroban_events.topics_xdr` pointed at a "ClickHouse-lake re-project" recovery that does not exist; `aquarius_protocol_fee.recipient` still told readers to join a trade to find the token that 0139's `token` column carries; `aquarius_rewards_events` said 11 kinds where the CHECK admits 12; `trades.usd_volume` said "derived by the aggregator post-insert" when it is valued AT INSERT and a NULL never fills in; `oracle_updates.contract_id` named `coinmarketcap`/`chainlink-http` (measured on r1: chainlink, coingecko, ecb); `decoder_stats_5m` cited a `/v1/diagnostics/decoders` route that is not in the spec; `completeness_snapshots` cited a gitignored `notes/` file; and `customer_webhooks.secret_hash` got its first comment, recording that it holds the RAW HMAC key, not a hash. Catalog-only — no heap, no index, no chunk. Down restores all seven previous strings verbatim. Stored strings cannot be fixed by editing the original migration (see [Amending a shipped migration](#amending-a-shipped-migration)). |
| 0152 | [`0152_drop_unwired_scaffold_tables.up.sql`](0152_drop_unwired_scaffold_tables.up.sql) | Drops six never-wired scaffold tables — `wasm_versions` + `contract_wasm_history` (0017), `tvl_observations` (0021), `anchors` (0023), `classic_asset_stats_5m` (0024), `aggregator_exposures` (0025) — plus the four orphaned `stripe_event_log` columns and two partial indexes from 0118/0121, whose writers were deleted in `d2185560` (#358 items 2-6, #357 F8). Each had ZERO Go readers and writers at HEAD **and** in the released v0.57.0 tree, and `count(*) = 0` on r1. **⚠ The up REFUSES (RAISE EXCEPTION) if any of the six holds a row** — loud, not silent. Nothing is dropped, but golang-migrate marks 152 DIRTY, so recovery is `stellarindex-migrate force 151` → inspect/empty the table the message names → `stellarindex-migrate up`. Down re-creates every table with its original DDL and re-adds the Stripe shape with its 0118/0121 comments. Ships with the two sibling enumerations that would otherwise break: `scripts/ops/add-missing-compression-policies.sql` (ON_ERROR_STOP would abort the rest of the list) and `scripts/ops/config-assertions.sh` (`compression_policies_applied` would fail forever). |
| 0153 | [`0153_issuers_auth_flags_provenance.up.sql`](0153_issuers_auth_flags_provenance.up.sql) | Adds `auth_flags_source` + `auth_flags_as_of_ledger` to `issuers`, so a flag set recovered from a REMOVED account's last-known state is labelled as historical rather than served as current (#374). Existing rows backfill to `live`. |
| 0154 | [`0154_create_asset_price_snapshot.up.sql`](0154_create_asset_price_snapshot.up.sql) | Creates `asset_price_snapshot`, the worker-maintained per-asset headline USD price + 1h/24h/7d change + backing source count behind the `/v1/assets` listing (#331 F1). The price twin of 0087: the listing used to materialise twelve `DISTINCT ON … FROM prices_1m` CTEs for every asset on every uncached variant (r1 `pg_stat_statements`: 8,019 calls at mean 2,400 ms / max 10,295 ms, 380k buffer hits each), and now LEFT JOINs a ~10.5k-row keyed-on-PK table refreshed by `Store.RefreshAssetListingRollups` in the same transaction as `asset_volume_24h`. Money stays NUMERIC and unrounded; the listing keeps the identical wire rendering. The listing's join carries a 15-minute floor on `computed_at`, so a wedged aggregator serves NO price rather than a stale one. **⚠ On a fresh database the listing shows no prices until the aggregator's first pass** (≤ 2 min after it starts) — same posture as 0087. Down drops the table; a full rollback also needs the code reverted. |


F-1241 (codex audit-2026-05-12): the table previously stopped at
0015, leaving 0016..0029 (14 migrations) undocumented even though
they shipped. Future migrations: continue adding one row per
`.up.sql` landed on `main`.

## References

- [ADR-0003 i128 no-truncation](../docs/adr/0003-i128-no-truncation.md)
- [ADR-0006 TimescaleDB](../docs/adr/0006-timescaledb-for-price-time-series.md)
- [HA plan §3.3](../docs/architecture/ha-plan.md) — hypertable + retention design
- [Coverage matrix S6/S7](../docs/architecture/coverage-matrix.md) — requirement rows mapping to these schemas
