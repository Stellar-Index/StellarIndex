# Stellar Coverage Matrix

This matrix enumerates **Stellar-specific** surfaces where we
must be **deeper** than CoinGecko + CoinMarketCap. The CG/CMC
parity matrix tracks the floor; this tracks the moat.

Closure rule: every row must be filled before the audit closes.

Scoring:

- `covered` — we ship it, with proof
- `partial` — we ship some of it; specify the gap
- `gap` — we don't ship it; finding required (gap rows are
  launch-quality blockers)
- `non-goal` — explicit product decision; cite the decision

## A. Classic Stellar (pre-Soroban)

| Surface | Status | Evidence |
| --- | --- | --- |
| SDEX classic operations | `covered` | `internal/sources/sdex/` decoder ships; live `/v1/pools` (EV-0043) returns `source:sdex` with `trade_count_24h: 35082` and `volume_24h_usd: $516,595`. **Differentiator:** CG/CMC have NO native SDEX trade observation — they backfill from CEX prices |
| SDEX classic effects parsing | `covered` | `internal/sources/sdex/decode.go`; live trade count confirms decoder runs |
| Account-entry observation (ADR-0021) | `covered` | `internal/sources/accounts/` package feeds Algorithm 1 XLM supply derivation; result visible via `/v1/assets/native circulating_supply: 500018068120000000` (EV-0032) |
| Trustline observation | `covered` | `internal/sources/trustlines/` feeds Algorithm 2 classic-asset supply |
| Claimable balance observation | `covered` | `internal/sources/claimable_balances/` shipped; contributes to circulating-supply exclusion |
| Classic liquidity pool observation | `covered` | `internal/sources/liquidity_pools/` decoder for LP reserves |
| SAC wrapper balance observation | `covered+` | **`/v1/sac-wrappers` (F-0092 POSITIVE) exposes the SAC↔SEP-23 contract-id map for 40+ tokens** — CG/CMC have nothing comparable; this is a unique Stellar differentiator |
| CAP-67 unified events (Whisk) | `covered` | Dispatcher handles both pre + post P23 (CLAUDE.md "Things that will surprise you" callout); single transfer/mint/burn event with `sep0011_asset` topic[3] |
| Classic-issued asset registry | `covered` | migration 0023 + `internal/storage/timescale/asset_registry.go`; `/v1/network/stats` reports `assets_indexed: 189996` (EV-0031) |
| Classic asset stats 5m CAGG | `covered` | migration 0024 hypertable + CAGG materialisation; reads via `/v1/markets` |

## B. Soroban (post-protocol-20)

