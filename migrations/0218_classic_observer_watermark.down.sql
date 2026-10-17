-- 0218 down — drop the classic observer watermark table. The reader falls back
-- to MIN(MAX(ledger)) over the component tables; the row is a live cursor the
-- indexer re-establishes on its next ledger.

BEGIN;

DROP TABLE IF EXISTS classic_observer_watermark;

COMMIT;
