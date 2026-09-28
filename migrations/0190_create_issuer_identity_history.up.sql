-- 0190 up — record every change to an established issuer's SEP-1 identity
-- (GH #840).
--
-- SetIssuerSep1Payload overwrote sep1_payload blind: a compromised or
-- re-registered home_domain could rewrite an issuer's OrgName, ORG_URL,
-- ORG_LOGO and every [[CURRENCIES]].image within one refresh, and the
-- previous value was unrecoverable. The writer now diffs the held payload
-- against the new one inside the same transaction and appends one row per
-- changed field here. A first fetch (no held payload) records nothing.
--
-- No FK to issuers: the history must outlive the row it describes. Rows are
-- written only on an identity change of public stellar.toml data, so the
-- table grows with anchor rebrands, not with refresh volume, and holds no
-- personal data; no reaper.
--
-- New table only: the previous binary neither reads nor writes it (rule 9).

BEGIN;

CREATE TABLE issuer_identity_history (
    id           bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    g_strkey     text        NOT NULL,
    -- The home_domain the NEW payload was fetched from.
    home_domain  text        NOT NULL,
    field        text        NOT NULL
        CHECK (field IN ('org_name', 'org_url', 'org_logo', 'currency_image')),
    -- For currency_image only: the entry's key, `<Code>-<Issuer>` as the
    -- stellar.toml declared it.
    currency     text,
    -- NULL = the field was absent on that side of the change.
    old_value    text,
    new_value    text,
    changed_at   timestamptz NOT NULL DEFAULT now(),
    CHECK ((field = 'currency_image') = (currency IS NOT NULL)),
    CHECK (old_value IS DISTINCT FROM new_value)
);

CREATE INDEX issuer_identity_history_issuer_idx
    ON issuer_identity_history (g_strkey, changed_at DESC);

COMMENT ON TABLE issuer_identity_history IS
    'One row per change to an established issuer''s SEP-1 identity field '
    '(OrgName, ORG_URL, ORG_LOGO, a currency Image), written by '
    'SetIssuerSep1Payload in the same transaction as the payload overwrite.';

COMMIT;