| Surface | Status | Evidence |
| --- | --- | --- |
| Soroban contract events catch-all (ADR-0029) | `covered+` | `internal/sources/sorobanevents/` + migration 0041 + 6 SQL-backfill subcommands — F-0079 POSITIVE. **Differentiator:** CG/CMC don't even index contract events |
| Per-event decoder coverage matrix | `covered` | W35 `every-event-coverage.tsv` has 81 rows; classified_by_decoder=yes for all but 5 confirmed DeFindex gaps (F-0018) |
| Contract-call decoder (Band-style) | `covered` | `internal/dispatcher` ContractCallDecoder interface (PR #168); Band, Soroswap-Router, and RedStone op-args plumbing use it |
| WASM upgrade detection | `covered` | `cmd/ratesengine-ops/wasm-history` subcommand; every BackfillSafe=true source has an audit log (F-0059 POSITIVE) |
| Per-source WASM audit framework | `covered+` | `docs/operations/wasm-audits/` with 16 audit files; **Differentiator:** the structural decoder-correctness gating mechanism CG/CMC have no analog for |
| BackfillSafe flag governance | `covered` | `internal/sources/external/registry.go` ships per-source `BackfillSafe` bool gated by WASM audit; default-false for unaudited Soroban; F-0079 confirms wiring |

## C. Soroban DEX / AMM coverage

| Source | claimed events | gap | Status |
| --- | --- | --- | --- |
| Soroswap (pair) | swap, sync, deposit, withdraw, skim | rc.81 added `skim` (#28) | `covered` per W35 |
| Soroswap (factory) | new_pair | — | `covered` per W35 |
| Soroswap (router contract) | swap_exact / swap_tokens-for-exact (call-decoder) | admin functions explicitly out of scope per `events.go:34-38` | `covered` per W35 |
| Phoenix (pool volatile) | swap (8-field), provide_liquidity, withdraw_liquidity, bond, unbond | rc.81 added provide/withdraw + bond/unbond (#27) | `covered` per W35 |
| Phoenix (pool stable) | same | shared decoder path | `covered` |
| Phoenix (stake per-pool) | bond, unbond | rc.81 (#27) | `covered` |
| Aquarius (v2) | swap, deposit, withdraw | — | `covered` per W35 |
| Comet (POOL namespace) | swap, join_pool, exit_pool, deposit, withdraw | rc.81 added all 5 (#26) | `covered` per W35 |
| Blend (pool money-market) | 13 events | rc.81 added (#25) | `covered` per W35 |
| Blend (pool admin) | 7 events | rc.81 added (#25) | `covered` per W35 |
| Blend (auctions, legacy) | new_auction, fill_auction, delete_auction | — | `covered` per W35 |
| DeFindex (vaults) | strategy deposit/withdraw | **5 gaps:** factory create / factory n_fee / strategy harvest / vault rebalance / vault admin (F-0018) | `partial` |

## D. Oracles

| Source | What it emits | Our handling | Status |
| --- | --- | --- | --- |
| Reflector DEX contract | (topic[0]=REFLECTOR, topic[1]=update) | Topic-based dispatch with source-variant tag (EV-0009) | `covered` |
| Reflector CEX contract | same | same | `covered` |
| Reflector FX contract | same | same | `covered` |
| Redstone Adapter | WritePrices (with op_args for feed_id) | RawEventSink + op_args plumbing (PR #166) | `covered`; EUROC + BENJI feed-id fix landed (F-0111) |
| Band (Stellar) | zero events; relay/force_relay calls | ContractCallDecoder (PR #168) | `covered` |
| Chainlink (off-chain HTTP) | per-feed `latestRoundData()` polled at 30s | `internal/sources/external/chainlink/poller.go` (EV-0044) | `covered`; **Differentiator:** included as a 4th oracle parallel to Reflector/Band/Redstone |

## E. Bridges

| Source | Status | Evidence |
| --- | --- | --- |
| CCTP v2 (3 contracts: token messenger, message transmitter, forwarder) | `covered` | `internal/sources/cctp/` + cctp_events; WASM audit 2026-05-26 (registry.go:80); J30 trace |
| Rozo intent-bridge (3 contracts) | `covered` | `internal/sources/rozo/` + rozo_events; WASM audit 2026-05-26 (registry.go:84) |

## F. Stablecoins / SEP-41

| Surface | Status | Evidence |
| --- | --- | --- |
| SEP-41 token transfer / mint / burn / clawback | `covered+` | `internal/sources/sep41_supply/` covers mint/burn/clawback (Algorithm 3 supply); sibling `internal/sources/sep41_transfers/` covers transfer/approve/set_admin/set_authorized for per-account audit-trail (migration 0047 + `GET /v1/contracts/{id}/transfers`). F-0021 closed 2026-05-27. **Differentiator:** per-account net-position queries (CG/CMC cannot do this — they only see exchange-side flows). |
| SEP-41 supply derivation via mint/burn delta | `covered` | `internal/supply/sep41.go` Algorithm 3 per CLAUDE.md |
| Stablecoin fiat-proxy late binding | `covered+` | ADR-0026 + `internal/aggregate/stablecoin.go`; **Differentiator:** late binding at aggregator (not decoder) preserves depeg detection vs eager normalization |
| Depeg detection | `covered` | `internal/divergence/depeg_test.go`; per-stablecoin tracking via verified catalogue |
| Per-stablecoin tracking (USDC, USDT, PYUSD, EUROC, EUROB, MXNe) | `covered` | `internal/currency/data/seed.yaml` hand-curated YAML embedded via go:embed (R-018) |

## G. Asset identity / SEP standards

| Surface | Status | Evidence |
| --- | --- | --- |
| SEP-1 toml resolution | `covered` | `internal/metadata/sep1.go`; singleflight cache for per-domain resolution |
| SEP-1 image_url overlay | `covered` | `internal/api/v1/assets_sep1.go` |
| Cross-network asset identity (`/v1/assets/{slug}` dual-shape) | `covered+` | `assets.go` + `assets_global.go`: catalogue-slug returns `GlobalAssetView` (EV-0033 USDC with `networks[]`), canonical asset_id returns `AssetDetail` (EV-0032 native). **Differentiator:** multi-network asset model |
| Verified-currency catalogue | `covered+` | `internal/currency/data/seed.yaml`; **Differentiator:** hand-curated YAML embedded via go:embed prevents accidental "verified" badge on attacker tokens; feeds CG ID mapping (`coingecko_id: usd-coin` per EV-0033) |
| Unverified-collision warning | `covered` | `/v1/assets/{slug}` distinguishes catalogue slug from canonical asset_id; flags collisions |
| SEP-40 (Stellar oracle interface) | `covered` | `/v1/oracle/lastprice`, `/v1/oracle/x_last_price` route to SEP-40 surfaces (HTTP 500 under cascade per F-0086, but interface present) |

## H. Stellar-only metrics

| Metric | Status | Evidence |
| --- | --- | --- |
| Ledger close time | `covered` | `/v1/ledger/tip` returns `latest_ledger: 62746377, lag_seconds: 2` (EV-0031) |
| Stream of ledger closes | `covered` | `/v1/ledger/stream` SSE route at `server.go:933` |
| Per-source TVL | `covered` | migration 0021 + `/v1/lending/pools` (HTTP 500 under cascade per F-0087, but interface ships) |
| Source contributions to VWAP | `covered` | migration 0026 + `vwap_provenance` Redis key; `/v1/methodology` documents the source-class filter |
| Per-source 24h volume | `covered` | `internal/storage/timescale/sources_stats.go` + `/v1/diagnostics/ingestion sources[].trade_count_24h` (sub. F-0095 inconsistency caveat) |
| Galexie archive tip lag (operator metric) | `covered` | runbook `galexie-archive-tip-lag.md` exists under `docs/operations/runbooks/` |
| Per-region archive completeness (ADR-0017 tier A/B/C/D) | `covered` | `internal/archivecompleteness/` daemon + 4 tiers + alert rules in `archive-completeness.yml` |
| Cross-anchor archive fill | `covered` | `internal/archivecompleteness/cross_anchor_fill.go` shipped per ADR-0017 X1.7 |

## I. Operator-facing depth (not user-facing, but signals platform credibility)

| Surface | Status | Evidence |
| --- | --- | --- |
| Per-source decoder stats CAGG (migration 0020) | `covered` | `internal/dispatcher/statsflush/` rolls stats to the hypertable |
| Source-entry counts table (migration 0035) | `covered` | `internal/storage/timescale/sources_stats.go` + `seed-entry-counts` subcommand |
| Pools-per-source CAGG (migration 0036) | `covered` | feeds `/v1/pools` (EV-0043 POSITIVE) |
| Hash-db of (ledger_seq → sha256(LCM)) | `covered` | `internal/hashdb/` — drift detector vs upstream rewrites |
| Verify-archive tier A/B/C/D | `covered+` | `cmd/ratesengine-ops/verify-archive` with rc.81 `TolerateTrailingMissing` (F-0099/F-0070); **Differentiator:** four-tier defence-in-depth integrity verification |
| WASM-history extract + audit framework | `covered+` | `cmd/ratesengine-ops/wasm-extract` + `wasm-history` + `docs/operations/wasm-audits/` 16 audit files; **Differentiator** |
| Cross-region check | `covered` | `cmd/ratesengine-ops/cross-region-check` + `cross-region-monitor` (F-0084 POSITIVE) |
| soroban_events SQL-as-historical-backfill | `covered+` | W27 + W29 + ADR-0029 + 6 backfill subcommands (F-0079 POSITIVE); **Differentiator:** SQL queries instead of MinIO walks for new decoders |
| `scan-soroban-events` operator diagnostic | `covered` | `cmd/ratesengine-ops/main.go` |
| `verify-decoders` operator diagnostic | `covered` | `cmd/ratesengine-ops/main.go` |
| `extract-wasm-from-galexie` | `covered` | `cmd/ratesengine-ops/main.go` |

## J. Free-form: surfaces we should ship but don't yet (find these)

| Surface | Rationale | Status |
| --- | --- | --- |
| Per-asset Soroban contract holder distribution | CG/CMC can't get this; we can if we observe SAC + sep41_supply events | `?` |
| Per-pool LP-token holder distribution | Stellar-specific; CG/CMC indifferent | `?` |
| Per-account net position (across SDEX + Soroban) | Aspirational | `?` |
| Soroban events explorer search (free text on topic_0_sym) | Pure differentiator on top of soroban_events | `?` |
| WASM-version history explorer | Operator-facing on launch day, eventually customer-facing | `?` |

## Audit pass output

Each `?` becomes one of `covered` / `partial` / `gap` / `non-goal`
with evidence. `gap` rows are launch-quality findings; this is
where the moat lives or dies.

The "we ship X but CG/CMC do not" rows (sections C-I, J) are
the brand positioning. Loss of any of these to launch-day
disrepair is a `critical`/`high` finding.
