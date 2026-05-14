-- 0031 up — remove the 90-day retention policy on `trades` and the
-- 30-day retention on `prices_1m` / `prices_15m`.
--
-- Original design (ADR-0006, migrations 0001 + 0002): raw `trades`
-- aged out at 90 days; minute-grain aggregates at 30 days; only
-- the long-lived hourly+ CAGGs were kept forever.
--
-- Revision (2026-05-14): operator wants every raw trade preserved
-- forever, not just the rolled-up aggregates. Justification:
--   1. Storage was the original concern — 90d × ~1.3M rows/day for
--      SDEX = 40 GB. Forever × 11y = ~1.8 TB raw / ~360 GB
--      compressed. Was assumed to not fit on r1.
--   2. Reality check: r1's postgres data dir is on a 1.5 TB ZFS
--      volume (data/postgres) with 4 % used. The 49 GB OS root was
--      what we were tracking — wrong volume. There's room for a
--      decade of raw trades.
--   3. Customer/audit value: per-trade fidelity (tx_hash,
--      maker/taker, exact ts) is unrecoverable from CAGGs. For
--      regulatory or proof-of-pricing queries we want the raw row.
--
-- Compression policy on `trades` (chunks > 7d) is unchanged —
-- old chunks are still compressed ~5x, so storage growth is
-- bounded and predictable.
--
-- The continuous-aggregate refresh policies on prices_1m /
-- prices_15m / prices_1h / etc are also untouched — those keep
-- materialising as new trades arrive.

BEGIN;

SELECT remove_retention_policy('trades',     if_exists => true);
SELECT remove_retention_policy('prices_1m',  if_exists => true);
SELECT remove_retention_policy('prices_15m', if_exists => true);

COMMIT;
