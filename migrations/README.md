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
   includes staging). Add a new migration instead.
2. **Numbering must be dense** — no gaps, no duplicates.
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
   construction. The legacy `prices_*` CAGGs (migration 0002) use
   the per-row form; measured on r1 2026-07-02 the divergence is
   ≤ 1.0e-16 relative (40,565 1h-bucket comparisons) — below the
   12-decimal wire truncation, so NOT worth rematerializing seven
   indefinite CAGGs. New aggregates must use the exact form. Note
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

## Conventions

- Statement terminators on their own line; always semicolon-end.
- `CREATE … IF NOT EXISTS` where idempotent; otherwise plain `CREATE`
  so a rerun after manual poking fails loudly.
- Comments above the statement (not inline) and explain the *why*.
- Timestamp columns are `timestamptz`, stored + served in UTC.
- Transactions: each migration runs in its own transaction by default
  (golang-migrate); disable with `-- migrate:no-transaction` when
  creating a hypertable on a very large existing table.

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
| 0016 | [`0016_create_anomaly_freezes.up.sql`](0016_create_anomaly_freezes.up.sql) | `anomaly_freezes` table — durable record of anomaly-freeze decisions per ADR-0019 |
| 0017 | [`0017_create_archive_completeness.up.sql`](0017_create_archive_completeness.up.sql) | `archive_completeness` tables backing the dual-archive completeness daemon per ADR-0017 |
| 0018 | [`0018_create_external_poller_state.up.sql`](0018_create_external_poller_state.up.sql) | `external_poller_state` table — persists last-success / last-error per external poller for restart-safe resume |
| 0019 | [`0019_create_market_observations.up.sql`](0019_create_market_observations.up.sql) | `market_observations` — per-pair / per-source observation log for divergence and `flags.single_source` provenance |
| 0020 | [`0020_create_supply_state.up.sql`](0020_create_supply_state.up.sql) | `supply_state` rollup populated by the aggregator from the per-class hypertables; backs F2 fields without recomputing on every read |
| 0021 | [`0021_create_change_summary.up.sql`](0021_create_change_summary.up.sql) | `change_summary_5m` hypertable — multi-window delta strip data backing every "+12.3% / -4.1%" surface on the explorer |
| 0022 | [`0022_create_incidents.up.sql`](0022_create_incidents.up.sql) | `incidents` + `incident_components` — public-facing SEV register backing `/v1/incidents` and `/v1/incidents.atom` |
| 0023 | [`0023_create_redstone_extras.up.sql`](0023_create_redstone_extras.up.sql) | Redstone-specific feed-id resolution table — bridges WritePrices events to operator-named feeds per redstone discovery note |
| 0024 | [`0024_create_divergence_runs.up.sql`](0024_create_divergence_runs.up.sql) | `divergence_runs` table — one row per CoinGecko / CMC / Chainlink-HTTP cross-check execution; backs the divergence dashboards |
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
| 0080 | [`0080_create_price_alerts.up.sql`](0080_create_price_alerts.up.sql) | `price_alerts` table — customer-configurable price-threshold alerts (BACKLOG #60 / RFP §6). Owner-scoped by `account_id` (FK to `accounts`, ON DELETE CASCADE, mirrors `customer_webhooks`). Columns: `base_asset`/`quote_asset` (canonical wire form), `condition` (above/below), `threshold` (NUMERIC per ADR-0003), `cooldown_seconds`, `enabled`, `last_fired_at`. The aggregator's price-alert evaluator (`internal/pricealerts`) sweeps enabled rows against the latest closed 1m VWAP and enqueues account-scoped `price.alert` deliveries into the existing `webhook_deliveries` queue. Additive + old-binary-safe (rule 9). |

| 0091 | [`0091_aquarius_liquidity_pool_tokens_idx.up.sql`](0091_aquarius_liquidity_pool_tokens_idx.up.sql) | Partial covering index `(contract_id, token_index, ledger_close_time DESC) WHERE token IS NOT NULL` on `aquarius_liquidity` — the `PoolTokens` resolver's `DISTINCT ON (contract_id, token_index) … ORDER BY …` had no matching index prefix, forcing a full materialize-and-sort of a forever-retained hypertable inside the `/v1/protocols/{name}` request path (review 2026-07-08 finding 2; same class as the 2026-06-19 protocol-detail runaway query). Additive, index-only. |
| 0093 | [`0093_create_nonstandard_decimals_assets.up.sql`](0093_create_nonstandard_decimals_assets.up.sql) | `nonstandard_decimals_assets` table — the read-side control table backing the dex-nonstandard-decimals READ-TIME serving guard (confirmed production bug 2026-07-08: token `CC2RB…` declares `decimals()=9`, skewing its aquarius pair's served price 100x). Small plain table (not a hypertable), PK `asset`, `CHECK (decimals <> 7)`. `internal/decimalsguard` (aggregator) upserts on confirmation; `internal/api/v1.NonstandardDecimalsCache` (API, ~60s refresh) mirrors it and declines `/v1/price`, `/v1/vwap`, `/v1/history`, `/v1/ohlc` for any pair with a listed leg. Additive + old-binary-safe (rule 9). |

| 0092 | [`0092_cctp_governance_events_check.up.sql`](0092_cctp_governance_events_check.up.sql) | Extends `cctp_events_event_type_check` 5 → 10 values for the governance/admin topics (ownership_transfer, ownership_transfer_completed, admin_changed, remote_token_messenger_added, token_pair_linked) — #89b admin-topic audit. Follows migration 0070's precedent; additive + old-binary-safe (rule 9). |

| 0094 | [`0094_cctp_full_governance_events_check.up.sql`](0094_cctp_full_governance_events_check.up.sql) | Extends `cctp_events_event_type_check` 10 → 26 values, closing the full topic census (admin_change_started, attester_enabled, attester_manager_updated, denylisted, denylister_changed, fee_recipient_set, max_message_body_size_updated, min_fee_controller_set, pauser_changed, rescuer_changed, set_burn_limit_per_message, set_token_controller, signature_threshold_updated, swap_minter_config_set, token_decimal_config_added, un_denylisted) — #89c full-census audit, retires the docs/protocols/cctp.md known-gap note. Follows migration 0092's precedent; additive + old-binary-safe (rule 9). |
| 0096 | [`0096_create_blend_emitter_events.up.sql`](0096_create_blend_emitter_events.up.sql) | `blend_emitter_events` hypertable — one row per observed event from the Blend **Emitter** contract (protocol-emissions plumbing, distinct from `blend` pools and `blend_backstop`): `distribute` (recurring BLND emission to a backstop), `drop` (one-shot airdrop, variable-length recipient list — fanned out one row per recipient via a `recipient_index` discriminator, same lesson as `aquarius_reserves`' `token_index`), `q_swap` / `swap` (backstop-swap timelock queue/execute). Gated on contract identity (`distribute` collides with `blend_backstop`'s own `distribute` topic). No published price, never VWAP. PK leads with `ledger_close_time` (TS103); compression after 7 days segmented by `(contract_id, event_kind)`. `BackfillSafe=false` pending a wasm-history audit across the Emitter's 3 observed WASM uploads. See `internal/sources/blend_emitter/README.md`. |
| 0099 | [`0099_create_aquarius_rewards_events.up.sql`](0099_create_aquarius_rewards_events.up.sql) | `aquarius_rewards_events` hypertable — the Aquarius rewards-gauge / liquidity-mining surface (ROADMAP #89 topic census, 2026-07-10): 12 event kinds (`pool_state`, `claim_reward`, `set_rewards_config`, `position_update`, bare `deposit`, `claim_fees`, `rewards_gauge_claim`, bare `claim`, `rewards_gauge_schedule_reward`, `set_rewards_state`, `rewards_gauge_add`, plus the router-side `config_rewards`) in one table with an `event_kind` discriminator + promoted `user_address`/`amount` columns + jsonb `attributes` remainder (blend_admin's 0045 pattern; i128s inside jsonb are decimal strings per ADR-0003). Pool-scoped kinds gate on the registered-pool set; `config_rewards` on the router trust root. Never VWAP. PK leads with `ledger_close_time` (TS103) and includes `event_index` from day one (F-1324 lesson). Historical fill via `projector-replay -source aquarius`. See `internal/sources/aquarius/README.md`. |
| 0100 | [`0100_create_aquarius_admin.up.sql`](0100_create_aquarius_admin.up.sql) | `aquarius_admin` hypertable — the Aquarius governance/upgrade surface (same #89 census): 8 event kinds (`apply_upgrade`, `commit_upgrade`, `set_privileged_addrs`, `apply_transfer_ownership`, `commit_transfer_ownership`, `enable_emergency_mode`, `disable_emergency_mode`, `pool_gauge_switch_token`) with `event_kind` + promoted `admin`/`target` columns + jsonb `attributes` (blend_admin pattern; `event_index` in the PK from the start). Gated on the canonical router trust root only — the flagged parallel deployment + unidentified sibling contracts fail-closed (visible ADR-0033 recognition gap, not silent attribution). Never VWAP. |
| 0101 | [`0101_soroswap_router_swaps_call_path.up.sql`](0101_soroswap_router_swaps_call_path.up.sql) | Adds `call_path text[]` / `call_depth smallint` / `call_kind text` to `soroswap_router_swaps` (ROADMAP #11): the tx auth-tree position of each captured router call — `call_path` is the ordered contract chain from the top-level invocation down to the router (always ends in `contract_id`), `call_kind` is `top_level`/`sub_invocation`. NULLable (pre-#11 rows carry NULL); all-or-none enforced by a CHECK that also ties kind↔depth↔cardinality. NOT in the PK and NOT in `call_sig` (auth-tree duplicates must still dedup, 0056). Partial-position columns backfill via DELETE + `ch-rebuild -sources soroswap-router -contract-calls -write -from 50746272` (queued r1 heavy job — see the migration header + `internal/sources/soroswap_router/README.md`). Additive + old-binary-safe (rule 9). |
| 0102 | [`0102_create_sac_balance_seed_provenance.up.sql`](0102_create_sac_balance_seed_provenance.up.sql) | `sac_balance_seed_provenance` — one row per watched SAC-wrapper contract auditing the most recent `supply seed-sac-balances` pass (`source` = `current_state` \| `full_history`, `holders_seeded`, `min_ledger_seen`/`max_ledger_seen`, `seeded_at`). PHO/BLND/EURC/KALE follow-up (incident 2026-07-06 / ROADMAP #14): `current_state` reads `stellar.ledger_entries_current`, which has a coverage floor (~ledger 62,000,000 — the MV never processed ch-backfilled rows inserted before it existed); the new `full_history` source (`clickhouse.StreamSACBalanceSeedsFullHistory`) reads `stellar.ledger_entry_changes` directly (complete to genesis, ADR-0034) to recover dormant pool-held SAC balances the floor hides. Pure audit trail — not consumed by the supply computation. Additive + old-binary-safe. |
| 0103 | [`0103_discovery_oracle_broadening.up.sql`](0103_discovery_oracle_broadening.up.sql) | Broadens `discovered_assets` (0006) for the generic-oracle discovery extension (docs/architecture/generic-oracle-sep-onboarding.md §3(b)): adds `discovery_kind` (`sep41` \| `oracle_event` \| `oracle_call`, DEFAULT `sep41`) and widens the `first_seen_event` CHECK to admit the oracle-suggestive symbol set (`price`, `oracle`/`Oracle`/`ORACLE`, `REDSTONE`, `StandardReference`, `relay`, …) alongside the original four SEP-41 values. Same table, same sighting-only discipline — no parallel storage surface. Additive + old-binary-safe (DROP+re-ADD CHECK, same idiom as 0070/0092/0094). |
| 0104 | [`0104_seed_soroswap_aggregator_exec.up.sql`](0104_seed_soroswap_aggregator_exec.up.sql) | Seeds the `routers` registry (0025) with the ONE evidence-verified aggregator wrapper contract (`CD45PQFH…`, real mainnet `exec()` call at ledger 62,029,020 — see `internal/sources/soroswap_router/real_bytes_test.go`), `auto_discovered=true`, fail-closed on everything unverified. Enables call-path-aware `routed_via` attribution. Additive + old-binary-safe. |
| 0105 | [`0105_create_classic_movements.up.sql`](0105_create_classic_movements.up.sql) | `classic_movements` hypertable (ADR-0047 Phase 1) — pre-P23 classic-asset movements reconstructed from the ClickHouse raw lake (`stellar.operations`/`operation_results`), never Horizon or a MinIO walk. `movement_kind` (10 values) + `provenance` (`classic_derived` / reserved `cap67_event`) discriminators admit all ten D1 kinds up front so Phases 2-4 need no schema churn; Phase 1's `internal/sources/classicmovements` decoder writes `payment`/`create_account` only. Natural key `(ledger, tx_hash, op_index, leg_index)`, `asset` as canonical asset_id string, `amount` NUMERIC. Historical-only — the writer (`stellarindex-ops classic-movements-backfill`) hard-clamps below the P23 boundary (ledger 58762517); never live-wired. Write-path only (no serving endpoint yet); retention deliberately deferred pending the first real backfill's row-size measurement. Not a live gap-detector target (see `internal/storage/timescale/gap_targets_test.go`'s `excludedFromGapDetector` entry) — coverage is a static recognition-switch test instead (ADR-0047 D4.2). **SUPERSEDED by [ADR-0048](../docs/adr/0048-serve-by-query-shape.md) D2 (2026-07-10):** this table stays applied but is now intentionally **UNPOPULATED** — the pre-P23 movement archive moved to ClickHouse-native `stellar.account_movements` (`deploy/clickhouse/tier1_schema.sql`, `internal/storage/clickhouse/account_movements.go`), feed-shaped (two rows per movement, direction discriminator). `stellarindex-ops classic-movements-backfill` no longer opens a Postgres connection at all. `internal/storage/timescale/classic_movements.go` (the writer this migration backs) is kept in-tree for reference; a future cleanup migration drops this table once the ClickHouse path is proven. |
| 0106 | [`0106_sep41_transfers_address_idx.up.sql`](0106_sep41_transfers_address_idx.up.sql) | Two partial indexes on `sep41_transfers` — `(from_addr, ledger DESC) WHERE from_addr IS NOT NULL` and the `to_addr` mirror — backing `timescale.Store.ListSEP41TransfersByAddress` (ADR-0048 D5): an address-scoped, cross-contract read the existing `sep41_transfers_contract_{from,to}_idx` (0047, `contract_id`-prefixed) and `sep41_transfers_ledger_idx` (0083, ledger-only) can't serve without a full scan. Backs the Postgres "recent tail" half of `GET /v1/accounts/{g}/movements`'s merge with ClickHouse's `stellar.account_movements`. `IF NOT EXISTS`, no `CONCURRENTLY` — same "applied by hand on r1 ahead of the migration run" convention as 0083. |
| 0107 | [`0107_positions_view_user_indexes.up.sql`](0107_positions_view_user_indexes.up.sql) | Two user-leading partial indexes backing `GET /v1/accounts/{g}/positions` (the "DeFi positions" view): `blend_positions (user_address, ledger_close_time DESC)` and `blend_backstop_events (user_address, ledger_close_time DESC) WHERE user_address IS NOT NULL` — neither table had a user-leading index (`blend_positions_pool_user_asset_idx` is pool-leading, 0053; blend_backstop_events had none at all, 0063), so a per-user fold across every pool would seq-scan. The other four tables the positions fold reads (`phoenix_stake_events`, `defindex_flows`, `credit_positions`, `aquarius_rewards_events`) already carry a suitable user-leading index from their own migrations. `IF NOT EXISTS`, no `CONCURRENTLY` — same convention as 0106. |
| 0113 | [`0113_drop_classic_movements.up.sql`](0113_drop_classic_movements.up.sql) | **Drops the dead `classic_movements` hypertable** (0105) — the cleanup migration the 0105 row promised (audit-2026-07-16 C2-18 / DAT-03). ADR-0048 D2 (2026-07-10) moved the pre-P23 classic-movement archive to ClickHouse-native `stellar.account_movements`; since then `stellarindex-ops classic-movements-backfill` writes ONLY to ClickHouse and opens no Postgres connection, so this table had no live writer or reader and was intentionally UNPOPULATED. The now-caller-less Go store (`internal/storage/timescale/classic_movements.go`) is removed in the same change. **DESTRUCTIVE + operator-reviewed — applies post-Phase-0 only.** No data is lost (table was empty by design); `.down.sql` recreates the exact 0105 schema (table + hypertable + indexes + compression policy) for reversibility. `DROP TABLE IF EXISTS`, no CASCADE (no CAGG/view/FK references it). |

F-1241 (codex audit-2026-05-12): the table previously stopped at
0015, leaving 0016..0029 (14 migrations) undocumented even though
they shipped. Future migrations: continue adding one row per
`.up.sql` landed on `main`.

## References

- [ADR-0003 i128 no-truncation](../docs/adr/0003-i128-no-truncation.md)
- [ADR-0006 TimescaleDB](../docs/adr/0006-timescaledb-for-price-time-series.md)
- [HA plan §3.3](../docs/architecture/ha-plan.md) — hypertable + retention design
- [Coverage matrix S6/S7](../docs/architecture/coverage-matrix.md) — requirement rows mapping to these schemas
