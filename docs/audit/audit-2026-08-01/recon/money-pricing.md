# Recon — money & pricing (audit-2026-08-01)

- **Derived:** 2026-08-01 against HEAD `f8c099ee`, cold, read-only. Evidence = current code/schema + non-mutating command output only. The 2026-07-16 recipe §1-6 was treated as claims and re-derived; deltas vs its §5 are flagged inline as **[Δ-recipe]**.
- **Scope:** trades ingestion → USD stamping → VWAP/TWAP/OHLC → CAGGs → serving; supply (3 algorithms); divergence; oracles (redstone/reflector/band); stablecoin fiat-proxy; decimals/scaling boundaries; billing/Stripe (now fully wired — a NEW money surface vs the old recipe).

---

## 1. Money flows (17)

Stellar Index moves no user funds except Stripe billing (flow F13); everywhere else "money" = the served price/volume/supply numbers, which are the product.

**F1 — On-chain DEX trades (projected).** Galexie MinIO → `internal/ledgerstream` → `internal/dispatcher` → per-venue decoders (`internal/sources/{soroswap,phoenix,aquarius,comet}/dispatcher_adapter.go`) → CH `contract_events` lake → `internal/projector` (per-source cursors, `internal/projector/projector.go:11-133`; `registry.go:122 buildSource`) → `timescale.Store.InsertTrade`/`BatchInsertTrades` (`internal/storage/timescale/trades.go:628,906`) → `trades` hypertable → CAGGs → API.

**F2 — SDEX trades (non-projected).** dispatcher events-goroutine → `internal/pipeline/sink.go` (`PersistEvents:183`, `HandleEvent:741`, `SinkMode` truth table `:42-96`, `IsProjectedEvent:454`) → same `InsertTrade` path. Amounts at 10^7 (Stellar classic).

**F3 — CEX trades.** `internal/sources/external/{binance,kraken,bitstamp,coinbase}` streamers/backfillers via the `external.Connector` framework (`framework.go`) → `InsertTrade`. Amount scale = 10^8 (`Metadata.AmountScaleDecimals()`, `framework.go:192-200`, default-8 on unset).

**F4 — FX quotes.** `internal/sources/external/{ecb,exchangeratesapi,polygonforex,frankfurter,forex(massive)}` pollers at 10^6 (`exchangeratesapi/events.go:78 DefaultDecimals=6`; registry `AmountDecimals: 6` at `registry.go:128-129`) → trades AND `fx_quotes` (`fx_quotes.go:45` upsert — **float64 in-process**, `:24-25`, NUMERIC column, CHECK rate_usd>0 in `migrations/0028`).

**F5 — usd_volume stamping (write-time).** 5-tier exactness waterfall in `trades.go:55-170` (`tradeUSDVolume`): tier 1 off-chain USD/peg quote (per-source scale), tier 2 on-chain operator-declared USD-pegged quote (10^7), tier 2b USD-pegged BASE leg (`tradeUSDVolumeViaUSDBase:197`), tier 3 FX-resolver estimate (`tradeUSDVolumeViaFX:221`, per-source decimals switch `:233-243` — CS-040), tier 4 XLM/10^7-base anchor (`tradeUSDVolumeViaXLMBaseAnchor:296`, widened 2026-07-22 to any 10^7-scale base; pure SEP-41 bases excluded — documented scope boundary). Lockstep checker `ClassifyUSDVolumeTier` + `stellarindex-ops verify-usd-volume` (`internal/ops/chops/verify_usd_volume.go`) + drift test `TestClassifyUSDVolumeTier_TracksTheWaterfall`.

**F6 — VWAP compute + Redis serve.** Aggregator `orchestrator.Tick` (`internal/aggregate/orchestrator/orchestrator.go:735`) → `refreshPairWindow:876` → pair expansion incl. fiat-proxy `ExpandTargetPairWithClassicPegs` (`:1309`; map in `internal/aggregate/stablecoin.go:24`) → `filterForVWAP:1656` (ClassExchange ∧ IncludeInVWAP only) → outlier filter (`aggregate/outliers.go`) → exact big.Rat `VWAP` (`aggregate/vwap.go:29`) → dust floor `dropForMinUSDVolume:1620` → freeze evaluation `evaluateAndMaybeFreeze:1148` → Redis SET (`:986`, keys via `internal/cachekeys`) + `flushContributions:1743` → `price_source_contributions` (`price_source_contributions.go:39` DO UPDATE; **Volume/Weight float64 in Go** `:16-17`) + SSE publish `:1118`.

