-- 0190 down — drop the SEP-1 identity change history. The recorded
-- transitions are lost; the current payloads in issuers are untouched.

BEGIN;

DROP TABLE IF EXISTS issuer_identity_history;

COMMIT;
