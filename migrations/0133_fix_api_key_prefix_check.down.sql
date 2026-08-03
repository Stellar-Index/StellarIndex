-- 0133 down — restore migration 0027's `rek_`-only prefix CHECK.
--
-- NOTE: this reinstates the constraint that makes API-key issuance fail
-- (every minter emits `sip_`). It also fails outright if any `sip_` row
-- exists, which is the correct behavior for a down-migration that would
-- otherwise leave unrepresentable rows in place — drop or rewrite those
-- rows first if you genuinely need to roll back.
ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS api_keys_key_prefix_check;

ALTER TABLE api_keys
    ADD CONSTRAINT api_keys_key_prefix_check
    CHECK (key_prefix ~ '^rek_[a-f0-9]{8}$');
