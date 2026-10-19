-- 0221 up — `trades.usd_basis`: which valuation route produced a trade's
-- stored `usd_volume` (quote_pegged, base_pegged, xlm_base, xlm_quote,
-- fiat_peg, fx_quote, fx_base).
--
-- WHY: usd_volume alone does not say which leg was priced or how. A bounded
-- route stores the SMALLER of two leg values, so the leg the number came
-- from varies row by row and cannot be re-derived from the pair. The writer
-- (tradeUSDVolumeCredited) now returns the route with the value; the
-- generation-aware UPSERT keeps usd_basis with whichever usd_volume its
-- CASE kept, and the restamp writes it with each rewrite.
--
-- Nullable + no default = catalog-only, including on compressed chunks
-- (TestMigration0221_AddsUSDBasisOverCompressedChunks in
-- test/integration/migrations_test.go asserts every chunk is still
-- compressed after this file). No CHECK: it would validate
-- against compressed chunks; the closed set is the Go USDBasis type.
--
-- Lock: ACCESS EXCLUSIVE on trades, as 0196 — sub-second, but it queues
-- behind any long reader, compression run or CAGG refresh. The deploy runs
-- migrate under PGOPTIONS lock_timeout; if it trips, schema_migrations is
-- DIRTY at 221 with the DDL rolled back: `stellarindex-migrate force 220`
-- and re-run in a quiet window.
--
-- Binary order: deploy-binary.yml migrates before restarting any binary,
-- so the new INSERT never meets the old schema. Old-binary-safe (rule 9)
-- while it still runs: it names no usd_basis, so its rows are NULL and its
-- DO UPDATE leaves the column alone. After a binary rollback (the schema
-- stays, rule 9) a row the old binary re-upserts keeps the basis of the
-- value it replaced; a later restamp or re-derive rewrites both. Do not run
-- the down for this: it drops every basis.
--
-- NULL = no usd_volume, or written before 0221. Historical rows gain a basis
-- when a restamp or re-derive rewrites them; no reader keys on it yet.
--
-- usd_volume_restamp_log gains prior_usd_basis / usd_basis. Undoing a run
-- is 0175's recipe with one more SET column, so the label goes back with
-- the value:
--
--   SET usd_volume = l.prior_usd_volume,
--       usd_basis = l.prior_usd_basis,
--       derive_generation = l.prior_derive_generation
--
-- Log rows written before 0221 have NULL in both, so undoing a pre-0221
-- run that way leaves usd_basis NULL, which is its pre-0221 value.

BEGIN;

ALTER TABLE trades ADD COLUMN usd_basis text;

COMMENT ON COLUMN trades.usd_basis IS
    'Valuation route of usd_volume: quote_pegged, base_pegged, xlm_base, '
    'xlm_quote, fiat_peg, fx_quote or fx_base. Written with usd_volume by '
    'the UPSERT and the restamp. NULL = no usd_volume, or written before 0221.';

ALTER TABLE usd_volume_restamp_log
    ADD COLUMN prior_usd_basis text,
    ADD COLUMN usd_basis       text;

COMMIT;
