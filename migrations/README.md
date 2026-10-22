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
   was ever deleted — the rule applies going forward.) "Going forward"
   is enforced, not just written down: `lint-migrations.sh` fails any
   NEW migration numbered at or below the head that existed before it
   (GH-1164) — a file back-numbered into one of the five historical
   gaps above, or any other, is invisible to `migrate up` on a
   deployment already past that number.
3. **Changes to TimescaleDB features** (hypertables, compression,
   continuous aggregates) must be done with the extension's API
   (`create_hypertable`, `add_compression_policy`,
   `refresh_continuous_aggregate`) — not by touching the internal
   `_timescaledb_*` schemas directly.
4. **Every migration that creates a continuous aggregate** also adds
   its refresh policy in the same file — a CAGG without one is a silent
   bug — and does NOT add a retention policy. History is kept
   indefinitely (0031, ADR-0034; AGENTS.md invariant 8): a retention
   policy deletes rows on a schedule, forever, with nobody typing a
   command. The few relations that carry one are enumerated, each with
   its reason, in
   `internal/storage/timescale/retention_policy_test.go`
   (`TestRetentionPolicies_AreExactlyTheDeclaredSet`), which fails on
   any policy outside that set. None of them is a compute-completeness
   reconcile target, which 0116's floor depends on
   (`internal/ops/chops` `TestReconcileTargets_CarryNoRetentionPolicy`).
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
   end, so one NUMERIC rounding (~16 significant digits), not one
   per row. Never the per-row form
   `sum((quote/base) * base) / sum(base)`: each per-row division
   rounds at NUMERIC division scale, so the result is inexact by
   construction. The legacy `prices_*` CAGGs (migration 0002) used
   the per-row form; measured on r1 2026-07-02 the divergence is
   ≤ 1.0e-16 relative (40,565 1h-bucket comparisons) — below the
   12-decimal wire truncation, so not worth a re-materialization on
   its own. Migration 0147 (which re-materialized anyway for the
   deterministic open/close tie-break) switched them to the
   single-division form, so ALL price CAGGs now comply. They store
   no Σ(quote): the OHLC reads rebuild it as `round(vwap * volume)`,
   exact below 10^16 smallest units per row. A
   `sum(quote_amount) AS volume_quote` column is the free rider for
   the next re-materialization, not a reason for one. Note
   also the 0002 CAGGs materialize a `twap` column that is an
   equal-weight mean (`avg(quote/base)`), NOT time-weighted; on
   `prices_1m` migration 0166 floors it at $0.01 of notional. It is
   NOT dead: migration 0081's `twap_1h`/`twap_1d` CAGGs are
   `avg(prices_1m.twap)` over the window, and migration 0126's
   `sample_count` lets the serve layer
   (`internal/storage/timescale/aggregates.go::combineDirTWAP`) fold
   the stored market directions into the served TWAP. Read it only
   through `twap_1h`/`twap_1d`, never `prices_1m.twap` directly.

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
   anything already depends on what it would be reverting). A down
   that narrows a CHECK therefore REFUSES (`DO $$ … RAISE EXCEPTION`,
   as 0070's down does) while an offending row exists; it never
   `DELETE`s it. `scripts/ci/lint-migrations.sh` pass 5 fails any
   down-side `DELETE`/`TRUNCATE` not marked
   `-- lint-down-delete:ok <reason>` (the seed downs 0032/0033/0104,
   which remove only their own seed rows).

10. **An index on an existing hypertable is `CREATE INDEX IF NOT
    EXISTS` under `SET LOCAL lock_timeout`, and its header and register
    row name the `CONCURRENTLY` pre-build.** The in-transaction build
    holds a SHARE lock that blocks every write to the table for the
    whole build (a partial index still scans every row), and without a
    `lock_timeout` it waits behind any open transaction while every
    later writer queues behind it. `IF NOT EXISTS` is what lets an
    operator pre-build the index `CONCURRENTLY` and have the migration
    no-op; 0037 is the recipe. Pass 5 of `scripts/ci/lint-migrations.sh`
    enforces the two SQL halves; the nine shipped index builds that
    predate it (0025, 0037, 0083, 0091, 0101, 0106, 0107, 0123, 0150)
    are listed there.
11. **A migration, up OR down, that drops and recreates a continuous
    aggregate `WITH NO DATA` names the refresh for every view it
    recreates**, in order, in its header and register row. The
    migration cannot run it: `refresh_continuous_aggregate` refuses a
    transaction block, and golang-migrate runs each file as one (see
    Conventions). A hierarchical view (`twap_1h` / `twap_1d`, built on
    `prices_1m`) comes after its parent is whole. Pass 6 of
    `lint-migrations.sh` enforces that every recreated view is named;
    the data-freshness gauge `stellarindex_cagg_history_missing{view}`
    is what shows the refresh was skipped.
12. **An up-migration that blanks data a deployment must rebuild —
    an unqualified `DELETE` or a `TRUNCATE`, or a continuous aggregate
    recreated `WITH NO DATA` — declares each rebuild command on its own
    header line:**

    ```sql
    -- REQUIRED-FOLLOWUP: stellarindex-ops projector-replay -config /etc/stellarindex.toml -source cctp -from 62146641 -write
    -- REQUIRED-FOLLOWUP: CALL refresh_continuous_aggregate('prices_1m', NULL, NULL);
    ```

    `deploy.yml`'s migration follow-up gate
    (`scripts/ci/migration-followup-gate.sh`) lists these lines from
    every migration a deploy adds and refuses to start until the
    operator passes `followups_acknowledged=true`. Use the marker on
    any other up-migration whose data needs a follow-up too (a
    targeted `DELETE … WHERE`). Pass 10 of `lint-migrations.sh`
    enforces it; the seven shipped files that predate it are listed
    there. A follow-up that rebuilds a projected source
    (`projector-replay` or `projected-rebuild -source X`) also needs
    `DELETE FROM ingestion_cursors WHERE source = 'projected-rebuild'
    AND sub_source LIKE 'X:%'` in the same file: `projected-rebuild
    -resume` skips every checkpointed window without checking the
    table, so an older checkpoint keeps the emptied window empty
    (0206). Pass 10 enforces that too.

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
ANY reason, including a bug in it (ship a corrective migration instead),
with the single exception below.

### Neutralising an unapplied migration

A corrective migration cannot fix an UP body whose damage happens when
that body itself runs: golang-migrate applies N before any later number.
0174 (v0.92.0) was one — a 780 GB decompress inside the deploy. For that
case only, a shipped UP body may be replaced by a no-op (`SELECT 1;`) in
the next patch release, when ALL of these hold:

1. **Nobody we operate has applied it.** Every environment's
   `schema_migrations.version` is measured `< N` and `dirty = f`, and the
   measurement (host, value, date) is in the commit message.
2. **The number and both files are kept.** Never delete or renumber:
   golang-migrate cannot advance a database whose current version has no
   file (`Next(N)` returns not-exist).
3. **The DOWN reverses both bodies** — the old one and the no-op (e.g.
   `DROP CONSTRAINT IF EXISTS`).
4. **A database that ran the old body is in a state the new code
   tolerates.** Say why in the header (0174: the kept CHECK is a superset
   of the Go row contract).
5. **The release notes say so**, including who could have run the old
   body: anyone who built from source or self-hosted the release between
   the migration's commit and the patch.
6. **The old body is kept as a comment** in the header, with the reason it
   was neutralised, so the header still describes the file.

The checksum baseline moves in the same commit (`--write`), as for any
correctable edit. Anything short of all six is a corrective migration.

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
migration lands. Write each row as whole sentences ending in a full
stop, not a line lifted from the header; `scripts/ci/lint-migrations.sh`
rejects a row that is not.

| Number | Adds |
| --- | --- |
| 0001 | Core `trades` hypertable, retention policy, primary indexes. |
| 0002 | Creates continuous aggregates for 1m, 15m, 1h, 4h, 1d, 1w and 1mo with refresh and retention policies. The `twap` column is `avg(quote/base)`, an arithmetic mean, not time-weighted. |
| 0003 | `oracle_updates` hypertable for Reflector / Redstone / Band observations + compression + retention. |
| 0004 | Relaxes the `trades.ledger > 0` constraint so off-chain sources (Binance / Kraken / etc) can stamp `ledger = 0`. |
| 0005 | `asset_supply_history` hypertable per ADR-0011 — append-only per-asset supply snapshots backing the F2 fields on `/v1/assets/{id}`. |
| 0006 | `discovered_assets` table for SEP-41 auto-discovery; every contract emitting a transfer / mint / burn / clawback event lands here for operator triage. |
| 0007 | `volatility_baseline_1m` per-pair statistical baseline per ADR-0019 Phase 2 — robust median + MAD baseline used by the anomaly-freeze policy. |
| 0008 | Adds 1d + 7d baseline columns to `volatility_baseline_1m` per ADR-0019 §"Multi-window safeguard against frog-boiling". |
| 0009 | `blend_auctions` hypertable — one row per observed Blend auction event (new_auction, etc.). |
| 0010 | `account_observations` hypertable per ADR-0021 — one row per AccountEntry-delta touching an operator-watched account, backs Algorithm 1 (XLM) reserves. |
| 0011 | `trustline_observations` hypertable per ADR-0022 — backs Algorithm 2 classic-credit supply: Σ trustline component. |
| 0012 | `claimable_observations` hypertable per ADR-0022 — backs Algorithm 2: Σ claimable-balance component. |
| 0013 | `lp_reserve_observations` hypertable per ADR-0022 — backs Algorithm 2: Σ LP-reserve component. |
| 0014 | `sac_balance_observations` hypertable per ADR-0022 — backs Algorithm 2: Σ SAC-wrapped contract balance component. |
| 0015 | `sep41_supply_events` hypertable per ADR-0023 — backs Algorithm 3 SEP-41 supply: Σ mint − Σ burn − Σ clawback per contract. |
| 0016 | Adds the `soroswap_pairs` registry mapping pair_contract to (token0, token1), which the Soroswap decoder needs to assign base and quote sides. A table so parallel backfills see out-of-window pairs. |
| 0017 | Adds `wasm_versions` (content-addressed bytecode) and `contract_wasm_history` (per-contract temporal index), making WASM history queryable for the contract explorer pages and version timeline. |
| 0018 | `freeze_events` hypertable per ADR-0019 — durable history of every freeze the anomaly engine (`internal/aggregate/freeze`) fires, which otherwise lived only in Redis and was lost on restart. |
| 0019 | `divergence_observations` hypertable — persists the per-(asset, quote, reference) delta percent `internal/divergence/worker.go` computes against external references (Chainlink, CoinGecko, Reflector, Redstone, Band); before it only the `flags.divergence_warning` boolean survived. |
| 0020 | Adds the `decoder_stats_5m` rollup hypertable, filled every 5 minutes by the indexer's statsflush worker with the dispatcher's per-source counters. No route reads it yet. |
| 0021 | `tvl_observations` (per-protocol TVL ticks) + `mev_events` — two tables populated by aggregator-side analytics workers rather than the live ingest path, so they share one migration. |
| 0022 | `change_summary_5m` rollup — one row per (entity_type, entity_id) carrying the multi-window deltas (1h / 24h / 7d / 30d) behind every delta strip, refreshed every 5 minutes. |
| 0023 | Adds the classic-asset registry: `classic_assets` (one row per observed `(code, issuer)`), `issuers` and `anchors`, so asset listings browse a catalogue instead of scanning `trades`. |
| 0024 | Adds the `classic_asset_stats_5m` rollup hypertable: per-asset 24h USD volume, last trade ledger, trustline count and supply, refreshed every 5 minutes for the asset detail and directory pages. |
| 0025 | Soroswap router + attribution tables — tracks which router contract emitted each multi-hop swap so attribution is per-router. |
| 0026 | `price_source_contributions` (live: per-source weight history) + `sdex_offer_events` (**unwired**: no Go writer or reader has ever existed; the table is empty and carries no gap-detector target, see `internal/storage/timescale/per_source_gaps.go`). |
| 0027 | Adds the platform v1 schema: accounts, users, sessions, API keys, Stripe subscriptions and the dashboard webhook store. `subscriptions`, `stripe_event_log` and `api_usage_events` have no writers but stay for account erasure. |
| 0028 | `fx_quotes` hypertable — long-form persisted fiat/USD FX history written by the `massive` forex worker (historical rows via `scripts/ops/fx-history-backfill`), backing `/v1/chart` fiat:fiat past 7d. |
| 0029 | Drops two unused JSONB GIN indexes on `blend_auctions` — the auction query path uses btree on the typed columns. |
| 0030 | Promotes `asset_supply_history_asset_ledger_idx` from a unique index to a unique constraint on `(asset_key, ledger_sequence, time)` so the writer's `ON CONFLICT` matches on hypertables. Compression is disabled around the DDL and restored. |
| 0031 | Removes the 90-day retention policy on `trades` and the 30-day retention on `prices_1m` and `prices_15m`, so every raw trade is kept forever. |
| 0032 | Pre-seeds `routers` with the operator-vetted Soroswap router contract id (`auto_discovered = false`), so the router-attribution observer can populate the dispatcher's `ContractCallDecoder` match-set at startup. |
| 0033 | Pre-seeds `routers` with the three Phase-A DeFindex autocompound vaults (`kind = 'aggregator-vault'`); hand-curated until factory-event vault discovery ships. |
| 0034 | Continuous aggregates for `oracle_updates` (7-tier grain set, sister to 0002) — first/last/min/max/count per `(source, asset, quote, bucket)`, preserving per-oracle identity rather than collapsing to a single VWAP. |
| 0035 | `source_entry_counts` — an always-on, per-source running tally of ingested entries (trades + oracle_updates) so `/v1/diagnostics/ingestion` has a coverage number that stays cheap to read even during an all-time backfill. |
| 0036 | `pools_per_source_1h` continuous aggregate — durable 1h-bucket backing for `/v1/pools`, eliminating the cold full-`trades`-scan the handler used to pay (#25). |
| 0037 | Adds `trades_pair_source_ts_idx (base_asset, quote_asset, source, ts DESC, ledger DESC)`, turning the latest-trade-per-source query into a skip-scan. On a populated node build it `CONCURRENTLY` by hand first; see the `.up.sql` header. |
| 0038 | Adds the `cctp_events` hypertable: one row per Circle CCTP v2 bridge event, with typed columns (amount, fee, token, counterparty_domain) and a jsonb `attributes` remainder. Bridge class; never feeds VWAP. |
| 0039 | Adds the `rozo_events` hypertable: one row per Rozo v1 intent-bridge event (payment or flush), fully typed with no jsonb blob. Bridge class; never feeds VWAP. |
| 0040 | Removes the 90-day retention policy on `oracle_updates`, matching 0031 for `trades`, so every raw oracle observation is kept. The 0034 aggregates are unchanged; the header documents re-backfilling them. |
| 0041 | Adds the `soroban_events` hypertable, the raw landing zone for routed Soroban contract events, with topics, body and op_args as raw XDR for decoder backfills. Compressed after 7 days. |
| 0042 | Adds the `comet_liquidity` hypertable: one row per `(ledger, tx_hash, op_index, event_kind, token)` for Balancer-v1 join, exit, deposit and withdraw events. The PK includes `token`. |
| 0043 | Adds the `soroswap_skim_events` hypertable for Soroswap pair `skim` events (claim of excess pool balance), so every emitted topic is classified. Not a trade; never feeds VWAP. Compressed after 7 days. |
| 0044 | Adds the `phoenix_liquidity` and `phoenix_stake_events` hypertables for `provide_liquidity`, `withdraw_liquidity`, `bond` and `unbond`. Withdraw rows have NULL tokens and provide rows NULL `shares_amount`, since the contract does not emit them. |
| 0045 | Adds the `blend_positions`, `blend_emissions` and `blend_admin` hypertables covering the 18 Blend event topics the auction-only classifier missed. Compressed after 7 days, segmented by `contract_id`. |
| 0046 | Adds `first_ledger` to `ingestion_cursors` so the density-coverage projection credits the live cursor's full `[first_ledger, last_ledger]` span, not just backfilled ranges. |
| 0047 | `sep41_transfers` hypertable (F-0021) — every SEP-41 `transfer` event plus the `approve` / `set_admin` / `set_authorized` administrative events, for per-account net-position queries. mint / burn / clawback stay in `sep41_supply_events` (0015). |
| 0048 | Adds `source_coverage_snapshots`: one row per (source, table), overwritten by the gap detector after each scan, which diagnostics read for data-derived density and gap-free percentages. |
| 0049 | Adds the `soroswap_router_swaps` hypertable: one row per router invocation, decoded from InvokeContract args because the router emits no events. A multi-hop call is one row here and N `trades` rows. |
| 0050 | `defindex_flows` hypertable — one row per decoded DeFindex flow event, across both layers: STRATEGY (`BlendStrategy`; actor is the vault moving capital) and VAULT (`DeFindexVault`; actor is the end user). |
| 0051 | Adds `ledger_ingest_log`: one row per ledger the indexer walked, written once its events are enqueued to the async sink, with the hash chain and LCM census counts. It is not a persistence marker; reconciliation against stored rows proves persistence. |
| 0052 | Adds `completeness_snapshots`: one row per source holding the completeness watermark, the highest ledger up to which substrate, recognition and projection all reconcile from genesis. Written by `stellarindex-ops compute-completeness`. |
| 0053 | Fixes coarse-PK data loss in Blend tables: `blend_positions` adds `(asset, user_address)` to its PK, and `blend_emissions` and `blend_admin` gain `event_index`. Existing emissions and admin rows get 0, so re-derive them. |
| 0054 | Changes the `blend_positions` PK discriminator from `(asset, user_address)` to `event_index`, since one op can emit two same-kind events for one asset and user. Existing rows get 0; re-derive the table. |
| 0055 | Adds `event_index` to the `defindex_flows` PK, the same coarse-PK fix as 0053 and 0054. On a populated host, delete rows before adding the new PK and re-derive. |
| 0056 | Adds `call_sig` (content hash of function, recipient, path and amounts) to the `soroswap_router_swaps` PK so distinct swaps in one op stay separate. Populated hosts truncate and re-derive. |
| 0057 | Adds `event_index` and `event_kind` to the `sep41_supply_events` PK so several supply events in one op no longer collapse. Populated hosts delete rows and re-derive; stop the indexer first. |
| 0058 | Adds `event_index` and `event_kind` to the `blend_auctions` PK, which 0053 skipped, since one op can emit several same-kind auction events. Stop the indexer first. |
| 0059 | Adds `event_index` to the `comet_liquidity` PK so two same-kind, same-token events in one op no longer collapse under `ON CONFLICT DO NOTHING`. Stop the indexer first. |
| 0060 | Adds `event_index` (index of a reassembled action's first field-event) to the `phoenix_liquidity` and `phoenix_stake_events` PKs so two same-kind actions in one op no longer collapse. Stop the indexer first. |
| 0061 | Adds `protocol_contracts`, the registry of factory-deployed child contracts that the blend, aquarius, phoenix and defindex decoders gate on, upserted when the factory's creation event is decoded. |
| 0062 | Widens `chunk_time_interval` to 7 days on the five hot hypertables created with 1 day (`trades`, `soroban_events`, `blend_auctions`, `phoenix_liquidity`, `phoenix_stake_events`). New chunks only; the down is a no-op. |
| 0063 | `blend_backstop_events` hypertable — one row per Blend Backstop contract event (the V2 singleton and the V1 deployment before it), a separate event surface from the pool / pool-factory tables. |
| 0064 | Adds the `dex_volume_by_pair_1d` continuous aggregate, a daily per-(source, base_asset, quote_asset) trade rollup for DEX protocol-page analytics. Created `WITH NO DATA`; the operator must refresh it once. |
| 0065 | Adds an `attempts` counter to `magic_link_tokens`, the brute-force guard on the 6-digit email-code sign-in: each wrong guess increments it and matching stops past the cap. |
| 0066 | Adds the `supply_1d` continuous aggregate: daily last-known supply per `asset_key` from `asset_supply_history`, the supply leg of market-cap-over-time charts. Created `WITH NO DATA`; the operator must refresh it once. |
| 0067 | Adds the `arbitrage` kind to `mev_events` and a unique `dedup_key` (`<kind>:<tx_hash>:<taker>`) so the v1 atomic-arbitrage detector's overlapping re-scans are idempotent under `ON CONFLICT DO NOTHING`. The other kinds stay reserved. |
| 0068 | Adds the `source_volume_1h` continuous aggregate: per-(source, hour) trade count and USD-volume inputs for the source page activity chart. Created `WITH NO DATA`; the operator must refresh it once. |
| 0069 | Enables real-time aggregation (`materialized_only = false`) on `source_volume_1h`, so the in-progress hour is served instead of reading as no data until ~5 minutes after it closes. |
| 0070 | Admits `mint_and_forward` into the `cctp_events.event_type` CHECK: 0038 enumerated only the original four types, so every insert of the CctpForwarder's fifth event (decoded since v0.7.0) was rejected. |
| 0071 | `usage_daily` hypertable — per-(day, subject, endpoint) API usage rollup folded from the Redis per-endpoint counters every 5 minutes; backs /v1/account/usage and is the only per-endpoint history beyond Redis's 35-day TTL. |
| 0072 | Renames the Soroswap router's `routers.name` from `soroswap-router-v1` to `soroswap-router`, the name written to `trades.routed_via`, and appends a note. The down restores both name and notes. |
| 0073 | Adds `api_keys.scopes`, the coarse route-family scope list (read, account, dashboard, admin) a key is confined to. The empty-array default keeps existing keys on full access. |
| 0074 | Adds the `oracle_sandwich` kind to the `mev_events` CHECK as the MEV worker ships the detectors 0067 left reserved (`sandwich`, `wash_trade` and `liquidation_cascade` were already admitted); `oracle_deviation` stays reserved. |
| 0076 | Enables real-time aggregation (`materialized_only = false`) on `pools_per_source_1h`, so /v1/pools no longer reads zero for the in-progress hour — the 0069 fix. |
| 0080 | Adds the `price_alerts` table: owner-scoped (`account_id` FK, ON DELETE CASCADE) price-threshold alerts with `base_asset`, `quote_asset`, `condition`, `threshold` (NUMERIC), `cooldown_seconds`, `enabled` and `last_fired_at`. The evaluator enqueues `price.alert` webhook deliveries. |
| 0081 | Adds the `twap_1h` and `twap_1d` continuous aggregates for the TWAP chart, a time-weighted mean of `prices_1m.twap`, unlike 0002's column. Created `WITH NO DATA`; refresh `twap_1h` then `twap_1d` once. |
| 0082 | `status_notices` — operator-posted, customer-facing status banners (maintenance windows, unfolding incidents), written only by the audit-logged `/v1/admin/status-notices` endpoints and read by the public `/v1/status/notices`. Notices are resolved, never deleted. |
| 0083 | Adds `sep41_transfers_ledger_idx`, a ledger-led index so the projection reconcile's windowed count stops seq-scanning `sep41_transfers`. `IF NOT EXISTS` because populated nodes build it by hand first. |
| 0085 | Adds `sep41_supply_rollup`: one row per watched contract with running mint, burn and clawback totals up to `last_ledger`, advanced incrementally so the supply read stops re-aggregating all `sep41_supply_events` each tick. |
| 0086 | `protocol_events_24h` — worker-maintained rollup, one row per source, backing the `events_24h` column on GET /v1/protocols and /v1/protocols/{name} in place of a per-request count over ~17 protocol hypertables (~5s cold). |
| 0087 | `asset_volume_24h` — worker-maintained rollup backing the `volume_24h_usd` column on the GET /v1/assets listing in place of a per-request `SUM(volume_usd)` over `prices_1m` (~4.8s cold on the unfiltered listing). |
| 0088 | Adds genesis-baseline columns to `sep41_supply_rollup` (`genesis_mint_total`, `genesis_burn_total`, `genesis_clawback_total`, `genesis_baseline_ledger`, `genesis_seeded_at`) for pre-Soroban mints the observer never sees. The reader adds nothing until `stellarindex-ops supply seed-sep41-genesis` runs. |
| 0089 | Adds the `aquarius_reserves` (post-state reserves, one row per token position) and `aquarius_liquidity` hypertables for Aquarius events that were classified but dropped. Fill history with `projector-replay -source aquarius`. |
| 0090 | Adds `credit_positions`, `credit_statements`, `credit_settlements` and `credit_events`, the served-tier tables for the sorocredit USDC credit protocol, filled by the projector. |
| 0091 | Adds a partial covering index on `aquarius_liquidity (contract_id, token_index, ledger_close_time DESC) WHERE token IS NOT NULL`, so the `PoolTokens` resolver stops sorting the whole hypertable. |
| 0092 | Extends `cctp_events_event_type_check` 5 → 10 values for the governance/admin topics (ownership_transfer, ownership_transfer_completed, admin_changed, remote_token_messenger_added, token_pair_linked) — #89b admin-topic audit. Follows migration 0070's precedent; additive + old-binary-safe (rule 9). |
| 0093 | Adds `nonstandard_decimals_assets` (PK `asset`, `CHECK (decimals <> 7)`), the control table behind the read-time guard that declines price endpoints for pairs with a non-7-decimals leg. |
| 0094 | Extends `cctp_events_event_type_check` from 10 to 26 values, completing the CCTP topic census (admin, attester, denylist, fee, limit and config events). |
| 0095 | Admits `rw_zone` (the V1 backstop's reward-zone-update topic) and `rw_zone_remove` (V2, not yet observed) into the `blend_backstop_events.event_kind` CHECK. Additive + old-binary-safe (rule 9). |
| 0096 | Adds the `blend_emitter_events` hypertable for Blend Emitter events (`distribute`, `drop`, `q_swap`, `swap`); `drop` fans out one row per recipient via `recipient_index`. Gated on contract identity; never VWAP. |
| 0097 | Admits the V1 pool-factory's three extra event kinds (`update_emissions`, `new_liquidation_auction`, `delete_liquidation_auction`) into the `blend_emissions.event_kind` / `blend_admin.event_kind` CHECKs. |
| 0098 | Admits `withdraw_rewards` / `distribute_rewards` into `phoenix_stake_events.action`; makes `user_addr` + `amount` nullable. |
| 0099 | Adds the `aquarius_rewards_events` hypertable for Aquarius rewards-gauge and liquidity-mining events: 12 kinds in one table with `event_kind`, `user_address`, `amount` and a jsonb `attributes` remainder. PK includes `event_index`. Never VWAP. |
| 0100 | Adds the `aquarius_admin` hypertable for 8 Aquarius governance and upgrade event kinds, with `event_kind`, `admin`, `target` and jsonb `attributes`. Gated on the canonical router only; never VWAP. |
| 0101 | Adds `call_path text[]`, `call_depth smallint` and `call_kind text` to `soroswap_router_swaps`: the router call's tx auth-tree position. Nullable, all-or-none by CHECK, and not in the PK or `call_sig`. |
| 0102 | Adds `sac_balance_seed_provenance`: one row per watched SAC-wrapper contract auditing the latest `supply seed-sac-balances` pass (`source` of `current_state` or `full_history`, `holders_seeded`, ledger range). Audit trail only. |
| 0103 | Adds `discovery_kind` (`sep41`, `oracle_event` or `oracle_call`) to `discovered_assets` and widens the `first_seen_event` CHECK to admit oracle-suggestive symbols alongside the original SEP-41 values. |
| 0104 | Seeds the `routers` registry with the one evidence-verified aggregator wrapper contract (`auto_discovered=true`), failing closed on everything unverified, to enable call-path-aware `routed_via` attribution. |
| 0105 | Adds the `classic_movements` hypertable for pre-P23 classic-asset movements. Superseded by ADR-0048: the table was never populated, the archive lives in ClickHouse `stellar.account_movements`, and 0113 drops it. |
| 0106 | Adds two partial indexes on `sep41_transfers` for `from_addr` and `to_addr` (each with `ledger DESC`), serving address-scoped cross-contract reads that the contract-prefixed indexes cannot. `IF NOT EXISTS`, no `CONCURRENTLY`. |
| 0107 | Adds user-leading partial indexes `blend_positions (user_address, ledger_close_time DESC)` and `blend_backstop_events (user_address, ledger_close_time DESC) WHERE user_address IS NOT NULL` for the per-user DeFi positions fold. `IF NOT EXISTS`, no `CONCURRENTLY`. |
| 0108 | Adds `lake_complete` to `completeness_snapshots`: whether the ClickHouse archive is contiguous, hash-chained and recognition-complete from genesis, separate from `complete`, which is also gated by the projection window. |
| 0109 | Adds `derive_generation` to `trades`, `oracle_updates` and `asset_supply_history`, so a corrected re-derive updates a wrong value in place through a generation-guarded upsert instead of no-op'ing under `ON CONFLICT DO NOTHING`. |
| 0110 | Extends the 0109 generation guard to protocol projector tables: `derive_generation bigint NOT NULL DEFAULT 0` on every protocol-specific projector-output table. |
| 0111 | Adds `intra_ledger_seq` to the five balance-observation hypertables so the final intra-ledger change deterministically wins the last-writer-wins upsert whatever order the parallel PersistEvents workers commit in (audit-2026-07-16 C2-6). |
| 0112 | Adds `event_index` to the `cctp_events` and `rozo_events` PKs, since one op can emit two same-type events and the old key dropped one. Existing rows get 0; dropped siblings need re-ingest. |
| 0113 | Drops the unpopulated `classic_movements` hypertable (0105), whose archive moved to ClickHouse `stellar.account_movements`. Destructive and operator-reviewed; no data is lost, and the down recreates the 0105 schema. |
| 0114 | Adds `soroban_events.topics_xdr`, the complete ordered topic list, because the four fixed `topic_N_xdr` columns dropped topics 5 and beyond. Forward fix only; existing rows keep `'{}'`. |
| 0115 | Applies a notional floor to the OHLC extremes of the seven `prices_*` aggregates: high and low ignore fills under 0.01 USD volume. VWAP and volume stay unfiltered. Needs re-materialization. |
| 0116 | Adds `completeness_target_floors`, keyed `(source, target_table, target_filter)`: the lowest ledger each reconcile target was ever verified from. The floor only falls, so truncating the served tier cannot erase its own evidence. |
| 0117 | Adds nullable `sac_wrapped_stroops NUMERIC` to `asset_supply_history`, the SAC-wrapped supply component for the escrow-versus-minted cross-check. NULL means unchecked and must never be defaulted to 0. |
| 0118 | Adds nullable `dead_lettered_at`, `dead_letter_reason` and `dead_letter_resolved_at` to `stripe_event_log` for the paid-but-not-provisioned state. Tombstoned by 0152, which dropped these columns. |
| 0119 | Adds nullable `hold_until`, `extensions_used`, `escalated` and `corroborated` to `freeze_events`, so the freeze extension ladder survives losing Redis instead of silently releasing live freezes. |
| 0120 | Comment-only amendment to 0111's `intra_ledger_seq` contract: the column comments now describe the real ledger-wide phase order of stellar-core meta, not a per-transaction walk. No schema or row change. |
| 0121 | Adds nullable `claimed_at` and a partial in-flight index to `stripe_event_log`, the durable claim (taken under an advisory lock) that stops two concurrent deliveries of one paid event from both provisioning. |
| 0122 | Adds `login_code_lockouts`, a durable per-email failed-verify counter (`failed_count`, `window_started_at`, `locked_until`) for the 6-digit code sign-in, surviving Redis loss. It gates the code path only; rows are reaped by `internal/logincodereaper`. |
| 0123 | Adds partial account-leading indexes `trades_taker_ts_idx` and `trades_maker_ts_idx` for the per-account trades endpoint. On a populated node build both by hand first with `timescaledb.transaction_per_chunk`; the header has the statements. |
| 0124 | Admits `other` into the `freeze_events.reason` CHECK, so an automated freeze with an unrecognised decision is no longer recorded as `manual`, the operator-only value. |
| 0125 | `projection_dirty_windows` — the per-source ledger window a `projector-replay` rewound over, recorded before the cursor rewinds, so the completeness verifier re-reconciles it instead of carrying the prior clean projection claim. |
| 0126 | Adds `sample_count` (per-direction minute coverage) to the `twap_1h` and `twap_1d` aggregates, recreated as a column superset, so opposite-orientation SDEX TWAPs merge by coverage. Created `WITH NO DATA`; re-materialize both. |
| 0127 | `soroswap_liquidity` hypertable — one row per Soroswap pair `deposit` / `withdraw` event, previously classified but dropped with no table. Historical fill: `stellarindex-ops projector-replay -source soroswap -from <ledger>`. |
| 0128 | Adds the `aquarius_reserves_sync` hypertable for the pool `reserves_sync` event, which carries different values from `update_reserves`; one row per token position like `aquarius_reserves`. |
| 0129 | `aquarius_protocol_fee` hypertable — the pool `set_protocol_fee` and `claim_protocol_fee` treasury events in one table with a `kind` discriminator, previously recognized but not projected. Historical fill: `stellarindex-ops projector-replay -source aquarius`. |
| 0130 | `aquarius_kill_switches` hypertable — the eight pool `kill_*` / `unkill_*` circuit-breaker toggles (deposit, swap, claim, gauges_claim), previously recognized but not projected. Historical fill: `stellarindex-ops projector-replay -source aquarius`. |
| 0131 | `phoenix_initialize` hypertable — the two `initialize` events a Phoenix pool emits at deploy (one per pool token), previously classified but dropped. Historical fill: `stellarindex-ops projector-replay -source phoenix`. |
| 0132 | Adds the `phoenix_admin_events` hypertable for the four Phoenix pool admin-rotation governance events, so the first rotation is recorded rather than dropped. |
| 0133 | Widens the `api_keys.key_prefix` CHECK to admit the `sip_` prefix every minter emits; 0027 pinned the old `rek_` prefix, so Postgres-backed key inserts failed. Existing rows stay valid. |
| 0134 | Backfill the classic_assets.slug disambiguator 0023 documented but never wrote (194k NULLs → code-issuer8; identity incident 2026-08-04). |
| 0135 | Classic slugs become the full asset_id — issuer-prefix abbreviations are vanity-grindable (~2^32/8 chars); full form is self-certifying. |
| 0136 | `account_directory`: third-party curated address labels (stellar-expert/public-directory, MIT) synced by `stellarindex-ops directory-sync`; display-only, not a trust surface. |
| 0137 | Deletes `comet_liquidity`'s legacy `event_index = 0` rows from 0059 so a replay, which writes true indexes, cannot double-count. Requires `projector-replay -source comet -from 51499000` after deploy. |
| 0138 | Admits `direction = 'harvest'` into the `defindex_flows` CHECK, so BlendStrategy harvest events are recorded and vault NAV includes harvested yield. |
| 0139 | Adds the claimed token to `aquarius_protocol_fee`, since one tx can claim different tokens from one pool and per-pool sums would mix denominations. Earlier rows have NULL `token`; re-derive with `projector-replay`. |
| 0140 | Adds `webauthn_credentials`: one row per registered passkey for dashboard sign-in, keyed to the user. Stores only the COSE public key; `sign_count` is a clone-detection counter, so `bigint`, not money. |
| 0141 | Brings `fx_quotes` into the generation-guard family, so a corrected `rate_usd` (the denominator of fiat-quoted `usd_volume`) is no longer reverted by the next live refresh. |
| 0142 | Widens `derive_generation` from `int4` to `bigint` on the six protocol tables created by 0127 to 0132, matching every other generation column; a monotonic counter must not overflow. |
| 0143 | Stores dashboard sessions keyed by a hash of a random cookie token instead of the raw primary key, so leaked rows cannot be replayed. Everyone must sign in again. |
| 0144 | Adds the `account_observer_watermark` singleton (id=1): the account observer's true processed-ledger watermark, so the XLM supply freshness gate stays fresh when quiet and fails closed when the observer is dead. |
| 0145 | Admits `treasury_updated` into the `credit_events.event_type` CHECK so the sorocredit treasury-rotation event is recorded verbatim in `attributes` instead of tripping `recognition_ok=FALSE`. |
| 0146 | Adds the `defindex_fees` hypertable for the DeFindex vault `dfees` event (`distributed_fees`: a vector of token and i128 amount), which was recognised but dropped. |
| 0147 | Makes open and close deterministic in the nine price aggregates by tie-breaking same-ledger trades, and computes VWAP as an exact single division. Created `WITH NO DATA`; needs full re-materialization. |
| 0148 | Widens the `divergence_observations.reference` CHECK to admit `synthetic-usd-cross`, the second lens for non-USD fiat pairs. Decompresses chunks around the change; keep `divergenceReferences` in `anomalies.go` in lockstep. |
| 0149 | Adds the worker-maintained `asset_volume_character` rollup behind the `volume_character` label and account-structure signals (makers, takers, self-cross share) on asset endpoints, from a trailing 14-day roll over `trades`. |
| 0150 | Adds nullable `trades.signer`, the transaction source account behind an AMM or Soroban swap, since AMM decoders set `taker` to the router. On a populated node `trades_signer_idx` is a plain in-transaction build that blocks writes: apply it with ingest writers stopped after checking `pg_stat_activity`, and Do NOT pre-build it `CONCURRENTLY` (no `IF NOT EXISTS`, so the migration would fail dirty). |
| 0151 | Re-issues twelve wrong `COMMENT ON` strings (for example on `soroban_events.topics_xdr`, `aquarius_protocol_fee.recipient`, `trades.usd_volume` and `oracle_updates.contract_id`). Comment-only; no schema change. |
| 0152 | Drops six never-wired scaffold tables (`wasm_versions`, `contract_wasm_history`, `tvl_observations`, `anchors`, `classic_asset_stats_5m`, `aggregator_exposures`) and the four Stripe claim and dead-letter columns and their indexes from 0118 and 0121. The up refuses if any table holds a row. |
| 0153 | Adds `auth_flags_source` and `auth_flags_as_of_ledger` to `issuers`, so flags recovered from a removed account's last-known state are labelled historical rather than served as current. Existing rows backfill to `live`. |
| 0154 | Adds `asset_price_snapshot`: the per-asset headline USD price, 1h/24h/7d change and backing source count behind the asset listing. The listing requires a fresh `computed_at`, so a wedged aggregator serves no price. |
| 0155 | Adds `projection_verified_from` to `completeness_snapshots`: the lowest ledger the served tier holds for a source, so coverage endpoints stop claiming completeness from genesis. Default 0 means no floor recorded. |
| 0156 | Attaches a 90-day retention policy to `prices_1m`, created `scheduled => false` so nothing drops until an operator arms it. Arming drops all older chunks; match `hypertable_name = 'prices_1m'`. |
| 0157 | Adds the `upshift_vault_events` hypertable for Upshift vault events (deposit, withdraw, share transfer, `deployed_assets_changed`) on earnUSDC and earnXLM. Created empty; fill with `projector-replay -source upshift -from 62623313`. |
| 0158 | Splits `classic_assets` provenance into trade and holding by adding nullable `first_trade_*`, `last_trade_*`, `first_holding_*` and `last_holding_*` columns, so held-but-never-traded assets can be registered. |


| 0159 | Adds a SEP-1 refresh retry ladder: nullable `sep1_consecutive_failures` and `sep1_next_attempt_after` on `issuers`, plus `issuers_sep1_refresh_queue_idx`, so domains that keep failing are re-queued less eagerly. The backoff schedule lives in the writer. |
| 0160 | Adds `asset_listing_directory`, the cached Stellar slice of CoinGecko's platform-to-address map (Soroban C-strkeys and classic `CODE-GISSUER` pairs), synced hourly by `listing-sync`. It corroborates an address; it is not an identity attestation. |
| 0161 | Adds `rwa_curated_directory`: a third party's list of tokenized real-world-asset addresses and its own USD price per token, keyed on `(curator, address)`. Rows are the curator's word only. |
| 0162 | Adds `curated_rwa_published_series`, the totals a curator publishes about Stellar RWAs (monthly market-cap series and its split by subclass), because the per-asset data behind 0161 is private and unreadable. |
| 0163 | Adds nullable `window_ladders jsonb` to `freeze_events`, keyed by window seconds, so each (pair, window) keeps its own durable freeze ladder. Key `"0"` holds a pre-0163 ladder with no recorded owner. |
| 0164 | Deletes the `event_index = 0` legacy rows of `cctp_events` and `rozo_events` so replay cannot double-count. Requires rebuilding `-source cctp -from 62146641` and `-source rozo -from 60829397` after deploy. |
| 0165 | Widens the `prices_1m` refresh policy `start_offset` from 5 to 15 minutes, because drained trades after a lake hole can carry close times about 10 minutes old, past the old lookback. |
| 0166 | Applies the 0.01 USD notional floor to the TWAP chain: `prices_1m` gains `notional_trade_count`, and `twap_1h` and `twap_1d` average only minutes clearing the floor. Leaves all three EMPTY; re-materialize. |
| 0167 | Attaches a 12-month retention policy to `usage_daily`, which 0071 left unbounded although each row is one account's per-endpoint request history. Armed on apply; it drops nothing yet. |
| 0168 | Re-enables compression on `asset_supply_history` only if it is disabled, recovering from a failed post-`COMMIT` statement in 0030. A no-op wherever 0030 applied cleanly; the down is a no-op. |
| 0169 | Adds nullable `window_seconds` to `price_source_contributions` and a unique key `(asset_id, quote_id, window_seconds, source, bucket)`, so 5m, 1h and 24h breakdowns differ. Adds a 90-day retention policy shipped disabled. |
| 0170 | Adds `account_directory.override_reason` with CHECK `account_directory_override_reason_chk`: an `operator-override` row must carry a non-blank reason and other rows none, so lifting a scam flag always records why. |
| 0171 | Widens to 7-day chunks the nine hypertables 0062 missed (`oracle_updates`, `api_usage_events`, `soroswap_skim_events`, `blend_positions`, `blend_emissions`, `blend_admin`, `sep41_transfers`, `blend_backstop_events`, `aquarius_rewards_events`). Future chunks only. |
| 0172 | Pins `timescaledb.materialized_only = true` on the 18 served aggregates (`prices_*`, `twap_1h`, `twap_1d`, `oracle_prices_*`, `supply_1d`, `dex_volume_by_pair_1d`), the property the closed-bucket invariant rests on. `source_volume_1h` and `pools_per_source_1h` stay real-time. No re-materialization. |
| 0173 | Re-issues the `COMMENT ON` for `divergence_observations.status` to describe the real service-wide threshold and the warning-flag conditions. Catalog-only. |
| 0174 | No-op since v0.92.1. As first shipped it decompressed `sep41_transfers` to add `sep41_transfers_amount_check`; that contract is now enforced only in Go by `validateSEP41TransferRows`. Databases that applied the original keep the CHECK. |
| 0175 | Adds `usd_volume_restamp_log`: one row per `trades.usd_volume` rewrite by `usd-volume-restamp`, holding the trade key, prior value and generation, and what was written, for undo. The down refuses while it holds rows. |
| 0176 | Re-issues the `COMMENT ON` for `oracle_updates`: the primary key also carries `ts`, so replay after a `ts` fix must delete the stale row first. Catalog-only. |
| 0177 | Adds `account_directory.override_by` with CHECK `account_directory_override_by_chk`: an `operator-override` row must name a non-blank operator and other rows none, the same shape as 0170. Existing override rows get a placeholder. |
| 0178 | Adds `issuers.sep1_payload_fetched_at`, stamped only on a successful SEP-1 fetch, so the RWA membership scan ignores payloads older than 30 days. Rows with a failing streak stay NULL. |
| 0179 | Enforces `audit_log` as append-only: `audit_log_append_only()` behind row and `BEFORE TRUNCATE` triggers refusing with SQLSTATE 42501. The only allowed update is the `ON DELETE SET NULL` of account and user ids. |
| 0180 | Adds the unique index `customer_webhooks_account_url_key` on `customer_webhooks (account_id, url)`, so one account cannot register the same destination twice. A pre-flight refuses if duplicates exist; nothing is merged or deleted. |
| 0181 | Raises every `price_alerts.cooldown_seconds` below 300 to 300, matching the API floor, since a 0 cooldown made the evaluator enqueue webhooks on every tick. Data only; the down is a no-op. |
| 0182 | Re-issues the `COMMENT ON` for `ledger_ingest_log` and comments `persisted_at`: the table is a substrate-continuity and census record, not a persistence marker. Catalog-only. |
| 0183 | Adds nullable `lake_verified_through` and `holders_retracted` to `sac_balance_seed_provenance`. A full-history row with NULL `lake_verified_through` predates the check and must be re-seeded. |
| 0184 | Adds `claimable_seed_provenance`: one row per classic asset recording the latest complete `supply seed-claimable-balances` pass (seeded and retracted balances, ledger range, `lake_verified_through`). Audit trail only. |
| 0185 | Data-only: deletes `mev_events` `sandwich` and `oracle_sandwich` rows whose `detail.legs` do not prove opposite-direction brackets under the detectors' current rule. The down is a no-op. |
| 0186 | Adds nullable `ref_observed_at` to `divergence_observations`: when the reference price was observed, beside `observed_at`, so a row shows how stale the reference was. Older rows stay NULL. |
| 0187 | Recreates the price aggregates (`prices_*`, `twap_1h`, `twap_1d`, `pools_per_source_1h`) with price expressions restricted to rows where both legs are above zero, ahead of admitting zero-leg SDEX trades. Adds `volume_quote` and `volume_priced`. |
| 0188 | Adds account erasure: `audit_log_erase_metadata(jsonb)` strips personal fields, and `audit_log_append_only()` permits that rewrite (and nulling `ip` and `user_agent` on non-staff rows) only for a closed account being erased. Adds `erased_account_slugs`. |
| 0189 | Adds `account_observation_seed_provenance`: a singleton recording the latest complete `supply seed-observations` pass over the SDF reserve watchlist (watched, missing and removed accounts, counts, ledger range). |
| 0190 | Adds `issuer_identity_history`: one row per change to an established issuer's SEP-1 identity (`org_name`, `org_url`, `org_logo` or `currency_image`), with old and new values. No FK, so history outlives the issuer. |
| 0191 | Drops the inline `> 0` CHECKs on `trades.base_amount` and `trades.quote_amount` so a fill with one leg rounded to zero can be stored. `canonical.Trade.Validate` enforces both legs `>= 0`. |
| 0192 | Adds `defindex_admin_events`: one row per DeFindex vault admin event (`rescue`, `paused`, `unpaused`, `nreceiver`, `nmanager`, `nemanager`, `rbmanager`) with per-kind CHECKs. Hypertable with 30-day chunks and compression. Fill with `projector-replay -source defindex`. |
| 0193 | Adds the vendor-time FX bars table (ticker units per 1 USD, hourly where Massive has them, daily otherwise), written by the forex worker and `fx-history-backfill --series=fixings`. |
| 0194 | Re-issues the stored `COMMENT ON` text for `sep41_supply_rollup` and `last_ledger`: names the three writers, what `last_ledger = 0` means, and that the table is never truncated. Catalog-only. |
| 0195 | Admits the Phoenix stake-lifecycle events into `phoenix_stake_events.action` (and makes `lp_token` nullable), and `factory_config_updated` plus three `blend_set_*` settings into `phoenix_admin_events.admin_action`, adding a nullable NUMERIC `value`. Fill with `projector-replay -source phoenix`. |
| 0196 | Adds nullable `trades.tx_index`, the 0-based intra-ledger apply order of an on-chain trade's transaction, filled after insert from the lake by `RunTxIndexTagger` and `tag-tx-index`. Takes a brief ACCESS EXCLUSIVE lock. |
| 0197 | Raises the `trades` compression policy's `compress_after` from 7 to 15 days, past the asset-character roll's 14-day window, so compression never needs a chunk the roll is reading. |
| 0198 | Adds `price_alerts.disarmed boolean NOT NULL DEFAULT false` so an alert fires once per crossing instead of every cooldown. Firing sets it; the evaluator clears it when the condition stops holding. |
| 0199 | Adds `walk_version smallint NOT NULL DEFAULT 0` to the five balance-observation hypertables, so a re-derive under a newer entry walk replaces rows an older walk numbered higher. |
| 0200 | Adds `customer_webhooks.previous_secret` and `previous_secret_expires_at` (set or cleared together), so a signing secret rotates in place and the worker signs with both keys until expiry, keeping queued deliveries. |
| 0201 | Adds `completeness_snapshots.projection_reconciled_from` (lowest ledger the run's reconcile covered) and nullable `projection_evidenced_at` (when one run last reconciled the whole served range), so a carried-forward claim does not read as freshly proven. |
| 0202 | Adds `sushiswap_v3_pools` (pool, factory, tokens, fee tier, tick spacing, creation ledger), written from factory `pool_created` events and read at boot to restore the decoder's pool-to-token map. |
| 0203 | Adds the `sushiswap_v3_position_events` hypertable: one row per SushiSwap V3 pool `mint`, `burn` or `collect`. Requires `projector-replay -source sushiswap_v3 -from 61487379` after decompressing `trades` chunks. |
| 0204 | Adds `customer_webhooks.signing_key_sealed` and `previous_signing_key_sealed` (AES-256-GCM, row id as associated data), makes `secret_hash` nullable, and adds CHECK `customer_webhooks_signing_key_present`. Down refuses while any row is sealed-only. |
| 0205 | Replaces the built-in `trades` compression policy job with the custom job `trades_compression_policy`, which sets a 5 s `lock_timeout`, so a long reader makes it retry instead of queueing later readers. |
| 0206 | Deletes the `projected-rebuild` checkpoints (`ingestion_cursors`) for comet, cctp, rozo and sushiswap_v3 left by 0137, 0164 and 0203, because `-resume` skipped those windows without looking at the emptied tables. |
| 0207 | Release N+2 of 0169: deletes pre-0169 NULL-window `price_source_contributions` rows, sets `window_seconds` NOT NULL with `CHECK (> 0)`, and drops the 0026 primary key. Run off-peak. |
| 0208 | Adds nullable `released_by text` to `freeze_events`: `operator:<actor>` or `system:recovery` for who closed the freeze. NULL while open or closed before 0208; no CHECK, and no API read path selects it. |
| 0209 | Adds nullable `oracle_updates.published_price numeric`: the publisher's verbatim integer when `price` is an inverted, lossy reciprocal (RedStone and chainlink invert feeds). NULL means none recorded. Fill history with `projected-rebuild -source redstone`. |
| 0210 | Adds the `spectra_events` hypertable (market discovery, PT, YT, IBT and limit-order events) and the plain `spectra_markets` table (one row per PT). Per-kind CHECKs pin each kind's columns. Created empty. |
| 0211 | Adds `cagg_late_refresh_windows`: one row per `(family, view)` holding the trade-time window of late-written rows not yet refreshed, so a crash cannot drop a pending refresh. Only `trades` has a writer. |
| 0212 | Adds `asset_volume_24h.unpriced_trades bigint NOT NULL DEFAULT 0`: the trailing-24h count of trades with no trade-time `usd_volume`. The asset endpoints serve `volume_lower_bound: true` when it is above 0. |
| 0213 | Adds `apply_api_role_grants()` (SECURITY DEFINER): gives `stellarindex_api` SELECT on all public relations and write access only to the 19 tables the API writes. New API-written tables must be added to it. |
| 0214 | Pins `apply_api_role_grants()` to `search_path = pg_catalog, public, pg_temp`, so the SECURITY DEFINER function cannot resolve objects through a schema another role can write to. |
| 0215 | Adds `trustline_balance_current` and `trustline_balance_current_seeded`, kept by triggers on `trustline_observations`, so classic supply sums read the latest row per holder instead of scanning history. |
| 0216 | Re-creates `divergence_observations_reference_check` to admit `chainlink-stellar`, which the recorder writes but 0148's list omitted. Decompresses chunks first; the down refuses while such rows exist. |
| 0217 | Adds `trade_sums_1m`, a continuous aggregate of priceable `trades` per (minute, source, base, quote), so a capped window's older minutes can fill 1h/24h VWAP. Retention of 7 days is shipped disabled. |
| 0218 | Adds the `classic_observer_watermark` singleton (id=1): the classic-supply observers' processed-ledger watermark, so a quiet component table cannot freeze the supply gate's anchor. |
| 0219 | Adds `bound_ledger` and `bound_observed_at` to `sep41_supply_rollup`, so the delta read can prune `sep41_supply_events` chunks by `observed_at`. The reader applies the bound only while `bound_ledger = last_ledger`. |
| 0220 | Adds `lp_reserve_current` and `lp_reserve_current_seeded`, kept by a trigger on `lp_reserve_observations`, so LP reserve sums read the current row per asset and pool. Same design as 0215. |
| 0221 | Adds nullable `trades.usd_basis text`, the valuation route behind `usd_volume` (`quote_pegged`, `base_pegged`, `xlm_base`, `xlm_quote`, `fiat_peg`, `fx_quote`, `fx_base`), plus `prior_usd_basis` and `usd_basis` on `usd_volume_restamp_log` for undo. |
| 0222 | Adds `sac_balance_current` and `sac_balance_current_seeded`, kept by a trigger on `sac_balance_observations`, so SAC balance sums read the current row per holder. Same design as 0215. |

## References

- [ADR-0003 i128 no-truncation](../docs/adr/0003-i128-no-truncation.md)
- [ADR-0006 TimescaleDB](../docs/adr/0006-timescaledb-for-price-time-series.md)
- [HA plan §3.3](../docs/architecture/ha-plan.md) — hypertable + retention design
- [Coverage matrix S6/S7](../docs/architecture/coverage-matrix.md) — requirement rows mapping to these schemas
