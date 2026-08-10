-- 0139 down: drop the additive token column.
ALTER TABLE aquarius_protocol_fee
    DROP COLUMN IF EXISTS token;