**F7 — Price serving.** `/v1/price*` (`internal/api/v1/price.go`, `price_tip.go`) read Redis VWAP guarded by `pricingguard.GuardServedVWAP1m` (`internal/pricingguard/guard.go:71` → `aggregate.GuardServedVWAP`, `served_guard.go:104`, MAD band `robust.go:89`) with last-known-good fallback; decimals normalization `aggregate.AdjustPrice` (`decimals.go:101`) at `price.go:719`, `price_tip.go:360` — Redis-VWAP branch documented as already-normalized to avoid double-correct (`price.go:666-696`).

**F8 — CAGG history/OHLC/TWAP serving.** `prices_1m…1mo` CAGGs → `internal/storage/timescale/aggregates.go` (`HistoryPoints:299`, range variant `:472`, TWAP `:563`) → `/v1/history`, `/v1/ohlc`, `/v1/chart`, `/v1/twap`, `/v1/vwap`, `/v1/markets`. AdjustPrice applied at `ohlc.go:199-202`, `ohlc_series.go:394`, `history.go:221`, `markets.go:767`, `twap.go:108`, `vwap.go:170`, backed by `nonstandard_decimals_cache.go`. Closed-bucket predicate: **two forms coexist** — non-sargable `bucket + INTERVAL <= now()` at `aggregates.go:299,472` vs sargable `bucket <= now() - INTERVAL` at `:563` (see hot spots).

**F9 — OHLC fiat-combine + triangulation.** `/v1/ohlc` fiat targets combine per-peg legs (`api/v1/ohlc_fiat_combine.go:98`); composite chains `aggregate/triangulate.go:23,41 (TriangulateChain)`; confidence factors incl. $1k–$1M liquidity band (`aggregate/confidence/factors.go:61-62`, ceiling raised from $100k on 2026-07-25).

**F10 — Anomaly freeze lifecycle (NEW, ADR-0019).** `internal/aggregate/freeze/{freeze,lifecycle,recovery}.go` — duration/extension/unfreeze ladder, durable across Redis loss (migrations 0119, 0124), replacing the z=100 stop-gap; orchestrator `keepFrozenVWAPAlive:1102`.

**F11 — Supply (3 algorithms).** Alg-1 XLM fixed-total (`internal/supply/xlm.go:26,145`); Alg-2 classic Σ trustline+claimable+LP+SAC-wrapped from observer hypertables (`classic.go:130`; observers `internal/sources/{trustlines,claimable_balances,liquidity_pools,sac_balances}` → `classic_supply_observations.go:73,163,335,413` DO UPDATE upserts); Alg-3 SEP-41 Σmint−burn−clawback (`sep41.go:149`; events `sep41_supply_events.go:95` generation-guarded DO UPDATE). `Refresher.Tick` (`refresher.go:250`) gates: stale-component + **bounded** dormancy (`DefaultMaxDormantComponentLedgers`, `refresher.go:71-87` — F-1320 exception now fail-closed past horizon) → `asset_supply_history` (`supply.go:104` DO UPDATE via named constraint, generation-guarded) → `/v1/assets*` supply + market-cap.

**F12 — Supply cross-checks.** classic-vs-SAC wrap-class reconcile (`supply/crosscheck.go:265,331,379`; refresher `crosscheck_refresher.go:226`) + external refs (stellar-dashboard, CoinGecko: `divergence/supply.go:486,653`, staleness gate `:222`).

**F13 — Billing/Stripe (REAL user money — new since old recipe).** `POST /v1/webhooks/stripe` (`internal/api/v1/stripe_webhook.go:250`), HMAC signature verify (`:512 parseStripeWebhook`, crypto/hmac; 503 when signing secret unset per `config.go:1059`), tier upgrade (`:1056`), invoice-paid (`:918`), downgrade enforcement + key-budget clamp (`:685,784,814,862`), dead-letter for paid-but-unapplied (`migrations/0118`), claim-based idempotency (`migrations/0121`), platform bridge (`internal/platform/postgresstore/billing_store.go`). Quota/rate-limit enforcement is **fail-open by design** (`internal/usage/counter.go:273`; `internal/ratelimit/bucket.go:42-104` dwell-time inversion) — under-billing/over-serving risk, metered by counters.

