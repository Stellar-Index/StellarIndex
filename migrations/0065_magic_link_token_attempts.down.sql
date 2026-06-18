-- 0065 down — drop the magic_link_tokens.attempts counter.
ALTER TABLE magic_link_tokens
    DROP COLUMN IF EXISTS attempts;
