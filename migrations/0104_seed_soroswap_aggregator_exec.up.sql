-- 0104 up — seed the ONE evidence-verified aggregator wrapper contract
-- (ROADMAP #29 attribution follow-up / ROADMAP #11).
--
-- migration 0101 added call_path/call_depth/call_kind to
-- soroswap_router_swaps so a router invocation now records WHICH
-- contract (if any) wrapped it. Real captured mainnet bytes (ledger
-- 62,029,020, tx da2ffe5a8651e2289408631a180cc8f6fe26247c6558bfc80dbf6
-- d9527849cc7 — internal/sources/soroswap_router/real_bytes_test.go,
-- testdata/router_subinvocation_op_ledger62029020.b64, captured from
-- the certified ClickHouse lake on r1, 2026-07-10) show the router
-- wrapped two levels deep: an AGGREGATOR contract calls `exec()`,
-- which calls an adapter's `swap_exact_tokens_for_tokens()`, which
-- calls the router.
--
-- This seeds ONLY the outermost (aggregator) contract of that chain
-- — the identity `internal/storage/timescale.TagTradesRoutedVia`
-- looks up (call_path[1], i.e. Go CallPath[0], the outermost invoked
-- contract) to attribute a sub-invocation router trade to its
-- wrapper by name instead of lumping it into the bare
-- 'soroswap-router' bucket. The middle adapter
-- (CAYP3UWLJM7ZPTUKL6R6BFGTRWLZ46LRKOXTERI2K6BIJAWGYY62TXTO) is NOT
-- seeded — it is an implementation-detail hop, not the product the
-- end user (or an operator reading /v1/aggregators) would recognise
-- as "the aggregator".
--
-- Fail-closed honesty: unlike the 0032 soroswap-router seed (cross-
-- checked against soroswap-core/public/mainnet.contracts.json, an
-- upstream source of truth), this contract's PROTOCOL IDENTITY is
-- NOT independently confirmed — no vendor contracts list, WASM audit,
-- or protocol doc names it. The only evidence is one observed
-- mainnet transaction. `auto_discovered = true` marks it a flagged-
-- but-unverified candidate (the column's documented purpose, see
-- migration 0025) and `protocol_slug = 'unattributed'` avoids
-- claiming a protocol affiliation we cannot back up. Do not "upgrade"
-- this row to auto_discovered = false without a real audit trail
-- (vendor contracts list, WASM export/hash match, or a second
-- independently-sourced transaction).
--
-- Idempotent: ON CONFLICT DO NOTHING, matching 0032/0033/0072.

BEGIN;

INSERT INTO routers (contract_id, name, kind, protocol_slug, auto_discovered, notes)
VALUES (
    'CD45PQFHSIUMIC4MVZXCQ2RD6REKXJMEHWRN56TWT3C4DV2U4DHVJRZH',
    'soroswap-router-aggregator-exec',
    'router',
    'unattributed',
    true,
    'Evidence-observed, NOT vendor-verified. Source: one real mainnet ' ||
    'transaction (ledger 62,029,020, tx da2ffe5a8651e2289408631a180cc8f6' ||
    'fe26247c6558bfc80dbf6d9527849cc7) captured from the certified lake ' ||
    '2026-07-10 — internal/sources/soroswap_router/real_bytes_test.go, ' ||
    'testdata/router_subinvocation_op_ledger62029020.b64. Calls its own ' ||
    '`exec()` entry point, which wraps an adapter ' ||
    '(CAYP3UWLJM7ZPTUKL6R6BFGTRWLZ46LRKOXTERI2K6BIJAWGYY62TXTO, not ' ||
    'seeded) which wraps the soroswap-router. No WASM audit, no vendor ' ||
    'contracts list, and no second independent transaction confirm ' ||
    'this contract''s protocol identity — treat routed-via attribution ' ||
    'to this row as provisional (docs/architecture/contract-call-' ||
    'coverage-audit.md, ROADMAP #11).'
)
ON CONFLICT (contract_id) DO NOTHING;

COMMIT;
