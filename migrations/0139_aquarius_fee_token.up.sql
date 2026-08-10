-- 0139: add the claimed token to aquarius_protocol_fee.
--
-- Corrects migration 0129's premise. 0129 asserted "the token identity
-- is positional/not in the body — join a recent trade for the pool to
-- resolve it" and shipped no token column. The lake refutes both
-- halves (sources-decode audit 2026-08-04, finding 5): every sampled
-- claim_protocol_fee event carries an ScvAddress at topic[1] — the
-- claimed token — and ledger 63,698,651 has one tx claiming TWO
-- different tokens from one pool with near-identical amounts, so the
-- join-a-trade workaround mis-attributes and any per-pool SUM(amount)
-- without the token adds integers of different token scales.
--
-- Additive nullable column: rule-9 clean. Rows written before the
-- decoder fix carry NULL; `stellarindex-ops projector-replay -source
-- aquarius` re-derives them with the token populated (the table's
-- upsert key is unchanged).
ALTER TABLE aquarius_protocol_fee
    ADD COLUMN token text;

COMMENT ON COLUMN aquarius_protocol_fee.token IS
    'Claimed token contract (C-strkey) from topic[1]; NULL only for rows ingested before the 2026-08-10 decoder fix (re-derive via projector-replay). Always NULL for set_protocol_fee rows.';
