-- 0174 up — CHECK sep41_transfers.amount (T090).
--
-- 0047 declared `amount numeric` with no constraint, unlike its sibling
-- sep41_supply_events (0015: NOT NULL CHECK (amount >= 0)). The per-row
-- writer (InsertSEP41TransferBatch) rejected a negative or missing
-- transfer/approve amount, but the ch-rebuild COPY path
-- (CopyMergeSEP41Transfers) wrote whatever it was handed, and the table
-- itself accepted it. SEP-41 amounts are non-negative: the kind and the
-- from/to columns carry direction, so a negative amount inverts every
-- net-position sum it reaches.
--
-- The constraint mirrors the Go row contract exactly: transfer and approve
-- rows carry a non-NULL amount; set_admin / set_authorized carry none
-- (NULL); any amount present is >= 0.
--
-- sep41_transfers is a compressed hypertable, and on timescaledb 2.26.4 a
-- CHECK over the numeric `amount` column cannot be added while compressed
-- chunks exist: the ALTER fails with a corrupted-plan error ("invalid attnum
-- 64", "unrecognized node type", "stack depth limit exceeded"), NOT VALID
-- included. So this uses the 0101/0142/0148 decompress-every-chunk step
-- first; the compression policy recompresses them on its next run, and the
-- integration test pins that recompression and compressed-chunk inserts
-- both honour the CHECK. No explicit BEGIN, matching 0148: golang-migrate
-- runs the file as one implicit transaction.
--
-- If apply fails with sep41_transfers_amount_check violated, a row written
-- by the old COPY path is negative or missing its amount. Re-derive that
-- window (`stellarindex-ops projector-replay -config PATH -source
-- sep41_transfers -from <ledger>`); do not delete rows to get past it.

SELECT decompress_chunk(c, true) FROM show_chunks('sep41_transfers') c;

ALTER TABLE sep41_transfers ADD CONSTRAINT sep41_transfers_amount_check  -- migration-compat:ok the previous binary's per-row writer already rejects every row this refuses, and its COPY path falls back per-row on a rejected batch
    CHECK (
        (amount IS NULL OR amount >= 0)
        AND (event_kind NOT IN ('transfer', 'approve') OR amount IS NOT NULL)
    );
