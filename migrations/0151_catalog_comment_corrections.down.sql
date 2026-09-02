-- 0151 down — restore the twelve pre-0151 catalog comments VERBATIM.
--
-- Each string below is copied character-for-character out of the
-- migration that first issued it, so `migrate down` leaves
-- pg_description byte-identical to the pre-0151 state:
--
--   soroban_events.topics_xdr        0114:88-95
--   aquarius_protocol_fee.recipient  0129:80-83
--   aquarius_rewards_events          0099:121-128
--   trades.usd_volume                0001:54-55
--   oracle_updates.contract_id       0003:55-56
--   decoder_stats_5m                 0020:48-52
--   completeness_snapshots           0108:40-51
--   blend_auctions                   0009:100-102
--   blend_positions                  0045:151-155
--   soroswap_skim_events             0043:79-83
--   change_summary_5m                0022:82-86
--   customer_webhooks.secret_hash    had NO comment (0027:368 is an
--                                    inline `--` note in the DDL, which
--                                    is source text, not catalog text)
--                                    → restored to NULL.
--
-- Restoring these re-introduces eleven documented falsehoods (and drops
-- the secret_hash misnomer warning entirely). That is
-- what a down is for: it returns the database to the state the previous
-- release expects, it does not preserve the improvement.

BEGIN;

COMMENT ON COLUMN soroban_events.topics_xdr IS
    'Complete ordered list of every topic''s raw XDR bytes, in emit order '
    '(audit-2026-07-16 C2-11). Supersedes the four fixed topic_0..3 columns, '
    'which are retained + still populated (first four topics) for back-compat '
    'and the topic_0_sym index fast-path. Empty ''{}'' marks a legacy row '
    'written before this column existed — read topic_0..3 for those. Events '
    'with 5+ topics (e.g. Aquarius multi-token pool events) no longer '
    'truncate. Recover pre-0114 rows via a ClickHouse-lake re-project.';

COMMENT ON COLUMN aquarius_protocol_fee.recipient IS
    'claim_protocol_fee only — the fee-sweep destination. The claimed '
    'token is positional (one row per token); join a recent trade / '
    'deposit_liquidity for the pool to resolve it.';

COMMENT ON TABLE aquarius_rewards_events IS
    'Aquarius per-pool rewards-gauge / liquidity-mining event surface '
    '(11 kinds: pool_state, claim_reward, set_rewards_config, '
    'position_update, deposit, claim_fees, rewards_gauge_claim, claim, '
    'rewards_gauge_schedule_reward, set_rewards_state, rewards_gauge_add). '
    'Hypertable on ledger_close_time. Aquarius reward tokens have no '
    'published price at this layer; these rows never contribute to VWAP. '
    'See internal/sources/aquarius/README.md (ROADMAP #89).';

COMMENT ON COLUMN trades.usd_volume IS
    'Derived by the aggregator post-insert; null until that run completes.';

COMMENT ON COLUMN oracle_updates.contract_id IS
    'NULL for off-chain sources (coingecko, coinmarketcap, chainlink-http).';

COMMENT ON TABLE decoder_stats_5m IS
    '5-minute rollups of dispatcher.Stats() per source. Persists '
    'the in-memory counters so /v1/diagnostics/decoders can query '
    'history. Snapshot-and-clear semantics on the writer side '
    'guarantee no double-counting between buckets.';

COMMENT ON TABLE completeness_snapshots IS
    'Per-source completeness verdict (ADR-0033/ADR-0034). '
    'watermark_ledger/coverage_pct are the LAKE (archive) axis: the '
    'highest ledger where substrate+recognition (NOT projection) hold '
    'contiguously from genesis. lake_complete is that axis''s headline '
    '(watermark_ledger >= tip_ledger) — genesis-complete for the '
    'certified ClickHouse archive, decoupled from the served tier. '
    'complete is the SERVED/combined axis: lake_complete additionally '
    'gated by projection_ok, which is retention-scoped by design (ADR-0034: '
    'Postgres is the served tier, not the archive) — see '
    'notes/DECISION-genesis-complete-verdict-2026-07-16.md. Written by '
    'compute-completeness, read by the API.';

COMMENT ON TABLE blend_auctions IS
    'One row per Blend auction event (new / fill / delete). '
    'Hypertable partitioned on ts. See ADR-0006 + docs/discovery/dexes-amms/blend.md.';

COMMENT ON TABLE blend_positions IS
    'Per-event Blend money-market position changes (supply / withdraw '
    '/ supply_collateral / withdraw_collateral / borrow / repay / '
    'flash_loan). Hypertable on ledger_close_time. See #25 + '
    'docs/discovery/dexes-amms/blend.md.';

COMMENT ON TABLE soroswap_skim_events IS
    'Per-event Soroswap pair-contract skim events — caller-initiated '
    'claim of excess pool balance above reserves. Not a trade; never '
    'feeds VWAP. Hypertable on ledger_close_time. See task #28 + '
    'docs/discovery/dexes-amms/soroswap.md §SkimEvent.';

COMMENT ON TABLE change_summary_5m IS
    'O(1) lookup table for multi-window deltas + ATH/ATL + streak + '
    'acceleration per entity. Refreshed every 5 min. Powers every '
    'delta strip on the showcase (one endpoint = one query). See '
    'docs/architecture/showcase-site-data-inventory.md §6.1 + §9.6.';

COMMENT ON COLUMN customer_webhooks.secret_hash IS NULL;

COMMIT;
