-- 0141 up — bring `fx_quotes` into the INV-3 generation-guard family
-- (audit-2026-08-14 MR-1). fx_quotes.rate_usd is the denominator of every
-- fiat-quoted usd_volume (read via fxQuotesSnapAtOrBefore), yet its upsert
-- was the ONLY money-value derived writer the INV-3 fix (migrations
-- 0109/0110) skipped: `ON CONFLICT (ticker,bucket) DO UPDATE` with the
-- derived value OUTSIDE any generation guard, i.e. pure arrival-order
-- last-writer-wins. So an operator CORRECTION written by
-- scripts/ops/fx-history-backfill (source='frankfurter-historical') over a
-- key the live worker owns (source='massive') was silently reverted by the
-- next daily worker refresh — the exact "a correction is not durable"
-- class the generation guard was built to close on trades.
--
-- This migration adds the schema half. The writer (InsertFXQuoteBatch) now
-- issues the generation-guarded corrective upsert:
--   ON CONFLICT (ticker, bucket) DO UPDATE SET <value cols> = EXCLUDED.<col>,
--                                   derive_generation = EXCLUDED.derive_generation
--     WHERE fx_quotes.derive_generation <= EXCLUDED.derive_generation
-- so a higher-or-equal generation wins and a lower generation can never
-- revert a correction.
--
-- derive_generation is a MONOTONIC COUNTER, not a money value (NUMERIC-
-- lint: bigint is correct — it is not a rate/price/supply amount, so it is
-- deliberately NOT numeric):
--   * 0 (the DEFAULT) is the LIVE-ingest generation — what the forex
--     worker stamps. A gen-0 write can never revert a correction (gen N>0)
--     because of the `<=` guard, and a gen-0 write over an existing gen-0
--     row just re-writes the same value (harmless — worker idempotency).
--   * a POSITIVE generation is stamped only by the operator backfill entry
--     point (scripts/ops/fx-history-backfill, time.Now().Unix()), whose
--     corrections must win over — and survive — the live worker value.
--
-- Additive with a DEFAULT so the currently-deployed binary (whose INSERT
-- column list doesn't mention this column) keeps working unmodified —
-- old-binary-safe per repo convention (cf. 0109/0110). Existing rows
-- backfill to 0 (the live-ingest generation), so the first operator
-- correction (gen N>0) wins over every pre-existing row.
--
-- fx_quotes is a compressed hypertable (0028 sets a 90-day compression
-- policy). TimescaleDB 2.11+ (r1 runs 2.26) supports ADD COLUMN with a
-- DEFAULT on a compressed hypertable directly — no decompress/recompress
-- dance and no chunk rewrite (same rationale as 0109/0110).

BEGIN;

ALTER TABLE fx_quotes
    ADD COLUMN derive_generation bigint NOT NULL DEFAULT 0;

COMMENT ON COLUMN fx_quotes.derive_generation IS
    'Monotonic re-derive generation (audit-2026-08-14 MR-1, INV-3 family). '
    '0 = live forex-worker ingest; a positive value is stamped by the '
    'operator fx-history-backfill tool so its corrected rate wins the ON '
    'CONFLICT guard (derive_generation <= EXCLUDED.derive_generation) and '
    'cannot be reverted by a later live gen-0 worker refresh. See '
    'trades.derive_generation (migration 0109).';

COMMIT;
