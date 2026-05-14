-- 0032 down — remove the Soroswap router seed row.

BEGIN;

DELETE FROM routers
 WHERE contract_id = 'CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH'
   AND auto_discovered = false;

COMMIT;
