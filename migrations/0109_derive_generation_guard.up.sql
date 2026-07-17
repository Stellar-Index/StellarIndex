-- 0109 up — add `derive_generation` to the three core served-tier
-- derived-value tables (trades, oracle_updates, asset_supply_history)
-- so a corrected re-derive can UPDATE a wrong value in place instead of
-- silently no-op'ing (audit-2026-07-16 M1 / INV-3 — the re-derive trap,
-- the keystone fix for the re-backfill treadmill).
--
-- Before this migration the writers used `ON CONFLICT (...) DO NOTHING`
-- with the derived value OUTSIDE the conflict key, so a re-derive of a
-- corrected value was discarded (rowsInserted=0) — the ONLY way to fix a
-- wrong money value was DELETE/TRUNCATE + a full re-backfill. The writers
-- now use a generation-guarded idempotent-corrective upsert:
--   ON CONFLICT (...) DO UPDATE SET <every value col> = EXCLUDED.<col>,
--                                   derive_generation  = EXCLUDED.derive_generation
--     WHERE <table>.derive_generation <= EXCLUDED.derive_generation
-- so a higher-or-equal generation wins and a lower generation can never
-- revert a correction.
--
-- derive_generation is a MONOTONIC COUNTER, not a money value (NUMERIC-
-- lint: bigint is correct here — it is not a stroop/price/supply amount,
-- so it is deliberately NOT numeric):
--   * 0 (the DEFAULT) is the LIVE-ingest generation. Live retries /
--     replays write 0; a gen-0 write can never revert a correction
--     (gen N>0) because of the `<=` guard, and a gen-0 write over an
--     existing gen-0 row just re-writes the identical value (harmless).
--   * a POSITIVE generation is stamped only by the re-derive entry points
--     (stellarindex-ops backfill-external / backfill-chainlink / supply
--     snapshot / ch-rebuild / projected-rebuild), whose corrections must
--     win over the live value. The entry points use time.Now().Unix(), so
--     a later re-derive supersedes an earlier one.
--
-- Additive with a DEFAULT so the currently-deployed binary (whose INSERT
-- column list doesn't mention this column) keeps working unmodified —
-- old-binary-safe per repo convention (cf. 0108). Existing rows backfill
-- to 0, which is exactly the live-ingest generation, so the first
-- corrected re-derive (gen N>0) wins over every pre-existing row.
--
-- TimescaleDB 2.11+ (r1 runs 2.26) supports ADD COLUMN with a DEFAULT on
-- a compressed hypertable directly — no decompress/recompress dance and
-- no chunk rewrite, so this is cheap even on the large trades /
-- oracle_updates hypertables (contrast 0057, which decompresses only
-- because it also swaps a PRIMARY KEY constraint).

BEGIN;

ALTER TABLE trades
    ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;

ALTER TABLE oracle_updates
    ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;

ALTER TABLE asset_supply_history
    ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;

COMMENT ON COLUMN trades.derive_generation IS
    'Monotonic re-derive generation (audit-2026-07-16 INV-3). 0 = live '
    'ingest; a positive value is stamped by an ops re-derive so its '
    'corrected value wins the ON CONFLICT guard '
    '(derive_generation <= EXCLUDED.derive_generation). A lower '
    'generation can never revert a higher-generation correction.';
COMMENT ON COLUMN oracle_updates.derive_generation IS
    'Monotonic re-derive generation (audit-2026-07-16 INV-3). See '
    'trades.derive_generation.';
COMMENT ON COLUMN asset_supply_history.derive_generation IS
    'Monotonic re-derive generation (audit-2026-07-16 INV-3). See '
    'trades.derive_generation.';

COMMIT;
