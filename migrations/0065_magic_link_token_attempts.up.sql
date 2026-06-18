-- 0065 up — `attempts` counter on magic_link_tokens.
--
-- Backs the brute-force guard on the email-code sign-in path
-- (POST /v1/auth/verify-code). The magic LINK consumes by the full
-- 256-bit plaintext, so it needs no attempt cap. The CODE, however, is
-- a 6-digit numeric (≈1e6 space) — without a cap, an attacker who knows
-- a victim's email could grind guesses against any in-flight login
-- token during its 15-minute TTL. This column lets the verify-code
-- handler bound a single token to a handful of wrong guesses: each miss
-- increments attempts, and the handler stops treating the token as a
-- code candidate once attempts crosses the cap (the link still works —
-- the cap gates code matching only).
--
-- smallint + DEFAULT 0 so existing rows backfill to "no attempts yet".
ALTER TABLE magic_link_tokens
    ADD COLUMN attempts smallint NOT NULL DEFAULT 0;
