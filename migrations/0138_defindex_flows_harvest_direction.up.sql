-- 0138: admit direction = 'harvest' into defindex_flows.
--
-- Sources-decode audit 2026-08-04, finding 4 (MEDIUM-HIGH): the
-- BlendStrategy `harvest` event was recognised-and-dropped on the
-- premise its body "has never been observed on-chain" — the lake
-- disproves it (1,018 harvests; body `{amount: i128, from: Address,
-- price_per_share: i128}` — the exact {from, amount} shape decodeFlow
-- already reads, plus one unread field). Vault NAV reconstructed from
-- deposit+withdraw under-counts by the full harvested yield.
--
-- This widening is old-binary compatible (CS-099 discipline): binaries
-- predating the decoder change simply never write 'harvest'.
--
-- Position/series readers are unaffected by construction: the
-- df_tokens position CASE (positions.go) has no ELSE arm, so harvest
-- rows fall out of user-position sums (harvest is strategy yield, not
-- a user flow), and the bespoke-lending series filter directions
-- explicitly.
ALTER TABLE defindex_flows
    DROP CONSTRAINT defindex_flows_direction_check;  -- migration-compat:ok replaced in the same transaction by a strict SUPERSET check below
ALTER TABLE defindex_flows
    ADD CONSTRAINT defindex_flows_direction_check    -- migration-compat:ok widening only: every value the previous released binary writes ('deposit'/'withdraw') satisfies the new check
    CHECK (direction IN ('deposit', 'withdraw', 'harvest'));