**F14 — Oracle updates.** Reflector (3 contracts) + Redstone (`REDSTONE` batch events; feed attribution: OpArgs zip → signed-payload median (`redstone/payload.go`) → NEW exact StateWriteKeys subset (`redstone/decode.go`, dispatcher plumbing `dispatcher/dispatcher.go` — commits 7c9cc0ee, 0fd8eb7f); fail-closed refusal errors ErrAmbiguousSubset/ErrMissingOpArgs/ErrUpdaterMismatch/ErrStateWriteFeedMismatch) + Band (ContractCallDecoder, zero events, E18/E9 scales) → `oracle_updates` (`oracle.go:49` generation-guarded DO UPDATE, CHECK price>0 `migrations/0003:36`) → `/v1/oracle*` + divergence oracle reference.

**F15 — Divergence cross-check.** `divergence/worker.go:221 NewService` → references (coingecko.go, chainlink.go via Alchemy HTTP, oracle.go:162 reading our own oracle_updates with per-source max-age `:128`) → `Compare` (`compare.go:126`, **float64 display-grade**) → `divergence_observations` + `/v1/diagnostics/divergence` + Δ% series endpoint (e7a6a544).

**F16 — DEX TVL / protocol KPIs at serve.** `api/v1/dex_tvl_cache.go:108,469-513` (`USDPriceAt` prices_1m ratio chains bridged through XLM), lake pool-state readers (`internal/storage/clickhouse` phoenix/comet readers, b251007a), bespoke DEX/lending/yield rollups (`timescale/bespoke_*.go`), `/v1/protocols` TVL snapshot (b1711519).

**F17 — SDEX order-book depth (NEW).** in-process live offer book + `/v1/…/depth` endpoint (3dbd941b, 2169+ lines) — money-adjacent serving surface with its own amount arithmetic, not yet covered by any prior audit.

---

## 2. Invariants — verified enforcement tiers

Tier = weakest writer. DB > RT > T > W > CI > C.

**INV-1 — i128/u128 never truncates (ADR-0003). Tier: CI+T (on-chain) / C (off-chain float transit).** Enforcers opened: `scripts/ci/lint-i128.sh` (grep `int64(x.Lo)`, prod code only) + `internal/canonical/i128_truncation_guard_test.go` (repo-wide go/types walk, `//i128:ok` escapes) + `lint-migrations.sh` (SQL). Off-chain blind spot persists: `coinbase/backfill.go:220-228` FormatFloat transit; divergence/confidence deliberately float64. **Unchanged vs recipe.**

**INV-2 — money = NUMERIC in SQL, string in JSON. Tier: DB (columns) / RT (marshal) / C (two float writers).** `canonical.Amount.MarshalJSON` (`amount.go:165-169`) strings-only; DB CHECKs: `trades` base/quote > 0 (`migrations/0001:30-31`), `oracle_updates.price > 0` (`0003:36`), supply ≥ 0 (`0005:23-27`), `sep41_supply_events.amount ≥ 0` (`0015:47`), `fx_quotes.rate_usd > 0` (`0028:59`). **[Δ-recipe] PROMOTION:** `/v1/changes` now serves money as strings via `moneyStr` (`api/v1/changes.go:66-70`) — the old "JSON numbers" violation is gone (float64 remains in-process, documented display-grade). **Unchanged weakness:** `fx_quotes` RateUSD/InverseUSD float64-in-Go writer (`fx_quotes.go:24-25,56-60`; read path returns NUMERIC text, `:146`); `price_source_contributions` Volume/Weight float64 (`price_source_contributions.go:16-17`).

**INV-3 — derived money values re-derivable. Tier: RT+DB-constraint (generation-guarded corrective upsert). [Δ-recipe] MAJOR PROMOTION from "NONE/VIOLATED".** Migrations 0109 (trades, oracle_updates, asset_supply_history) + 0110 (protocol tables) added `derive_generation bigint` and every writer flipped DO NOTHING → `DO UPDATE … WHERE <t>.derive_generation <= EXCLUDED.derive_generation`. Writer inventory (each opened): `trades.go:657-677` (InsertTrade) and `:983-997` (batch); `oracle.go:49`; `supply.go:104` (asset_supply_history); `sep41_supply_events.go:95-101` + batch `:535` (intra-batch dedupe added because DO UPDATE rejects dup keys); `classic_supply_observations.go:73,163,335,413`; protocol tables `aquarius_{admin,liquidity,rewards}.go`, `blend_{admin,auctions,emissions,emitter,positions,backstop_events}.go`, `cctp_events.go:141`, `comet_liquidity.go:127` — all carry the "replaces DO NOTHING" generation arm. Gen-0 = live; positive gen = ops re-derives (time.Now().Unix()). Residual DO NOTHING in money-adjacent tables: `asset_registry.go:183` (identity row, not a value), `diagnostics.go:486` (one-row-per-key by design), `blend_auctions.go:68` (intra-op collision guard). Plus post-hoc checker `verify-usd-volume` (6c51c760).

