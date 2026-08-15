-- 0142 down — narrow `derive_generation` back to int4 on the six
-- protocol tables (reverse of 0142 up).
--
-- Rollback-only, dev/local iteration lever (migrations/README.md rule 9:
-- down.sql is NOT a production rollback path — the deploy pipeline never
-- auto-runs `migrate down`). Same compressed-hypertable idiom as the up:
-- decompress every chunk first (a column-type change is rejected while
-- compressed chunks exist), leave the compression settings + policy
-- untouched.
--
-- NARROWING bigint -> int4 is the one direction that is NOT
-- unconditionally safe: an int4 column cannot hold a value >= 2^31. At
-- any realistic rollback time (well before the 2038-01-19 unix cliff)
-- every stored derive_generation is a present-day epoch stamp (~1.7e9)
-- or 0, all < 2^31, so the narrowing loses no data. A rollback attempted
-- AFTER a post-2038 value has been written would raise "integer out of
-- range" here and must not be run — which is correct: this is a dev
-- reversibility aid, and the only reason to widen in the first place was
-- that such values become real after 2038. The migration-compat:ok
-- marker is inert on down files (the gate scans *.up.sql only) and is
-- recorded here purely to document this rollback-only justification.

SELECT decompress_chunk(c, true) FROM show_chunks('soroswap_liquidity') c;
ALTER TABLE soroswap_liquidity ALTER COLUMN derive_generation TYPE integer;  -- migration-compat:ok rollback-only narrowing; safe pre-2038 (all stored values < 2^31), never run in production (README rule 9)

SELECT decompress_chunk(c, true) FROM show_chunks('aquarius_reserves_sync') c;
ALTER TABLE aquarius_reserves_sync ALTER COLUMN derive_generation TYPE integer;  -- migration-compat:ok rollback-only narrowing; safe pre-2038 (all stored values < 2^31), never run in production (README rule 9)

SELECT decompress_chunk(c, true) FROM show_chunks('aquarius_protocol_fee') c;
ALTER TABLE aquarius_protocol_fee ALTER COLUMN derive_generation TYPE integer;  -- migration-compat:ok rollback-only narrowing; safe pre-2038 (all stored values < 2^31), never run in production (README rule 9)

SELECT decompress_chunk(c, true) FROM show_chunks('aquarius_kill_switches') c;
ALTER TABLE aquarius_kill_switches ALTER COLUMN derive_generation TYPE integer;  -- migration-compat:ok rollback-only narrowing; safe pre-2038 (all stored values < 2^31), never run in production (README rule 9)

SELECT decompress_chunk(c, true) FROM show_chunks('phoenix_initialize') c;
ALTER TABLE phoenix_initialize ALTER COLUMN derive_generation TYPE integer;  -- migration-compat:ok rollback-only narrowing; safe pre-2038 (all stored values < 2^31), never run in production (README rule 9)

SELECT decompress_chunk(c, true) FROM show_chunks('phoenix_admin_events') c;
ALTER TABLE phoenix_admin_events ALTER COLUMN derive_generation TYPE integer;  -- migration-compat:ok rollback-only narrowing; safe pre-2038 (all stored values < 2^31), never run in production (README rule 9)
