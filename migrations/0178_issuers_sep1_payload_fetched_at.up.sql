-- 0178 up — record WHEN the cached SEP-1 payload was fetched, so a reader
-- can bound the age of the attestation it acts on.
--
-- WHAT IS WRONG TODAY
--
-- `sep1_resolved_at` means "when the refresh last TRIED" (0159 re-documented
-- it): the cron pre-marks every attempt, and a failure never touches
-- `sep1_payload`. So a home_domain that goes permanently dark keeps its
-- last-fetched payload forever while `sep1_resolved_at` stays fresh, and
-- nothing on the row records how old that payload is. The RWA membership
-- scan (`BoundSep1Currencies`) admits and recognises accounts from that
-- payload, so a dead domain kept doing both for months (GH #1050).
--
-- WHAT THIS MIGRATION ADDS
--
--   sep1_payload_fetched_at   when the payload now in sep1_payload was
--                             fetched. Stamped only by the success writer
--                             (SetIssuerSep1Payload); a failure leaves it,
--                             so it ages while the domain stays dark.
--
-- BACKFILL, and why it is not total. A row holding a payload with no
-- failing streak was last touched by a success, so its sep1_resolved_at IS
-- the payload's fetch time. A row holding a payload with a failing streak
-- was last touched by a failure, and nothing recorded when its payload was
-- fetched: those rows stay NULL, which the reader treats as an attestation
-- of unknown age and does not admit. That is fail-closed and self-healing —
-- the next successful fetch stamps the column.
--
-- Runtime: one catalog-only ADD COLUMN and one UPDATE over the ~36k payload
-- rows of a plain, uncompressed, non-hypertable table. Seconds. Rule-9
-- safe: one nullable column, no default, no rewrite; the previous released
-- binary neither reads nor writes it.

BEGIN;

ALTER TABLE issuers
    ADD COLUMN sep1_payload_fetched_at timestamptz;

COMMENT ON COLUMN issuers.sep1_payload_fetched_at IS
    'When the SEP-1 payload now held in sep1_payload was fetched. Set only by '
    'a successful refresh; a failed attempt leaves it, so it ages while a '
    'domain stays dark. NULL with a payload present = fetch time unknown, '
    'read as stale. Unlike sep1_resolved_at, which every attempt stamps.';

UPDATE issuers
   SET sep1_payload_fetched_at = sep1_resolved_at
 WHERE sep1_payload IS NOT NULL
   AND COALESCE(sep1_consecutive_failures, 0) = 0
   AND sep1_payload_fetched_at IS NULL;

COMMIT;
