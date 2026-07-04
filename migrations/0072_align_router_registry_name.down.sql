-- 0072 down — restore the pre-alignment router name.
--
-- Note: any `trades.routed_via` values written while 0072 was live
-- carry 'soroswap-router' and are NOT rewritten here — the tag is
-- attribution metadata, and the up-migration can simply be re-applied.

BEGIN;

UPDATE routers
   SET name = 'soroswap-router-v1'
 WHERE contract_id = 'CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH'
   AND name = 'soroswap-router';

COMMIT;
