-- Reverse of 0080. Drops the price_alerts table (its indexes cascade).
--
-- Down-migrations are a local/dev iteration lever only — NOT a production
-- rollback lever (migrations/README rule 9). The deploy pipeline never
-- auto-runs `migrate down`.

DROP TABLE IF EXISTS price_alerts;
