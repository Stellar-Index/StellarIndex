-- 0101: soroswap_router_swaps — call_path / call_depth / call_kind (ROADMAP #11).
--
-- The dispatcher walks the full Soroban auth tree per op (task #48 Phase 1),
-- so router calls nested inside aggregator contracts ARE captured — but until
-- now every captured row looked identical regardless of WHERE in the tx's call
-- tree the router was invoked. These three additive columns record the tree
-- position:
--
--   call_path   text[]   — ordered contract C-strkey chain from the top-level
--                          invocation down to and including the router itself.
--                          [router] for a direct call; [aggregator, …, router]
--                          for a sub-invocation. Always ends in contract_id.
--   call_depth  smallint — cardinality(call_path) - 1. 0 = direct.
--   call_kind   text     — 'top_level' | 'sub_invocation' discriminator, cheap
--                          to filter/aggregate on ("what fraction of router
--                          flow is aggregator-routed" — the 8,729x-undercount
--                          census question, docs/architecture/
--                          contract-call-coverage-audit.md).
--
-- All three are NULLable: rows written before this migration deployed carry
-- NULL (tree position was observed but not recorded at insert time). They are
-- NOT part of the PK and NOT part of call_sig (RouterSwap.CallSig() hashes
-- economic identity only, so auth-tree duplicates of the same call still dedup
-- via ON CONFLICT — see migration 0056).
--
-- HISTORICAL FILL (operator, queued r1 heavy job): the columns backfill via a
-- focused lake re-derive — the ON CONFLICT DO NOTHING write path cannot update
-- existing NULL rows in place, so the operator order is DELETE + re-derive
-- (same pattern as every ROADMAP §0 correction):
--
--   set -a; . /etc/default/stellarindex; set +a
--   run-heavy-job.sh router-callpath-del \
--     psql "$DSN" -c "DELETE FROM soroswap_router_swaps"
--   run-heavy-job.sh router-callpath-rederive \
--     stellarindex-ops ch-rebuild -config /etc/stellarindex.toml \
--       -sources soroswap-router -contract-calls -write \
--       -from 50746272 -to <tip>
--
-- (50,746,272 = the router's first-deploy ledger per the WASM audit; the
-- contract-call pass streams windowed internally, and a soroswap-router-only
-- invocation skips the buffered event pass, so the full 12M-ledger range is a
-- single invocation. BackfillSafe=true since 2026-05-19: single WASM hash over
-- the contract's entire life — docs/operations/wasm-audits/soroswap-router.md.)
--
-- Scope note (EVERY-event principle): this table deliberately covers ONLY the
-- router's two token-moving swap entry points (function_name CHECK, 0049). The
-- router's liquidity entry points (add_liquidity / remove_liquidity) have a
-- different arg shape (token pair + desired/min amounts, no path) that this
-- table's columns cannot represent, and its admin/read-only surface moves no
-- tokens. Both remain deliberately out of scope here — documented in
-- internal/sources/soroswap_router/README.md; a liquidity surface would be its
-- own table + decoder arm.

SELECT decompress_chunk(c, true) FROM show_chunks('soroswap_router_swaps') c;

ALTER TABLE soroswap_router_swaps ADD COLUMN IF NOT EXISTS call_path  text[];
ALTER TABLE soroswap_router_swaps ADD COLUMN IF NOT EXISTS call_depth smallint;
ALTER TABLE soroswap_router_swaps ADD COLUMN IF NOT EXISTS call_kind  text;

-- Internal consistency: kind matches depth; the chain ends in the invoked
-- contract; depth is the chain length minus one. NULL rows (pre-#11) pass.
ALTER TABLE soroswap_router_swaps ADD CONSTRAINT soroswap_router_swaps_call_kind_check
    CHECK (
        (call_kind IS NULL AND call_depth IS NULL AND call_path IS NULL)
        OR (call_kind = 'top_level'      AND call_depth = 0 AND cardinality(call_path) = 1
            AND call_path[1] = contract_id)
        OR (call_kind = 'sub_invocation' AND call_depth > 0 AND cardinality(call_path) = call_depth + 1
            AND call_path[call_depth + 1] = contract_id)
    );

-- Coverage-dashboard access path: "sub_invocation share over time".
CREATE INDEX soroswap_router_swaps_call_kind_ts_idx
    ON soroswap_router_swaps (call_kind, ledger_close_time DESC);
