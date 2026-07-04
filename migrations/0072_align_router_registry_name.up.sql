-- 0072 up — align `routers.name` for the Soroswap router with the
-- source name used by trade attribution.
--
-- Phase B of router attribution (BACKLOG #29) tags same-tx `trades`
-- rows with `routed_via = soroswap_router.SourceName`
-- ("soroswap-router") — the stable registry key declared in
-- internal/sources/soroswap_router/events.go, which states outright
-- that SourceName is used in "`external.Registry`, `routers.name`,
-- and trade attribution". Migration 0032 pre-dated the tagger and
-- seeded the row as "soroswap-router-v1", contradicting that
-- contract; had we left it, the /v1/aggregators rollup join
-- (`trades.routed_via = routers.name`) would silently match nothing.
--
-- Policy going forward: `routed_via` carries the STABLE source name,
-- not a per-version label. If Soroswap ships a router v2 at a new
-- contract address, seed a second `routers` row with the same name —
-- the per-version split is recoverable from
-- `soroswap_router_swaps.contract_id`, and consumers of routed_via
-- keep one stable key across upgrades.
--
-- The DeFindex vault rows (migration 0033) keep their per-vault
-- names: they are kind='aggregator-vault', hold persistent capital,
-- and do not participate in per-tx routed_via tagging (their state
-- lives in defindex_flows / aggregator_exposures, not trades).

BEGIN;

UPDATE routers
   SET name  = 'soroswap-router',
       notes = COALESCE(notes, '') ||
               ' Renamed from soroswap-router-v1 by migration 0072 ' ||
               '(routed_via alignment).'
 WHERE contract_id = 'CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH'
   AND name = 'soroswap-router-v1';

COMMIT;
