-- 0229 up — keep the payload a home_domain change clears, so the new
-- domain's first fetch is diffed into issuer_identity_history.
--
-- Every writer of issuers.home_domain NULLs sep1_payload when the domain
-- changes (the payload records no source domain). SetIssuerSep1Payload then
-- saw no held payload, treated the new domain's fetch as a first fetch and
-- wrote no history row, so a rebrand or a hijacked home_domain rewrote the
-- issuer's identity with nothing recorded: the gap 0190 exists to close.
--
-- The reset now moves the cleared payload here (keeping an earlier one
-- across further moves); SetIssuerSep1Payload diffs against it when
-- sep1_payload is NULL and clears it in the same UPDATE. It is never
-- served: the old domain's claim stays unbound from the row.
--
-- Nullable column, no default: a catalog-only change with no table
-- rewrite, and the previous binary neither reads nor writes it (rule 9).

BEGIN;

SET LOCAL lock_timeout = '5s';

ALTER TABLE issuers ADD COLUMN sep1_payload_previous jsonb;

COMMENT ON COLUMN issuers.sep1_payload_previous IS
    'The sep1_payload a home_domain change cleared, held only until the new '
    'domain''s first successful fetch diffs against it into '
    'issuer_identity_history. Never served.';

COMMIT;
