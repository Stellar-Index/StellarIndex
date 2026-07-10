-- 0106 up — address-scoped indexes on sep41_transfers (ADR-0048 D5).
--
-- GET /v1/accounts/{g}/movements' Postgres "recent tail"
-- (timescale.Store.ListSEP41TransfersByAddress) reads
-- `WHERE (from_addr = ? OR to_addr = ?)` ACROSS every contract_id —
-- the existing sep41_transfers_contract_{from,to}_idx indexes
-- (migration 0047) are (contract_id, from_addr/to_addr, ...) prefixed
-- and useless for an address-only predicate; sep41_transfers_ledger_idx
-- (migration 0083) covers `ledger` alone. Without a dedicated index,
-- an account-movements page for an active address would seq-scan the
-- whole hypertable — the exact "no unbounded trade-scan queries"
-- failure mode already learned the hard way once (see CLAUDE.md /
-- feedback_no_unbounded_trade_scan.md).
--
-- Two partial indexes (one per side), each (addr_col, ledger DESC) —
-- ListSEP41TransfersByAddress's ORDER BY ledger DESC, tx_hash DESC,
-- op_index DESC, event_index DESC needs only the `ledger` prefix to
-- narrow to an address's rows in ledger order; the remaining sort
-- columns are cheap in-memory ORDER BY over an already-small
-- per-address result set. WHERE <col> IS NOT NULL keeps the
-- claimable-balance / escrow-adjacent NULL sides (rare for 'transfer'
-- rows, but the column is nullable) out of the index.
--
-- IF NOT EXISTS + no CONCURRENTLY, matching migration 0083's own
-- convention: r1 gets CREATE INDEX CONCURRENTLY applied by hand ahead
-- of this migration (golang-migrate runs each file in a transaction,
-- and CONCURRENTLY cannot run inside one); this file's plain form is
-- what a fresh/dev deployment's migration run actually executes, and
-- is a safe no-op on r1 once the by-hand CONCURRENTLY index exists.

BEGIN;

CREATE INDEX IF NOT EXISTS sep41_transfers_from_addr_ledger_idx
    ON sep41_transfers (from_addr, ledger DESC)
    WHERE from_addr IS NOT NULL;

CREATE INDEX IF NOT EXISTS sep41_transfers_to_addr_ledger_idx
    ON sep41_transfers (to_addr, ledger DESC)
    WHERE to_addr IS NOT NULL;

COMMIT;
