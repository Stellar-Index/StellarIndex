-- 0218 up — `classic_observer_watermark`: the classic-supply observers' processed-ledger
-- watermark, the freshness anchor for classic asset supply.
--
-- Store.MinClassicComponentLedger took MIN over four tables of MAX(ledger)
-- (trustline, claimable, lp_reserve, sac_balance observations). A table only
-- gets a row when a watched entry changes, so a quiet one (claimable balances
-- move rarely) froze the anchor while every observer was healthy, and the
-- supply gate rejected current snapshots as dormant. Same failure and same fix
-- as 0144: the live indexer advances processed_ledger every ledger it drives
-- the classic observers over; a dead indexer stops advancing it and the gate
-- fails closed past the horizon.
--
-- Singleton (id = 1, CHECK); the writer upserts with a monotonic guard. A
-- dedicated table, not an ingestion_cursors row, which feeds
-- LatestKnownLedger, the gap detector and the cursor-lag alerts. With no row
-- the reader falls back to the MIN(MAX(ledger)) query, so the previous binary
-- and a fresh cluster keep today's behaviour. Additive; old-binary-safe.

BEGIN;

CREATE TABLE classic_observer_watermark (
    id                smallint     PRIMARY KEY,
    processed_ledger  bigint       NOT NULL,
    updated_at        timestamptz  NOT NULL DEFAULT now(),

    CONSTRAINT classic_observer_watermark_singleton CHECK (id = 1)
);

COMMENT ON TABLE classic_observer_watermark IS
    'Singleton (id=1) processed-ledger watermark of the classic-supply observers '
    '(trustlines, claimable balances, liquidity pools, SAC balances). Advanced every '
    'live ledger by the indexer; read by Store.MinClassicComponentLedger.';

COMMIT;
