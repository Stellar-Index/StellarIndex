-- 0133 up — correct `api_keys.key_prefix` CHECK for the `sip_` namespace.
--
-- Migration 0027 pinned the prefix regex to the PRE-REBRAND namespace:
--
--     key_prefix text NOT NULL CHECK (key_prefix ~ '^rek_[a-f0-9]{8}$')
--
-- Every minter in the codebase emits `sip_` (Stellar Index Pricing):
-- internal/auth/store.go generateID(..., "sip_", 32) and
-- internal/api/v1/dashboardkeys/handlers.go generatePlaintext(), whose
-- key_prefix is plaintext[:12] = "sip_" + 8 hex. The rename landed in
-- the code; this constraint was never updated with it.
--
-- Effect (cold audit 2026-08-03, verified against r1): EVERY INSERT into
-- api_keys from the Postgres-backed store fails with a check_violation
-- (SQLSTATE 23514), which the handler surfaces as a 500 — dashboard API
-- key issuance is non-functional against Postgres. r1's api_keys table
-- holds 0 rows, consistent with "no key has ever been minted through
-- this path". Fail-closed (no bad data was written), but the feature is
-- dead and no test caught it: the store tests are pure-Go serialization
-- unit tests and the auth-validator tests use an in-memory stub, so the
-- CHECK was never exercised.
--
-- The new regex admits BOTH namespaces: `sip_` is what is minted now,
-- and `rek_` stays admitted so that any archived/legacy row (or an
-- older backup restored into this schema) does not become
-- unrepresentable. Widening a CHECK cannot fail on existing rows.
ALTER TABLE api_keys DROP CONSTRAINT IF EXISTS api_keys_key_prefix_check;  -- migration-compat:ok widening only; re-added immediately below as a strict superset
ALTER TABLE api_keys
    ADD CONSTRAINT api_keys_key_prefix_check  -- migration-compat:ok superset CHECK: admits sip_ in addition to rek_, removes nothing, so every row the previous released binary could write stays valid
    CHECK (key_prefix ~ '^(sip|rek)_[a-f0-9]{8}$');
