-- 0104 down — remove the aggregator-exec seed row.

BEGIN;

DELETE FROM routers -- lint-down-delete:ok removes only the seed row this migration's up inserted
 WHERE contract_id = 'CD45PQFHSIUMIC4MVZXCQ2RD6REKXJMEHWRN56TWT3C4DV2U4DHVJRZH'
   AND auto_discovered = true;

COMMIT;
