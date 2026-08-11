-- 0140: passkey (WebAuthn) credentials for dashboard sign-in.
--
-- Additive only: new table, no changes to existing objects. One row
-- per registered passkey, keyed to the platform user (and via
-- users.account_id to the account). The private key never exists
-- server-side — public_key is the COSE-encoded verification key.
--
-- sign_count is the WebAuthn signature counter (uint32 on the wire;
-- bigint here for headroom). It is a monotonic clone-detection
-- counter, not a token amount — NUMERIC (migrations rule 5 /
-- ADR-0003) does not apply.

CREATE TABLE webauthn_credentials (
    id               uuid PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id          uuid NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name             text NOT NULL DEFAULT '' CHECK (length(name) <= 100),
    credential_id    bytea NOT NULL,
    public_key       bytea NOT NULL,
    attestation_type text NOT NULL DEFAULT '',
    transports       text[] NOT NULL DEFAULT '{}',
    sign_count       bigint NOT NULL DEFAULT 0,
    backup_eligible  boolean NOT NULL DEFAULT false,
    backup_state     boolean NOT NULL DEFAULT false,
    aaguid           bytea,
    created_at       timestamptz NOT NULL DEFAULT now(),
    last_used_at     timestamptz
);

-- The WebAuthn spec requires credential IDs to be globally unique;
-- the login assertion looks rows up by this key.
CREATE UNIQUE INDEX webauthn_credentials_credential_idx
    ON webauthn_credentials (credential_id);

CREATE INDEX webauthn_credentials_user_idx
    ON webauthn_credentials (user_id);
