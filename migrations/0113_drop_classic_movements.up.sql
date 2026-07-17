-- 0113 up — drop the dead classic_movements hypertable
-- (audit-2026-07-16 C2-18 / DAT-03).
--
-- The classic_movements table (migration 0105, ADR-0047 D1) was
-- SUPERSEDED by ADR-0048 D2 (2026-07-10): the pre-P23 classic-movement
-- archive moved to ClickHouse-native stellar.account_movements
-- (deploy/clickhouse/tier1_schema.sql,
-- internal/storage/clickhouse/account_movements.go). Since then the
-- `stellarindex-ops classic-movements-backfill` command writes ONLY to
-- ClickHouse and opens no Postgres connection at all — this table has
-- had no live writer or reader and is intentionally UNPOPULATED. The
-- 0105 row in migrations/README.md promised exactly this cleanup ("a
-- future cleanup migration drops this table once the ClickHouse path is
-- proven"); this is that migration. The now-caller-less Go store
-- (internal/storage/timescale/classic_movements.go) is removed in the
-- same change.
--
-- DESTRUCTIVE + operator-reviewed: applies POST-Phase-0 only. DROP TABLE
-- removes the hypertable, its chunks, indexes and compression policy in
-- one statement (the same one-statement teardown 0105's own down relies
-- on). No CAGG, view, or FK references this table, so CASCADE is not
-- needed. Because the table is UNPOPULATED by design, dropping it loses
-- no data; the .down.sql recreates the exact 0105 schema for
-- reversibility.

BEGIN;

DROP TABLE IF EXISTS classic_movements;

COMMIT;
