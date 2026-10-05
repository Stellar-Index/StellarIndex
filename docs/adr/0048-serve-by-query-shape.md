---
adr: 0048
title: Serve by query shape — the account-movement archive is ClickHouse-native
status: Accepted
date: 2026-07-10
supersedes: []
superseded_by: null
---

# ADR-0048: Serve by query shape — the account-movement archive is ClickHouse-native

## Context

ADR-0034 split storage by role, but "served tier = everything the API touches" pulled archive-scale history toward Postgres. Only "everything an address ever did" (`WHERE address = X ORDER BY ledger` over 10–20B immutable rows) exceeds single-host Postgres; ClickHouse sorted by `(address, ledger)` makes it a range read.
Filling Postgres through the projector's live tail is tick-bound; lake-to-lake derivation is an OLAP job.

## Decision

1. **D1: split serving by query shape, not data age.** Postgres/Timescale serves OLTP, pricing aggregates, mutable state and bounded protocol tables. ClickHouse serves archive-scale immutable history through dedicated sorted serving tables, never the raw lake tables.
2. **D2: `stellar.account_movements` is the movement archive** (it replaced ADR-0047's Postgres `classic_movements`, dropped by migration 0113). Two rows per movement, one per participant, with a `direction` of `sent`, `received` or `self`; `ReplacingMergeTree(ingested_at)`; `ORDER BY (address, ledger, tx_hash, op_index, leg_index, direction)`. DDL lives in `deploy/clickhouse/tier1_schema.sql`, not `migrations/`. `classic-movements-backfill` decodes lake rows with `internal/sources/classicmovements` and writes ClickHouse only.
3. **D3: `stellarindex-ops projected-rebuild` is the bulk catch-up for projected sources** past about 1M ledgers; `projector-replay` handles smaller rewinds. It runs parallel, checkpointed, resumable ledger-window workers through the registry-built decoders and sink, writing idempotent rows. It never touches the live projector's cursor: it refuses to run unless that cursor is at or above `-to`, and `-allow-live-overlap` overrides the refusal.
4. **D4: serving protection.** API reads for the explorer and `/v1/accounts/{g}/movements` authenticate as `storage.clickhouse_serving_user` (the ansible role provisions `api_serving`; the Go default is empty and falls back to the live user, else ClickHouse's default user), a dedicated ClickHouse profile with bounded threads, memory and execution time and query priority above merges and backfill inserts. A serving query must never queue behind a derivation; backfills run under `run-heavy-job.sh` and ClickHouse quotas.
5. **D5: the unified account feed** `GET /v1/accounts/{g_strkey}/movements` merges the ClickHouse archive with the Postgres `sep41_transfers` tail at read time.

## Invariant

- `projected-rebuild` refuses a range the live projector cursor is still inside unless `-allow-live-overlap` is passed: `TestCheckLiveCursorGuard` in `internal/ops/chops/projected_rebuild_test.go`.
- The serving profile stays `readonly=2` so readers can still send query settings: `TestServingProfileReadonlyAllowsQuerySettings` and `TestServingReadersStillSendQuerySettings` in `internal/ops/chops/clickhouse_serving_profile_test.go`.
- The account feed merges the ClickHouse archive and the Postgres tail, and the two sides agree on the P23 boundary: `TestExplorer_AccountMovements_Merge` and `TestP23BoundaryConstantsAgree` in `internal/api/v1/explorer_movements_test.go`.
- `classic_movements` stays dropped from Postgres: `TestMigration0113DropsClassicMovements` in `internal/storage/timescale/classic_movements_drop_migration_test.go`.

## Consequences

- Movement re-derivation is a rerunnable ClickHouse job, so decoder-bug corrections in this domain are cheap.
- Postgres growth stays bounded to protocol tables and the pricing working set.
- The feed has a merge seam (ClickHouse archive plus Postgres tail); each side is verified on its own under ADR-0033.
- ClickHouse is a serving dependency for one public surface, so the D4 isolation profile must exist wherever D5 is served.

## Evidence

`deploy/clickhouse/tier1_schema.sql` (`account_movements`), `internal/ops/chops/projected_rebuild.go`, `internal/storage/clickhouse/account_movements.go`, `internal/api/v1/explorer/movements.go`, `configs/ansible/roles/archival-node/tasks/20-clickhouse-serving-profile.yml`.
