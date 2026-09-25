-- 0180 up — one webhook per (account, url).
--
-- WHAT IS WRONG TODAY
--
-- customer_webhooks (0027) constrains name length and a non-empty events
-- array but not url, so one account can register the same destination up
-- to its whole tier quota and have every event delivered to it once per
-- row, each with its own 15-attempt retry ladder (GH #828).
--
-- WHAT THIS MIGRATION ADDS
--
--   customer_webhooks_account_url_key   UNIQUE (account_id, url).
--
-- The store maps a violation of this index to platform.ErrConflict and
-- the dashboard answers 409. The same url on two different accounts stays
-- legal: each account holds its own signing secret.
--
-- EXISTING DUPLICATES. Rows are never merged or deleted here: each
-- duplicate carries its own signing secret, which its receiver may be
-- verifying against, so choosing a survivor is the customer's call. If any
-- duplicate exists the migration refuses with a count and the query that
-- lists them; the operator resolves them and re-runs.
--
-- RULE 9. Additive: a new index only. A previous binary that re-registers
-- an existing url now gets a unique violation (a 500) instead of a second
-- row; every other statement it issues is unaffected.

BEGIN;

DO $$
DECLARE
  dupes bigint;
BEGIN
  SELECT count(*) INTO dupes FROM (
    SELECT 1 FROM customer_webhooks GROUP BY account_id, url HAVING count(*) > 1
  ) d;
  IF dupes > 0 THEN
    RAISE EXCEPTION '0180: % (account_id, url) group(s) in customer_webhooks hold more than one row; '
      'list them with SELECT account_id, url, array_agg(id ORDER BY created_at) FROM customer_webhooks '
      'GROUP BY account_id, url HAVING count(*) > 1, have each account keep one, then re-run', dupes;
  END IF;
END
$$;

CREATE UNIQUE INDEX customer_webhooks_account_url_key
    ON customer_webhooks (account_id, url);

COMMIT;
