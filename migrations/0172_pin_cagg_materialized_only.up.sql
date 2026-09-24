-- 0172: pin timescaledb.materialized_only = true on every served CAGG
-- except the two real-time volume counters (0069 source_volume_1h, 0076
-- pools_per_source_1h).
--
-- ADR-0015's closed-bucket invariant rests on this property. A refresh
-- materializes whole buckets only, so a materialized_only view exposes
-- CLOSED buckets and nothing else; most readers of these views (catalogue
-- snapshot, /v1/markets, the DEX pages, FX resolution, RWA daily history
-- via oracle_prices_1d) carry no `bucket <= now() - INTERVAL` guard of
-- their own and depend on it. Until now nothing asserted it: each view
-- got whatever the TimescaleDB default was at CREATE time (false before
-- 2.13), and 0115/0126/0147/0166 captured and restored the prior value, so a
-- view that was ever real-time stayed real-time and served the open
-- bucket beside the guarded /v1/price reading the previous one.
--
-- Idempotent: a no-op on a database where every view is already
-- materialized_only (r1 as measured for 0147/0156). Each ALTER rewrites
-- only the view's user-facing definition — no data is touched and
-- nothing needs re-materializing. A missing view fails the migration
-- rather than being skipped. Executed from a real-time starting state in
-- test/integration/cagg_materialized_only_test.go, which also fails on
-- any future CAGG left real-time outside the allowlist.

BEGIN;

ALTER MATERIALIZED VIEW prices_1m  SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW prices_15m SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW prices_1h  SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW prices_4h  SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW prices_1d  SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW prices_1w  SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW prices_1mo SET (timescaledb.materialized_only = true);

ALTER MATERIALIZED VIEW twap_1h SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW twap_1d SET (timescaledb.materialized_only = true);

ALTER MATERIALIZED VIEW oracle_prices_1m  SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW oracle_prices_15m SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW oracle_prices_1h  SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW oracle_prices_4h  SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW oracle_prices_1d  SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW oracle_prices_1w  SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW oracle_prices_1mo SET (timescaledb.materialized_only = true);

ALTER MATERIALIZED VIEW supply_1d             SET (timescaledb.materialized_only = true);
ALTER MATERIALIZED VIEW dex_volume_by_pair_1d SET (timescaledb.materialized_only = true);

COMMIT;
