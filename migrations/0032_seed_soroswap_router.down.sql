-- 0032 down — remove the Soroswap router seed row.

BEGIN;

DELETE FROM routers -- lint-down-delete:ok removes only the seed row this migration's up inserted
 WHERE contract_id = 'CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH'
   AND auto_discovered = false;

COMMIT;
