-- 0104 down — remove the aggregator-exec seed row.

BEGIN;

DELETE FROM routers
 WHERE contract_id = 'CD45PQFHSIUMIC4MVZXCQ2RD6REKXJMEHWRN56TWT3C4DV2U4DHVJRZH'
   AND auto_discovered = true;

COMMIT;
