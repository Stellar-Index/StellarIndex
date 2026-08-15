-- 0142 up — widen `derive_generation` from int4 to bigint on the six
-- protocol tables migrations 0127-0132 created (audit-2026-08-14
-- W1-migrations-3).
--
-- Every OTHER derive_generation column in the tree is bigint: the three
-- core tables (0109) and the 25 protocol tables (0110), and fx_quotes
-- (0141). The six tables added by 0127-0132 drifted to `integer` while
-- their own headers claimed parity with 0110's bigint columns.
--
-- Why this is a latent BREAK, not a cosmetic mismatch: the value is
-- Store.deriveGeneration = time.Now().Unix() (store.go:66, an int64
-- epoch stamp) written by every projector-replay / re-derive. On
-- 2038-01-19 03:14:08 UTC unix time crosses int4 max 2,147,483,647, and
-- Postgres then rejects EVERY projector write to these six tables with
-- "integer out of range" — while the bigint core tables keep accepting.
-- Widening to bigint restores parity and removes the 2038 cliff.
--
-- Compressed-hypertable idiom (mirrors 0053): all six tables are
-- compressed hypertables (0127-0132 each SET timescaledb.compress +
-- add_compression_policy). TimescaleDB refuses a column-type change while
-- COMPRESSED CHUNKS exist ("operation not supported on hypertables with
-- compressed chunks"), so decompress every chunk first. The compression
-- SETTINGS and the compression POLICY are left untouched — verified on
-- TimescaleDB 2.26.4 (r1's version) that ALTER COLUMN … TYPE succeeds
-- with compression still enabled once no compressed chunk remains, and
-- that the policy re-compresses the decompressed chunks on its next tick.
-- show_chunks() returns no rows on a fresh/empty hypertable, so the
-- decompress is a safe no-op there (e.g. the integration container).
--
-- OLD-BINARY-SAFE (rule 9): int4 -> bigint is a WIDENING. The currently
-- released binary binds derive_generation as a Go int64 and only ever
-- writes today's epoch (~1.7e9, < 2^31); both the write and the int64
-- read remain valid against a bigint column, so no released binary is
-- affected. The only cost is the one-time ACCESS EXCLUSIVE table rewrite
-- the widening performs — an ops-timing concern (run in a maintenance
-- window on r1), NOT a backward-compatibility break. The inline
-- migration-compat:ok marker on each ALTER records exactly this.

SELECT decompress_chunk(c, true) FROM show_chunks('soroswap_liquidity') c;
ALTER TABLE soroswap_liquidity ALTER COLUMN derive_generation TYPE bigint;  -- migration-compat:ok int4->bigint WIDENING; released binary writes/reads it as int64 (<2^31), valid in a bigint column — only cost is the rewrite, not a compat break (0110 parity)

SELECT decompress_chunk(c, true) FROM show_chunks('aquarius_reserves_sync') c;
ALTER TABLE aquarius_reserves_sync ALTER COLUMN derive_generation TYPE bigint;  -- migration-compat:ok int4->bigint WIDENING; released binary writes/reads it as int64 (<2^31), valid in a bigint column — only cost is the rewrite, not a compat break (0110 parity)

SELECT decompress_chunk(c, true) FROM show_chunks('aquarius_protocol_fee') c;
ALTER TABLE aquarius_protocol_fee ALTER COLUMN derive_generation TYPE bigint;  -- migration-compat:ok int4->bigint WIDENING; released binary writes/reads it as int64 (<2^31), valid in a bigint column — only cost is the rewrite, not a compat break (0110 parity)

SELECT decompress_chunk(c, true) FROM show_chunks('aquarius_kill_switches') c;
ALTER TABLE aquarius_kill_switches ALTER COLUMN derive_generation TYPE bigint;  -- migration-compat:ok int4->bigint WIDENING; released binary writes/reads it as int64 (<2^31), valid in a bigint column — only cost is the rewrite, not a compat break (0110 parity)

SELECT decompress_chunk(c, true) FROM show_chunks('phoenix_initialize') c;
ALTER TABLE phoenix_initialize ALTER COLUMN derive_generation TYPE bigint;  -- migration-compat:ok int4->bigint WIDENING; released binary writes/reads it as int64 (<2^31), valid in a bigint column — only cost is the rewrite, not a compat break (0110 parity)

SELECT decompress_chunk(c, true) FROM show_chunks('phoenix_admin_events') c;
ALTER TABLE phoenix_admin_events ALTER COLUMN derive_generation TYPE bigint;  -- migration-compat:ok int4->bigint WIDENING; released binary writes/reads it as int64 (<2^31), valid in a bigint column — only cost is the rewrite, not a compat break (0110 parity)
