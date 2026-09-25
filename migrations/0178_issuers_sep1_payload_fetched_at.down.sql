-- 0178 down — drop the SEP-1 payload fetch time. The payloads themselves
-- are untouched; only the record of their age is lost, and a re-up can
-- re-derive it exactly for every row whose last attempt succeeded.

BEGIN;

ALTER TABLE issuers
    DROP COLUMN IF EXISTS sep1_payload_fetched_at;

COMMIT;
