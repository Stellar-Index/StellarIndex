-- 0149 up — `asset_volume_character` worker-maintained rollup.
--
-- Backs the `volume_character` label + account-structure signals on the
-- GET /v1/assets/{id} detail AND (new, #30 follow-up) the GET /v1/assets
-- listing (wash-and-scam-signals design §2). The live derivation is a
-- per-asset trailing-14d account-structure roll over the `trades`
-- hypertable (maker/taker only live in Timescale, not the ClickHouse
-- lake): distinct makers/takers, the top unordered-(maker,taker) pair
-- share, self-cross share, issuer-side share, market-styled share, then a
-- derived `market | operational | concentrated` character. Measured 4.09s
-- live on the USDC detail (per-request `volumeCharacterTimeout = 4s`
-- tripped, returning null) — the exact scan this rollup moves off the
-- request path.
--
-- It cannot be a TimescaleDB continuous aggregate: each trade contributes
-- to BOTH its base_asset and its quote_asset (as "the asset", with the
-- other side as counterpart), a UNION of two projections of the same
-- source that a CAGG's single GROUP BY can't express — the same
-- base-OR-quote shape migration 0087 hit. So an aggregator worker
-- (internal/aggregate/assetcharacterrollup) runs the all-asset single-pass
-- roll on a slow cadence (~15m — the roll is heavy) and upserts one row
-- per asset here. The detail then does a keyed-on-PK lookup and the
-- listing LEFT JOINs this small table.
--
-- Schema (one row per canonical asset over the trailing window):
--   asset_id                   — CANONICAL asset id (alias-folded via the
--                                process AliasRegistry, so a SAC twin and
--                                its classic agree — the same fold the
--                                per-asset AssetVolumeCharacter read does
--                                through assetAliasArray). e.g. 'USDC-G…',
--                                'native'.
--   window_days                — trailing window the signals roll over (14).
--   volume_usd                 — priced (usd_volume non-null) window volume
--                                the shares are computed against. NUMERIC
--                                (ADR-0003) — this is money-adjacent
--                                analytics; store it exact, render 2dp on
--                                the wire.
--   distinct_makers/takers     — distinct account counts over the window.
--   top_account_pair_vol_share — max unordered-(maker,taker) pair volume /
--                                total. The volume-painting / ping-pong /
--                                dust signature; also the §4-B directory
--                                demotion multiplier (1 − share).
--   self_cross_share           — volume where maker == taker.
--   issuer_side_share          — volume where the asset's OWN issuer is
--                                maker or taker.
--   market_styled_share        — volume whose counterpart is a real price
--                                surface (native / fiat:% / USDC-%).
--   is_market_styled           — market_styled_share ≥ 0.50.
--   character                  — derived enum: market | operational |
--                                concentrated (CHECK-constrained).
--   computed_at                — worker run timestamp; the worker prunes
--                                rows it did not re-write this pass (assets
--                                whose priced volume lapsed to nothing).
--
-- Not a hypertable: keyed on asset (bounded by the actively-traded asset
-- set), UPDATE'd in place. Reads omit the fields until the aggregator
-- worker's first pass populates it (LEFT JOIN → NULL → omitempty, same
-- posture as asset_volume_24h / change_summary_5m).

BEGIN;

CREATE TABLE asset_volume_character (
    asset_id                   text             PRIMARY KEY,
    window_days                integer          NOT NULL,
    volume_usd                 numeric          NOT NULL,
    distinct_makers            bigint           NOT NULL,
    distinct_takers            bigint           NOT NULL,
    top_account_pair_vol_share double precision NOT NULL,
    self_cross_share           double precision NOT NULL,
    issuer_side_share          double precision NOT NULL,
    market_styled_share        double precision NOT NULL,
    is_market_styled           boolean          NOT NULL,
    character                  text             NOT NULL
        CHECK (character IN ('market', 'operational', 'concentrated')),
    computed_at                timestamptz      NOT NULL DEFAULT now()
);

COMMENT ON TABLE asset_volume_character IS
    'Worker-maintained per-asset trailing-window account-structure rollup '
    '(volume_character + signals) backing /v1/assets{,/{id}} '
    '(wash-and-scam-signals design §2, migration 0149). Refreshed by '
    'internal/aggregate/assetcharacterrollup in the aggregator binary.';

COMMIT;
