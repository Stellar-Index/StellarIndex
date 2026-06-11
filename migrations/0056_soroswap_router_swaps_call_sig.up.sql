-- 0056: soroswap_router_swaps — add call_sig to the PK (per-call discriminator).
--
-- The PK was (ledger_close_time, ledger, tx_hash, op_index) — no per-call
-- discriminator. A single InvokeContract op can carry MULTIPLE distinct router
-- swaps (an aggregator splitting a trade, or a batch distributing to several
-- recipients): they share (ledger, tx_hash, op_index) and the coarse PK
-- collapsed them to one row via ON CONFLICT DO NOTHING. The completeness
-- census's honesty guard confirmed this is REAL loss, not dup-noise: 106
-- genuinely-distinct swaps across pubnet history (different recipient / path /
-- amounts in op_index 0 of their tx).
--
-- call_sig (RouterSwap.CallSig(): a 128-bit content hash of
-- function|recipient|path|amount_in|amount_out) is the discriminator — same
-- anti-collision fix as blend (0053/0054) / defindex (0055), but content-derived
-- rather than event_index because the router is EVENT-LESS (no soroban_events
-- index to key on). Distinct swaps get distinct call_sig (all stored); auth-tree
-- duplicates of the SAME call (multi-entry co-signed txs surface it at several
-- CallPaths) hash identically and still dedup.
--
-- Existing rows get call_sig='' (column default). On a populated host they are
-- one-per-(tx,op) so the 5-col ADD-PK succeeds directly, but a re-derive would
-- then insert call_sig=<hash> rows ALONGSIDE the '' rows (duplicates). So the
-- operator order is: decompress -> ADD COLUMN -> DROP old PK -> ADD new PK ->
-- TRUNCATE -> re-derive (ch-rebuild -contract-calls -sources soroswap-router
-- -write, which re-derives the full set from the certified lake with correct
-- call_sig). For a fresh database the table is empty and the re-derive populates
-- it directly. STOP the indexer before this migration and deploy the call_sig
-- sink before restarting — the old sink's ON CONFLICT (4-col) target no longer
-- matches the 5-col PK and would error every router insert.

SELECT decompress_chunk(c, true) FROM show_chunks('soroswap_router_swaps') c;

ALTER TABLE soroswap_router_swaps ADD COLUMN IF NOT EXISTS call_sig text NOT NULL DEFAULT '';
ALTER TABLE soroswap_router_swaps DROP CONSTRAINT soroswap_router_swaps_pkey;
ALTER TABLE soroswap_router_swaps ADD CONSTRAINT soroswap_router_swaps_pkey
    PRIMARY KEY (ledger_close_time, ledger, tx_hash, op_index, call_sig);
