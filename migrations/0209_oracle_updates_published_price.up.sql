-- 0209 — oracle_updates.published_price: the publisher's own integer when
-- `price` was derived from it by inversion (INV-2590).
--
-- RedStone MXNe and chainlink Invert feeds store the 8-dp reciprocal as
-- `price`. 1/x at fixed scale is lossy (r=1740000000 and r=1740000001 both
-- store 5747126), so the on-chain integer is otherwise unrecoverable from
-- the served tier. NUMERIC, never float (ADR-0003).
--
-- Nullable with no DEFAULT: old-binary-safe (its INSERT omits the column)
-- and metadata-only. TimescaleDB 2.11+ (r1 runs 2.26) adds a column to a
-- compressed hypertable without decompressing, per 0109's precedent on
-- this same table. Existing MXNe rows stay NULL until the operator runs
-- the projected-rebuild named in the PR's Replay-Plan.

BEGIN;

SET LOCAL lock_timeout = '5s';

ALTER TABLE oracle_updates
    ADD COLUMN published_price numeric;

COMMENT ON COLUMN oracle_updates.published_price IS
    'Publisher''s own integer at `decimals` scale, verbatim, when `price` '
    'was derived from it by inversion (RedStone / chainlink Invert feeds). '
    'NULL = no separate published value recorded, NOT that `price` is '
    'verbatim: ecb / exchangeratesapi invert at another scale and leave it NULL.';

COMMIT;
