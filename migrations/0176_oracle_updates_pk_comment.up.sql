-- 0176 up — correct the stored comment on oracle_updates (T094).
--
-- 0003's table comment claims "one row per (source, ledger, tx_hash,
-- op_index)". The actual PRIMARY KEY (0003:48) is
-- (source, ledger, tx_hash, op_index, ts) — `ts` is included because
-- TimescaleDB requires every unique constraint on a hypertable to carry
-- the partitioning column (chunks are separate tables; a per-chunk
-- unique index cannot see across them without it). `ts` is the ORACLE
-- PUBLICATION timestamp, decoder-derived — not the ledger close time —
-- and is explicitly excluded from canonical.OracleUpdate.ID().
--
-- The gap the old comment hid: if a decoder's `ts` derivation is later
-- corrected (as happened for Band's resolution timestamp on 2026-08-03)
-- and the affected ledgers are replayed, the corrected row's PK differs
-- from the original's (only `ts` changed), so INSERT ... ON CONFLICT on
-- this PK does not match the stale row — the replay ADDS a second row
-- for the same on-chain publication instead of replacing the first. A
-- reader who believed the old comment's "one row per (source, ledger,
-- tx_hash, op_index)" claim would double-count that publication.
--
-- No DB-level fix is possible without dropping `ts` from the hypertable
-- partition column entirely (a much larger, unrelated redesign), so this
-- corrects the claim: a decoder-`ts` replay must DELETE the stale row by
-- (source, ledger, tx_hash, op_index) BEFORE re-inserting the corrected
-- one, not rely on ON CONFLICT to merge them.
--
-- Why a migration, not a header edit: `COMMENT ON` text lives in
-- pg_description, not the repo. 0003's up body is immutable
-- (migrations/README.md, "Amending a shipped migration"; 0151/0173 are
-- the precedent). Catalog-only — no heap, index or chunk touched, and
-- nothing in Go reads a catalog comment, so old-binary-safe (rule 9).

COMMENT ON TABLE oracle_updates IS
    'Every observed oracle publication. The PRIMARY KEY is '
    '(source, ledger, tx_hash, op_index, ts) — `ts` is included solely '
    'because TimescaleDB requires the partitioning column in any unique '
    'constraint on a hypertable, NOT because it is part of the logical '
    'identity: canonical.OracleUpdate.ID() is source:ledger:tx_hash:op_index '
    'and deliberately excludes ts. A decoder fix to ts derivation (e.g. '
    'the 2026-08-03 Band resolution-timestamp fix) therefore makes a '
    'replayed row a NEW PRIMARY KEY, not a conflict with the stale one — '
    'ON CONFLICT on this key does not merge them. Replaying after a '
    'ts-derivation fix must DELETE the stale row by '
    '(source, ledger, tx_hash, op_index) first. Hypertable partitioned on '
    'ts. See ADR-0006.';