**INV-4 — one writer per Soroban domain (ADR-0031/0032). Tier: RT+T live / C-with-mitigation for ops.** `SinkModeForProjector` (`pipeline/sink.go:89-96`) + `lockstep_ast_test.go`. Ops re-derives remain a second writer class but are now generation-stamped (deliberate-win semantics rather than silent absorption). `routed_via.go:91` UPDATE on `trades` verified to touch ONLY the `routed_via` label column (`:85-105`) — metadata writer, not a money-value writer. **[Δ-recipe]:** the old "second live UPDATE racing projector inserts" concern is label-only.

**INV-5 — coverage data-derived; projector cursor honesty. Tier: W (verdict) / RT (cursor state machine). [Δ-recipe] PROMOTION:** `projector.go:94-115` now implements the documented disposition machine — TRANSIENT sink failure HOLDS the cursor, PERMANENT is counted+skipped (`heldRow:329`, `:386-401`); the old doc-vs-code contradiction ("advanced unconditionally") is resolved in code.

**INV-6 — amounts>0 / price>0 / supply≥0. Tier: DB (CHECK) + RT (Validate).** See INV-2 CHECK list. Batch path relies on DB CHECK for positivity (unchanged).

**INV-7 — closed buckets only (ADR-0015). Tier: RT (SQL predicate), no failure-case test found.** Predicate present on every CAGG read in `aggregates.go` — but in TWO syntactic forms (`:299,472` non-sargable vs `:563` sargable). Correctness holds; perf regression risk vs the 2026-06-20 sargable fix. Audit item: sweep + a shared predicate builder.

**INV-8 — non-7dp assets never serve unscaled. Tier: RT (AdjustPrice at every serve path) + cache (`nonstandard_decimals_cache.go`).** Call-site inventory: price.go:719, price_tip.go:360, ohlc.go:199-202, ohlc_series.go:394, history.go:221, markets.go:767, twap.go:108, vwap.go:170, server.go:223,1018. `/v1/price/at` covered per `nonstandard_decimals_cache.go:27` + `doc.go:87` (bucket-pinned). **[Δ-recipe]:** old trap-7 gaps (price/at, changes, market-cap 10^7 hardcodes) appear closed — verify market-cap path specifically in the audit.

**INV-9 — stablecoin fiat-proxy is aggregation-time only. Tier: RT (single map) + C (decoders keep raw pair).** Map `aggregate/stablecoin.go:24`; consumers: orchestrator `:1309` + `ohlc_fiat_combine.go:98`. No ingest-time ProxyTrade callers found outside aggregate/.

**INV-10 — VWAP inputs = ClassExchange ∧ IncludeInVWAP. Tier: RT.** `filterForVWAP` (`orchestrator.go:1656-1665`); registry: all oracles/aggregators/routers `IncludeInVWAP:false` (`registry.go:45-102`), unknown sources default excluded (`:12`).

**INV-11 — dust floor (min_usd_volume). Tier: RT with a DOCUMENTED HOLE.** `dropForMinUSDVolume:1620`; quote-aware per-trade valuation (034a014a). **The hole:** a pair whose quote has no recognized USD peg SKIPS the floor entirely — explicit warn at `orchestrator.go:1628` "NOT gated against dust-trade manipulation". This is the weakest serving-integrity link found.

**INV-12 — oracle attribution honest-blind, never misattributed. Tier: RT (fail-closed refusal errors) + T (golden fixtures from r1 ledgers 59258375, 62056824).** Three-stage redstone resolution (StateWriteKeys exact subset → payload-median → refuse); OpArgs attached only when invoked contract == event contract (0fd8eb7f); CH-enricher twin parity (undecodable entry_xdr poisons the key).

