-- 0032 up — pre-seed `routers` with the Soroswap router contract.
--
-- The router-attribution observer (Phase B follow-up) reads this
-- table at startup to populate the dispatcher's
-- ContractCallDecoder match-set. Manual seed (auto_discovered =
-- false) — operator-vetted contract id from
-- soroswap-core/public/mainnet.contracts.json (cross-checked
-- 2026-04-23 in docs/operations/wasm-audits/soroswap-router.md).
--
-- Idempotent: ON CONFLICT DO NOTHING so re-running the migration
-- against an already-seeded table is a no-op.

BEGIN;

INSERT INTO routers (contract_id, name, kind, protocol_slug, auto_discovered, notes)
VALUES (
    'CAG5LRYQ5JVEUI5TEID72EYOVX44TTUJT5BQR2J6J77FH65PCCFAJDDH',
    'soroswap-router-v1',
    'router',
    'soroswap',
    false,
    'Manual seed. Source: soroswap-core/public/mainnet.contracts.json. ' ||
    'Decoder: internal/sources/soroswap_router/. WASM audit: ' ||
    'docs/operations/wasm-audits/soroswap-router.md (in_progress).'
)
ON CONFLICT (contract_id) DO NOTHING;

COMMIT;
