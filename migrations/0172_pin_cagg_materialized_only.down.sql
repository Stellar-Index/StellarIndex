-- 0172 down — intentionally a no-op. The up migration asserts a property
-- every served environment already had (materialized_only on the price /
-- TWAP / oracle / supply / DEX-volume CAGGs); the prior value is not
-- recorded, and "restoring" real-time aggregation would serve the open
-- bucket, which is the defect 0172 closes.
SELECT 1;
