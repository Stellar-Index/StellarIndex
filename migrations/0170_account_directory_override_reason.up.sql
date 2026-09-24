-- 0170 up — account_directory.override_reason: the recorded
-- justification for an operator override (GH #858).
--
-- A row with source = 'operator-override' is an operator's decision to
-- stop honouring a third-party scam-class tag: the issuer's price and
-- market cap are published again, its assets leave the bottom rank tier
-- and the explorer's flag pill goes. Without a reason the row says only
-- THAT someone lifted a safety gate, never why, so it cannot be reviewed.
--
-- The CHECK makes the reason part of the ownership: an override row
-- must carry a non-blank reason and an upstream row must carry none (a
-- reason left behind on an upstream row would describe a decision that
-- no longer holds). Override rows written before this migration are
-- backfilled with an explicit placeholder so the constraint validates;
-- that placeholder is the marker an operator greps for to re-justify
-- them.
ALTER TABLE account_directory
    ADD COLUMN IF NOT EXISTS override_reason text;

UPDATE account_directory
   SET override_reason = 'recorded before override reasons were required (migration 0170)'
 WHERE source = 'operator-override'
   AND (override_reason IS NULL OR btrim(override_reason) = '');

ALTER TABLE account_directory
    DROP CONSTRAINT IF EXISTS account_directory_override_reason_chk;
ALTER TABLE account_directory
    ADD CONSTRAINT account_directory_override_reason_chk CHECK (  -- migration-compat:ok a released binary writes only upstream rows (override_reason NULL); no released binary writes operator-override rows
        CASE WHEN source = 'operator-override'
             THEN override_reason IS NOT NULL AND btrim(override_reason) <> ''
             ELSE override_reason IS NULL
        END
    );
