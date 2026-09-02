-- 0151 up — correct twelve stored catalog comments that a `\d+` reader
-- is currently misled by (#357 F4/F5/F6, #358 0001/0003/item-7/0108 +
-- the dead-pointer sweep, #346 F9).
--
-- Why a MIGRATION and not a header edit. `COMMENT ON` strings do not
-- live in the repo; they live in the DATABASE catalog
-- (pg_description). Editing the original migration's text changes what
-- a FRESH database would get and changes NOTHING about r1 — the string
-- an operator actually reads through `\d+` / `col_description()` stays
-- wrong forever. Re-issuing the comment is the only correction that
-- reaches both. (The mirror rule — a shipped UP body is frozen, its
-- header comments and its DOWN body are correctable — is written up in
-- migrations/README.md, "Amending a shipped migration".)
--
-- Zero data risk: `COMMENT ON` takes a SHARE UPDATE EXCLUSIVE-free
-- catalog write, touches no heap, no index, no chunk, and is invisible
-- to every query plan. Old-binary-safe by construction (rule 9) —
-- nothing in Go reads a catalog comment.
--
-- What each one said and why it was wrong:
--
--   1. soroban_events.topics_xdr (0114:88-95) ended "Recover pre-0114
--      rows via a ClickHouse-lake re-project." No such recovery exists:
--      projector-replay READS this table (or, since ADR-0034, the lake)
--      and WRITES the per-source tables — nothing writes topics_xdr
--      back here. The complete topic list for those ledgers is in the
--      lake and must be read there.
--
--   2. aquarius_protocol_fee.recipient (0129:80-83) told the reader the
--      claimed token is positional and to "join a recent trade /
--      deposit_liquidity for the pool to resolve it". Migration 0139
--      added the `token` column, sourced from topic[1]
--      (internal/sources/aquarius/decode.go:545-552), and re-commented
--      `.token` only — `.recipient` kept pointing at the join, which
--      mis-attributes a two-token claim.
--
--   3. aquarius_rewards_events (0099:121-128) says "11 kinds" and lists
--      11. The event_kind CHECK at 0099:92-98 admits TWELVE; the list
--      omits `config_rewards`.
--
--   4. trades.usd_volume (0001:54-55) says "Derived by the aggregator
--      post-insert; null until that run completes." No aggregator pass
--      has ever valued this column: InsertTrade's tiered USD router
--      stamps it AT INSERT (internal/storage/timescale/trades.go), and
--      NULL means "no route", not "not yet run" — a consumer that waits
--      for the value to appear waits forever.
--
--   5. oracle_updates.contract_id (0003:55-56) names `coinmarketcap`
--      and `chainlink-http`. Measured on r1 2026-09-02, the sources
--      that actually write a NULL contract_id are chainlink, coingecko
--      and ecb; the CMC connector writes no oracle_updates row, and
--      `chainlink-http` is the retired name the code deliberately does
--      not use (internal/sources/external/chainlink/events.go:40-46).
--
--   6. decoder_stats_5m (0020:48-52) says the rows exist "so
--      /v1/diagnostics/decoders can query history". That route is not
--      in openapi/stellar-index.v1.yaml and no Go code selects from the
--      table — it is write-only today, and the writer is the INDEXER's
--      statsflush worker, not the aggregator (0020:15).
--
--   7. completeness_snapshots (0108:40-51) cites
--      notes/DECISION-genesis-complete-verdict-2026-07-16.md. `notes/`
--      is gitignored (.gitignore:148), so that path resolves to nothing
--      for anyone who did not write it. The same two-axis rationale is
--      public on the /v1/coverage operation in the OpenAPI spec and in
--      ADR-0033 / ADR-0034.
--
--   8. Four stored comments point at documentation that no longer
--      exists (#358 dead-pointer sweep). `blend_auctions` (0009:102)
--      and `blend_positions` (0045:155) cite
--      docs/discovery/dexes-amms/blend.md; `soroswap_skim_events`
--      (0043:83) cites the soroswap.md beside it — the whole
--      docs/discovery/ tree was removed and has no successor there. Each
--      is re-pointed at the live per-source README + protocol page.
--      `change_summary_5m` (0022:86) cites
--      showcase-site-data-inventory.md, renamed to
--      explorer-data-inventory.md in 6f21cc8f; §6.1 and §9.6 both still
--      exist there, so only the filename changes.
--
--   9. customer_webhooks.secret_hash has NO stored comment at all, and
--      the name is a lie: the column holds the RAW HMAC-SHA-256 signing
--      key, not a hash of one (the receiver needs the shared secret to
--      verify a delivery). Recorded here because `\d+` is where an
--      operator forms the belief "this is already hashed" (#346 F9).
--      This does not fix the at-rest exposure — it stops the misnomer
--      from being load-bearing until a rename/seal lands.
--
-- Note on wording: none of the corrected strings REPEATS the stale name
-- it replaces. A catalog comment is grepped (`\d+`, col_description) and
-- a negating mention — "no coinmarketcap ingest writes here" — is still a
-- hit for `coinmarketcap`, and still the wrong word in front of a reader
-- skimming. The what-it-used-to-say belongs in this header, above; the
-- stored string says only what is true.
--
-- NOT included: wasm_versions (0017:48, whose comment cites the deleted
-- showcase-site-data-inventory.md). Migration 0152 DROPS that table, so
-- correcting its comment here would be dead text one migration later.

BEGIN;

COMMENT ON COLUMN soroban_events.topics_xdr IS
    'Complete ordered list of every topic''s raw XDR bytes, in emit order '
    '(audit-2026-07-16 C2-11). Supersedes the four fixed topic_0..3 columns, '
    'which are retained + still populated (first four topics) for back-compat '
    'and the topic_0_sym index fast-path. Empty ''{}'' marks a legacy row '
    'written before this column existed — read topic_0..3 for those. Events '
    'with 5+ topics (e.g. Aquarius multi-token pool events) no longer '
    'truncate. Legacy ''{}'' rows are NOT repaired by projector-replay: the '
    'projector reads this table (or, since ADR-0034, the ClickHouse lake) and '
    'writes the per-source tables — nothing writes topics_xdr back here. The '
    'complete topic list for those ledgers lives in the lake, '
    'stellar.contract_events.topics_xdr.';

COMMENT ON COLUMN aquarius_protocol_fee.recipient IS
    'claim_protocol_fee only — the fee-sweep destination. The claimed token is '
    'the `token` column (topic[1], migration 0139), NOT something to resolve by '
    'joining a trade or deposit_liquidity: an op that sweeps two tokens emits '
    'one event per token to the same recipient, and the join picks the wrong '
    'one. `token` is NULL only for rows ingested before the 2026-08-10 decoder '
    'fix — re-derive those with projector-replay.';

COMMENT ON TABLE aquarius_rewards_events IS
    'Aquarius per-pool rewards-gauge / liquidity-mining event surface '
    '(12 kinds, matching the event_kind CHECK: pool_state, claim_reward, '
    'set_rewards_config, position_update, deposit, claim_fees, '
    'rewards_gauge_claim, claim, rewards_gauge_schedule_reward, '
    'set_rewards_state, rewards_gauge_add, config_rewards). '
    'Hypertable on ledger_close_time. Aquarius reward tokens have no '
    'published price at this layer; these rows never contribute to VWAP. '
    'See internal/sources/aquarius/README.md (ROADMAP #89).';

COMMENT ON COLUMN trades.usd_volume IS
    'Valued AT INSERT by InsertTrade''s tiered USD router (exact peg / FX rate '
    '/ XLM anchor at trade time — internal/storage/timescale/trades.go). NULL '
    'means the router found no route for this trade, NOT "the aggregator has '
    'not run yet": no aggregator pass ever fills this column, so a NULL never '
    'becomes a value on its own. Corrective re-derives are operator-invoked and '
    'INV-3-guarded by derive_generation (`stellarindex-ops usd-volume-restamp` '
    'for the exact tiers, ch-rebuild for the estimated ones).';

COMMENT ON COLUMN oracle_updates.contract_id IS
    'NULL for off-chain sources. Measured on r1 2026-09-02 the sources that '
    'write a NULL here are exactly chainlink, coingecko and ecb; see '
    'internal/sources/external/ for the canonical source names. Non-NULL for '
    'every on-chain oracle (reflector-cex/-dex/-fx, redstone, band).';

COMMENT ON TABLE decoder_stats_5m IS
    '5-minute rollups of dispatcher.Stats() per source, written by the '
    'INDEXER''s statsflush worker (internal/dispatcher/statsflush) — not by the '
    'aggregator. Snapshot-and-clear semantics on the writer side guarantee no '
    'double-counting between buckets. WRITE-ONLY today: nothing in Go selects '
    'from this table and there is no /v1/diagnostics/decoders route in the '
    'OpenAPI spec. Query it directly for decoder history.';

COMMENT ON TABLE completeness_snapshots IS
    'Per-source completeness verdict (ADR-0033/ADR-0034). '
    'watermark_ledger/coverage_pct are the LAKE (archive) axis: the '
    'highest ledger where substrate+recognition (NOT projection) hold '
    'contiguously from genesis. lake_complete is that axis''s headline '
    '(watermark_ledger >= tip_ledger) — genesis-complete for the '
    'certified ClickHouse archive, decoupled from the served tier. '
    'complete is the SERVED/combined axis: lake_complete additionally '
    'gated by projection_ok, which is retention-scoped by design (ADR-0034: '
    'Postgres is the served tier, not the archive) — the two-axis rationale is '
    'public on the /v1/coverage operation in openapi/stellar-index.v1.yaml and '
    'in ADR-0033/ADR-0034. Written by compute-completeness, read by the API.';

COMMENT ON TABLE blend_auctions IS
    'One row per Blend auction event (new / fill / delete). '
    'Hypertable partitioned on ts. See ADR-0006, '
    'internal/sources/blend/README.md and docs/protocols/blend.md.';

COMMENT ON TABLE blend_positions IS
    'Per-event Blend money-market position changes (supply / withdraw '
    '/ supply_collateral / withdraw_collateral / borrow / repay / '
    'flash_loan). Hypertable on ledger_close_time. See #25, '
    'internal/sources/blend/README.md and docs/protocols/blend.md.';

COMMENT ON TABLE soroswap_skim_events IS
    'Per-event Soroswap pair-contract skim events — caller-initiated '
    'claim of excess pool balance above reserves. Not a trade; never '
    'feeds VWAP. Hypertable on ledger_close_time. See task #28, '
    'internal/sources/soroswap/README.md and docs/protocols/soroswap.md.';

COMMENT ON TABLE change_summary_5m IS
    'O(1) lookup table for multi-window deltas + ATH/ATL + streak + '
    'acceleration per entity. Refreshed every 5 min. Powers every '
    'delta strip on the explorer (one endpoint = one query). See '
    'docs/architecture/explorer-data-inventory.md §6.1 + §9.6.';

COMMENT ON COLUMN customer_webhooks.secret_hash IS
    'MISNAMED. Stores the RAW HMAC-SHA-256 signing key, not a hash of one — '
    'the receiver needs the shared secret to verify a delivery signature, so '
    'it cannot be one-way hashed (internal/platform/webhook.go). Treat this '
    'column as a credential at rest: it is NOT sealed the way '
    'accounts.mfa_secret_enc is. Renaming it to signing_key needs the rule-9 '
    'two-release dance (migrate up runs BEFORE the binary swap), so the name '
    'stays and this comment carries the truth (#346 F9).';

COMMIT;