**INV-13 — billing integrity. Tier: RT+T (HMAC verify, claim idempotency 0121, dead-letter 0118, downgrade clamps) / deliberate fail-open on quota enforcement** (`usage/counter.go:273`, `ratelimit/bucket.go` dwell-time). NEW invariant — absent from old recipe §5.

**INV-14 — supply freshness (quiet ≠ stale). Tier: RT.** Refresher stale-component gate + bounded dormancy (`refresher.go:51-87,355`). **[Δ-recipe] PROMOTION:** the F-1320 "accepts a stalled producer" exception is now bounded fail-closed.

### Weakest links (ranked)
1. **INV-11 dust-floor bypass** for unrecognized-USD-quote pairs — an unguarded VWAP-manipulation surface, self-declared in a log line.
2. **INV-2 float64 writers** — fx_quotes + price_source_contributions (unchanged since 2026-07-16); plus aggregator-internal float64 USD gating (`usdVolumeForPair:1352`) feeding the dust floor.
3. **INV-13 fail-open quota/rate-limit** — deliberate, metered, but the only revenue-enforcement line.
4. **INV-7 mixed closed-bucket predicate forms** — perf (not correctness) regression risk on the exact query shape that caused the 446ms incident.
5. **INV-1 off-chain float transit** — unchanged blind spot of the truncation guards.

---

## 3. Hot spots (triangulated: diff-size since 2026-07-25 × money-dimension × newness of seam; not recency alone)

1. **Redstone attribution rewrite** — 7c9cc0ee (+1165) & 0fd8eb7f (+1043, security-labeled, 2026-08-01): brand-new `StateWriteKeys`/OpArgs-provenance seam through dispatcher AND the CH re-derive twin; parity between the two extraction paths is asserted, not DB-enforced.
2. **Billing/Stripe wave** — 52105fdb (+3647), a6644402 (+2046), 3cebda7b (+2132): downgrade enforcement, kill switches, dead-letter, mint quota — real-money, all < 1 week old.
3. **Freeze lifecycle (ADR-0019)** — 28f6791a (+2714) + 0993dee8 (+1998) + migrations 0119/0124: served-price gating logic replaced wholesale.
4. **SDEX depth + per-address trades/activity serving** — 3dbd941b (+2169), 9b879966 (+1572), 893c66ef: new read surfaces with fresh amount arithmetic and indexes (0123).
5. **usd_volume correctness chain** — 6c51c760 (verify-usd-volume + watermark pinning), 034a014a (quote-aware dust floor), e9803e69 (triangulation chains + 7th confidence factor + $1M ceiling): the valuation waterfall and its checker moved together — lockstep drift is the risk.
6. **TVL/bespoke USD serving** — b1711519, e12dae07, b251007a, e2089283: new prices_1m ratio-chain USD conversions at serve time (dex_tvl_cache), outside the AdjustPrice audit trail above.

TODO density is near-zero in money packages (2 hits, both doc comments) — not a useful signal here; flag-based signal: new config `[trades].usd_pegged_classic_assets`, `[stripe]`, freeze lifecycle knobs, `storage.clickhouse_projector_source`.

## 4. Checklist deltas & traps (money dimension)

- **ADD:** Stripe webhook — signature bypass, replay/idempotency (claim table), downgrade-clamp completeness across BOTH Postgres keys and Redis cache (`downgradeRedisAPIKeys:862`), dead-letter resolution path.
- **ADD:** redstone dispatcher-vs-CH-enricher twin parity (same event, two extraction paths — assert byte-equal attribution).
- **ADD:** `verify-usd-volume` lockstep — mutate a tier and confirm `TestClassifyUSDVolumeTier_TracksTheWaterfall` fails.
- **ADD:** dust-floor bypass pairs — enumerate live pairs whose quote lacks a USD peg; each is unguarded.
- **ADD:** F17 depth endpoint + F16 TVL chains — decimals + closed-bucket + big.Rat discipline on the newest serve surfaces.
- **KEEP:** per-source decimals table (7 on-chain / 8 CEX / 6 FX / E18-E9 Band) — `AmountScaleDecimals` default-8-on-unset is a silent trap for a new FX venue that forgets `AmountDecimals: 6`.
- **RETIRE from old recipe:** INV-3 "no tier/violated" hunting pattern (now generation-guarded fleet-wide — instead audit that ops entry points actually STAMP a positive generation); "/v1/changes JSON numbers"; "projector cursor advances past sink failures".
