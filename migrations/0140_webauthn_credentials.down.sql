-- Reverses 0140: drops the passkey table. Registered passkeys are
-- lost (users fall back to email-code sign-in) — acceptable, since
-- the private keys stay on the users' authenticators and can be
-- re-registered.

DROP TABLE IF EXISTS webauthn_credentials;
