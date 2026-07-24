# Audit findings — audit-2026-07-23 · chunk 1 (money surface)

> **PARTIAL run** — chunk 1 hit the account session limit mid-verify (50 of 215 agents failed: 9 finders, ~35 skeptics, 3 negspace). NOT fully converged. Below = the skeptic-VERIFIED `confirmed` set the workflow joined before the limit; more candidate findings await re-verify on resume. Commit HEAD 1233689f. Chunks 2–6 pending.

## Confirmed (skeptic-survived)

| # | Sev | Exp | Dim | Verdicts | Finding |
|---|---|---|---|---|---|
| 1 | CRITICAL | LIVE | DAT-15 | C/C/C | projected-rebuild re-derives DEX trades with a high derive_generation but installs no USD-volume resolver, so the DO UPDATE overwrites every re-derive |
| 2 | HIGH | LIVE | MNY-04 | C/C | Freshness gate accepts a stalled component observer's frozen supply, re-stamped at the current ledger, with no alert |
| 3 | HIGH | LIVE | COR-14 | C/C | Confidence is a NORMALIZED geometric mean, but ADR-0019 (and the freeze threshold) require an UN-normalized product — the Phase 2 anomaly freeze fails |
| 4 | HIGH | LIVE | MNY-06 | C/C | Served /v1/price VWAP combines the two market directions by TRADE COUNT, not volume, yielding a non-VWAP price that is wrong on asymmetric-volume buck |
| 5 | HIGH | LIVE | MNY-22 | C/C | The multi-window baseline 'frog-boiling' defense cannot detect the slow-drift attack it is built for, because it z-scores per-bucket RETURNS, not pric |
| 6 | HIGH | LIVE | MNY-06 | C/C | Served /v1/history and default /v1/chart VWAP read only ONE stored market orientation, silently dropping all reverse-orientation liquidity and any buc |
| 7 | HIGH | LIVE | MNY-06 | C/C | Multiple additional served/decision-driving VWAP reads combine the two market directions by TRADE COUNT instead of volume, yielding a non-VWAP price ( |
| 8 | HIGH | GATED | MNY-22 | C/C | Stablecoin-proxy USD-volume gate silently ignores all crypto:USDT/USDC/DAI/PYUSD/USDP legs, wrongly dropping (or permanently blacking out) real high-v |
| 9 | MEDIUM | LIVE | DAT-10 | C/C | RecentOperations returns duplicate rows from unmerged RMT parts |
| 10 | MEDIUM | LIVE | DAT-10 | C/C | OperationsByTx returns duplicate rows from unmerged RMT parts |
| 11 | MEDIUM | LIVE | DAT-10 | C/P | StreamEntryChanges emits duplicate rows from unmerged ledger_entry_changes |
| 12 | MEDIUM | LIVE | PRF-02 | C | Per-Row Query Fan-Out in GET /v1/accounts/{g}/movements SEP-41 Asset Resolution |
| 13 | MEDIUM | LIVE | PRF-02 | C | N+1 Query Pattern in /v1/accounts Wealth Ranking Price Lookup |
| 14 | MEDIUM | LIVE | MNY-05 | C/P | On-chain DEX sources use 1e8 instead of 1e7 in confidence USD-volume calculation |
| 15 | MEDIUM | LIVE | MNY-05 | C/C | No test coverage for on-chain sources in approxUSDVolume confidence calculation |
| 16 | MEDIUM | LIVE | MNY-01 | C/C | OHLC high/low extremes set by dust trades with negligible USD volume |
| 17 | MEDIUM | LIVE | DOM-02 | C/C | SEP-1 declared max_supply guard uses circulating as the floor instead of total, letting an issuer publish max_supply < total_supply |
| 18 | MEDIUM | LIVE | MNY-08 | R/C | XLM circulating can go negative — the crown-jewel algorithm lacks the zero-clamp that classic and SEP-41 both have |
| 19 | MEDIUM | LIVE | MNY-04 | C/C | Classic↔SAC reconciliation (E4/N-F3) is effectively unbuilt: the shipped cross-check only catches sac_total > classic_total, never a mint/escrow short |
| 20 | MEDIUM | LIVE | MNY-22 | C/C | /v1/ohlc series mode (?interval=) serves raw CAGG high/low with no outlier filtering, and the fiat-combine 2x band is a no-op on single-constituent bu |
| 21 | MEDIUM | LIVE | API-02 | C | Combined fiat OHLC open/close have zero outlier protection and are volume-weighted averages unbounded by the filtered high/low — the bar can violate H |
| 22 | MEDIUM | LIVE | MNY-22 | C/C | Stablecoin fiat-USD fallback treats USDC/USDT VWAP as USD 1:1 with no depeg/deviation bound; historical /v1/price/at and /v1/price/changes carry no di |
| 23 | MEDIUM | LIVE | DAT-11 | C/C | OHLC high/low continuous aggregates apply no notional/volume filter, so a single dust trade sets a bucket's served chart extreme |
| 24 | MEDIUM | LIVE | COR-14 | C | fiat:USD price/volume methodology diverges between single-shot endpoints (first-peg only) and the OHLC series (all pegs combined) — same query, differ |
| 25 | MEDIUM | LIVE | AGT-08 | C | InsertTrade/BatchInsertTrades documentation still describes ON CONFLICT DO NOTHING while the SQL is DO UPDATE, hiding the overwrite semantics from fut |
| 26 | MEDIUM | LIVE | MNY-22 | C/C | Tier-3 direct FX resolver reads a prices_1m VWAP with no minimum-volume floor or deviation band before it lands in usd_volume |
| 27 | MEDIUM | LIVE | DAT-11 | C/C | prices_* twap column is labelled time-weighted but is an equal-weight per-trade mean, and feeds the served TWAP CAGGs |
| 28 | MEDIUM | LIVE | COR-14 | C/C | Non-USD fiat asset-detail view serves price_usd=null AND market_cap_usd=null in production (detail path never got the fx_quotes fallback the listing h |
| 29 | MEDIUM | LIVE | AGT-08 | C | SourceCountFactor understates single-source confidence ~2.5x vs the ADR/comment claim (0.119 vs '~0.3') |
| 30 | MEDIUM | LIVE | COR-01 | C | MAD==0 (pegged/quiet window) makes any nonzero move register as z=+Inf, and MaxZScore takes the max across windows — a single quiet 1d window false-tr |
| 31 | MEDIUM | LIVE | SEC-08 | C/C | isSafeImageURL is scheme-only and its docstring falsely guarantees XSS safety for issuer-controlled SEP-1 fields |
| 32 | MEDIUM | LIVE | DAT-11 | C/C | /v1/changes serves ath_value/atl_value labeled as all-time high/low but computed from only ~30 days of history |
| 33 | MEDIUM | LIVE | DAT-09 | C/C | /v1/coverage `complete=true` can be served for a trade source that has a contiguous served-tier projection gap at the retention-window floor |
| 34 | MEDIUM | LIVE | API-02 | C | /v1/coverage first_problem_ledger is served as 0 (documented "0 = none") even when projection_ok=false, mislabeling a real problem as none |
| 35 | MEDIUM | LIVE | AGT-08 | C | flushChanges carries a stale, self-contradicting comment asserting ledger_entry_changes is never written and 'do not assume it is populated' — the ent |
| 36 | MEDIUM | LIVE | MNY-22 | C/C | Median/MAD outlier filter drops every honest differing-price trade whenever a trade-count majority sits at one exact price (MAD=0), and the config doc |
| 37 | MEDIUM | LIVE | DAT-03 | C/C | Migration 0114's documented historical-recovery command does not exist and cannot recover the truncated topics it promises to |
| 38 | MEDIUM | LIVE | MNY-22 | C/C | Triangulated chain price consumes cached leg VWAPs (including frozen/stale last-known-good values) with no freshness or freeze check, silently propaga |
| 39 | MEDIUM | LIVE | MNY-19 | C/C | SEP-1 max_supply overlay uses circulating (not total) as its lower bound, so a hostile/sloppy issuer can poison the served max_supply to a value below |
| 40 | MEDIUM | LIVE | MNY-04 | C/C | Coverage/completeness trust endpoint has no live-tip freshness gate — a stalled indexer serves complete=true / coverage_pct=100 with a freshly re-stam |
| 41 | MEDIUM | LIVE | MNY-02 | C/C | SEP-41 Algorithm 3 always reads AdminBalance=0, so the default (no locked-set) circulating equals total yet is labelled admin_exclusion — the same CS- |
| 42 | MEDIUM | LIVE | MNY-04 | R/C | Classic-vs-SAC cross-check compares each side's latest snapshot with no ledger alignment, so a lagging SAC snapshot makes the subset-bound invariant u |
| 43 | MEDIUM | LIVE | COR-14 | C/C | Confidence score is forced to identically 0 for all non-USD-quoted pairs and for any window whose summed USD volume is under $1,000 — the liquidity 'n |
| 44 | MEDIUM | LIVE | COR-14 | C | baselineAgeDays reinterprets baseline 'age' as sample count (N/1440) and discards computedAt, so a sparsely-traded but calendar-mature asset is perman |
| 45 | MEDIUM | GATED | DAT-15 | C/C | Current-state money reads depend on the composite-version (C2-4c) RMT that the codebase documents as NOT deployed on production; on the live Replacing |
| 46 | MEDIUM | GATED | MNY-05 | C/C | USD-volume manipulation gate scales by a hardcoded per-pair quote-decimals (fiat:USD=8) instead of the per-source AmountScaleDecimals the registry and |
| 47 | MEDIUM | GATED | DAT-completeness | C/C | Completeness projection reconcile is hard-floored at tip-1.5M — served trades retained below that floor are never reconciled, so complete=true can hid |
| 48 | MEDIUM | GATE | DAT-03 | C/C | Readers and the write path reference intra_ledger_seq / version columns with no capability guard or graceful fallback, making live ingest and several  |
| 49 | LOW | LIVE | API-02 | C | Truncated flag false-positives on exactly-full windows across /v1/vwap, /v1/twap, /v1/ohlc (== cap instead of > cap) |
| 50 | LOW | LIVE | COR-12 | R/C | Market-cap / FDV mis-scale for non-7-decimal Soroban tokens: price-scale and supply-scale read from two independent, unreconciled decimals sources |
| 51 | LOW | LIVE | COR-01 | C | /v1/anomalies live "firing count" is capped at 500, silently undercounting a freeze storm |
| 52 | LOW | LIVE | COR-03 | C | Zero circulating supply yields null market cap on the listing but "0.00" on the detail, and the two market-cap implementations round differently |
| 53 | LOW | LIVE | AGT-08 | C | coverage_pct Go doc comment claims "100 means reaches tip" but the value is a [0,1] fraction |
| 54 | LOW | GATED | COR-14 | C | attachFiatMarketCaps skips all fiat market caps when PriceReader is nil even though fxHistory could supply them |
| 55 | INFO | LIVE | COR-01 | C | ClassicSupplyComponents.Trustline docstring claims the sum excludes the issuer's own trustline, but the backing SQL sums all accounts — a claim that c |

## Detail — CRITICAL + HIGH

### [CRITICAL/LIVE] DAT-15 — projected-rebuild re-derives DEX trades with a high derive_generation but installs no USD-volume resolver, so the DO UPDATE overwrites every re-derived trade's usd_volume with NULL
- **locations:** internal/ops/chops/projected_rebuild.go:137, internal/ops/chops/projected_rebuild.go:502, internal/storage/timescale/trades.go:606, internal/storage/timescale/trades.go:913
- **failure:** Operator runs `stellarindex-ops projected-rebuild -source soroswap -write -from N -to M` (a documented, supported re-derive command) to correct any soroswap projection defect. Every soroswap trade in [N,M] that the live indexer had correctly priced (via tier 2/3/4 with the resolver installed in the indexer main.go:196) is re-inserted with usd_volume=NULL and derive_generation=now(); the guard passes (now >> 0) and the correct dollar values are overwritten with NULL across the whole range. Every downstream aggregate that sums usd_volume/volume_usd — the prices_* CAGGs' volume_usd column, /v1/ma
- **minimal fix:** In projected_rebuild.go, after SetDeriveGeneration (line 137) add the same timescale.InstallUSDVolumeResolution(store, cfg.Trades.USDPeggedClassicAssets, cfg.Supply.SACWrappers) call ch_rebuild.go:172 and backfill_external.go:127 already make; return its error.
- **best fix:** Make the destructive combination unrepresentable: have SetDeriveGeneration(>0) refuse to write trades (or make BatchInsertTrades/InsertTrade fail closed) unless the USD-volume resolvers are installed, so any future re-derive entry point that forgets the wiring aborts instead of silently nulling money data. Alternatively fold generation-stamping and resolver-installation into a single mandatory 'en

### [HIGH/LIVE] MNY-04 — Freshness gate accepts a stalled component observer's frozen supply, re-stamped at the current ledger, with no alert
- **locations:** internal/supply/refresher.go:296-352, internal/supply/refresher.go:319-321
- **failure:** The USDC trustline observer process dies at ledger M while the chain keeps advancing. Every subsequent Refresher.Tick computes a snapshot with MinComponentLedger=M (frozen) and LedgerSequence=current tip. Because lastComponentLedger[USDC] already equals M from the last healthy tick, the gate marks every tick dormant and inserts a row into asset_supply_history stamped at the fresh ledger with USDC's total/circulating frozen at M — no stale_component rejection, no page (OutcomeKindDormant is benign). If 100M USDC is minted/burned after the crash, the API serves a stale total_supply/circulating_s
- **minimal fix:** Do not re-stamp a dormant snapshot at the fresh chain tip: when accepting a dormant asset, set snap.LedgerSequence (and ObservedAt) to MinComponentLedger's ledger so the served row is honestly dated to its actual observation, and keep emitting a non-benign staleness signal. At minimum, cap dormant acceptance (e.g. reject once the gap exceeds a hard ceiling well above the per-asset threshold) so an
- **best fix:** Replace the ambiguous 'MinComponentLedger unchanged' heuristic with an independent producer-liveness signal: query each contributing observer's own last-write/heartbeat timestamp. 'Asset dormant' = observers are alive AND up-to-date but the balance simply didn't change; 'producer stalled' = an observer's heartbeat is old. Only the former should be accepted (and even then dated to the true observat

### [HIGH/LIVE] COR-14 — Confidence is a NORMALIZED geometric mean, but ADR-0019 (and the freeze threshold) require an UN-normalized product — the Phase 2 anomaly freeze fails to fire on the single-source manipulation it was built to stop
- **locations:** internal/aggregate/confidence/score.go:194-204, internal/aggregate/confidence/doc.go:19-23, docs/adr/0019-anomaly-response-and-confidence-scoring.md:114-123, internal/aggregate/orchestrator/phase2_freeze.go:88-93
- **failure:** A single-source pool (source_count=1) reports a manipulated price on a long-tail Stellar asset with decent single-venue liquidity (>$100K bucket volume) and no external cross-oracle reference. The move is 8σ (z=8) against the per-asset baseline. Freeze requires confidence<0.10 AND z>5 AND source_count<=1: z=8>5 ✓, source_count=1<=1 ✓, but implemented confidence = 0.354, NOT < 0.10 → freeze does NOT fire. The manipulated bucket is published on /v1/price with confidence≈0.35 (a misleadingly moderate value that downstream consumers gating at >0.30 will accept). To reach confidence<0.10 with these
- **minimal fix:** Make the combiner match the ADR the freeze threshold was calibrated against: drop the /totalWeight normalization for the default (weights=1) case, i.e. conf := math.Exp(logSum) so a single low factor dominates as ADR-0019:111 requires; update doc.go/score.go comments to agree.
- **best fix:** Deliberately reconcile formula and threshold: either (a) keep the normalized geometric mean and RE-CALIBRATE ConfidenceMaxFreeze (and the 0.30 auto-unfreeze) to the mean scale with a regression test that asserts a modeled USTRY single-source z=8 bucket freezes, or (b) adopt the un-normalized product per ADR and give weights explicit product semantics. Add a test that reintroduces the normalization

### [HIGH/LIVE] MNY-06 — Served /v1/price VWAP combines the two market directions by TRADE COUNT, not volume, yielding a non-VWAP price that is wrong on asymmetric-volume buckets and manipulable by splitting trades
- **locations:** internal/storage/timescale/aggregates.go:935-938 (latestClosedVWAP1mTemplate), internal/storage/timescale/aggregates.go:436-438, internal/storage/timescale/aggregates.go:672-674, internal/storage/timescale/aggregates.go:998-1000 (EXCURSION: outside internal/aggregate scope)
- **failure:** Latest closed 1m bucket for XLM/USDC: forward direction = 1 fill of 1,000,000 XLM @ 0.10 USDC (vwap=0.10, trade_count=1); flipped direction = 100 tiny fills @ 0.12 USDC (1/vwap_flip=0.12, trade_count=100). Volume-true VWAP ~= 0.10; the served value = (0.10*1 + 0.12*100)/101 = 0.1198, a ~20% error. An actor can push the served price toward their side by emitting many minimal-size fills in one direction (cost bounded only by fees + the +/-3x GuardServedVWAP band), because count, not size, sets the weight.
- **minimal fix:** Weight each direction by its forward base volume: replace *trade_count with *(CASE WHEN base_asset=$1 THEN volume ELSE vwap*volume END) in the combine (and the SUM denominator), so the blend is a true volume-weighted VWAP.
- **best fix:** Combine directions on summed base/quote volumes rather than pre-averaged per-direction vwaps: combined_vwap = (Σ forward-oriented quote)/(Σ forward-oriented base) across both directions, which is exact and count-independent; add a test with 1 large forward fill vs many tiny flipped fills asserting the result tracks volume.

### [HIGH/LIVE] MNY-22 — The multi-window baseline 'frog-boiling' defense cannot detect the slow-drift attack it is built for, because it z-scores per-bucket RETURNS, not price-level deviation from the long window — the 30d window adds no protection over 1d
- **locations:** internal/aggregate/baseline/multi.go:19-41, internal/aggregate/baseline/multi.go:79-140, internal/aggregate/baseline/baseline.go:86-95, internal/aggregate/orchestrator/confidence.go:90-92
- **failure:** An attacker (or a compromised single source) drifts an asset ~0.5%/day for two weeks (~7% total, arbitrarily large over more weeks). Every per-bucket return stays ~3e-6, well inside the 30d return-MAD (~1e-4). MaxZScore returns z~0.02 across all windows, so ZScoreFactor stays near 1.0, the confidence z-factor never drops, and the Phase 2 freeze (which requires z>5) never fires. The documented anti-manipulation control is silently inert; a coordinated slow manipulation of a served price is never flagged by the baseline.
- **minimal fix:** Additionally compute a level-based z-score: standardize the CURRENT price level against the distribution of price LEVELS in the 30d window (median/MAD of levels), and feed max(returnZ, levelZ) into the freeze/confidence path so a price that has drifted far from its long-window level fires even when per-bucket returns are small.
- **best fix:** Redefine the multi-window safeguard around level deviation from each window's median with the long window intentionally lagging (so drift accumulates against pre-drift data), keep the return-based z only for sudden-spike detection, and change TestMultiBaseline_FrogBoilingDefense to assert the long-window z actually exceeds the anomaly threshold for a realistic drift.

### [HIGH/LIVE] MNY-06 — Served /v1/history and default /v1/chart VWAP read only ONE stored market orientation, silently dropping all reverse-orientation liquidity and any bucket that traded only the other way
- **locations:** internal/storage/timescale/aggregates.go:136-143 (HistoryPoints), internal/storage/timescale/aggregates.go:200-216 (HistoryPointsInRange), internal/api/v1/chart.go:109, internal/api/v1/history.go:623
- **failure:** A market whose SDEX offers are predominantly stored as USDC/XLM. A client requests the XLM/USDC line chart or /v1/history?base=XLM&quote=USDC. HistoryPointsInRange returns only the minority XLM/USDC-oriented buckets: the VWAP per returned bucket omits the reverse-side volume (understating/biasing the price), and every minute/hour that traded ONLY as USDC/XLM is entirely absent — the chart shows gaps or a flat/stale line while the asset was actively trading, and volume_usd is understated. The candlestick (OHLCSeries) and TWAP charts of the SAME pair, which do combine both directions, disagree w
- **minimal fix:** Change HistoryPoints and HistoryPointsInRange to read both orientations and invert+combine flipped rows, exactly as OHLCSeries/TimedVWAPsForPair1m already do (WHERE ((base=$1 AND quote=$2) OR (base=$2 AND quote=$1)), CASE-invert vwap and volume_usd, GROUP BY bucket).
- **best fix:** Introduce one shared 'combine-both-orientations' SQL builder used by every prices_* read path so a new read cannot regress to single-orientation, and weight the directional combine by volume_usd rather than trade_count (see companion finding); add a test that inserts only flipped-orientation trades and asserts the history/chart endpoints return them.

### [HIGH/LIVE] MNY-06 — Multiple additional served/decision-driving VWAP reads combine the two market directions by TRADE COUNT instead of volume, yielding a non-VWAP price (same defect class as the confirmed MNY-06 at latestClosedVWAP1mTemplate)
- **locations:** internal/storage/timescale/aggregates.go:436-438 (recentClosedVWAP1mCombinedTemplate → RecentClosedVWAP1mCombined → pricingguard), internal/storage/timescale/aggregates.go:672-674 (closedVWAPAtOrBeforeQueryTemplate → /v1/price_at), internal/storage/timescale/aggregates.go:998-1000 (TimedVWAPsForPair1m → anomaly baseline), internal/storage/timescale/aggregates.go:1057-1059 (VWAPsForPair1m → baseline training window)
- **failure:** In one bucket, direction A has 500 dust trades at a manipulated price of 1.20 and direction B has 3 whale trades totalling 99% of the volume at 1.00. The true VWAP ~1.00; the trade-count-weighted result ~ (500*1.20 + 3*1.00)/503 ≈ 1.199. An attacker splits many tiny trades on one side to drag the served /v1/price_at value and the anomaly baseline. Because TimedVWAPsForPair1m feeds the anomaly-freeze baseline, this compounds the confirmed COR-14/MNY-22 manipulation surface: the detector is trained on a manipulable non-VWAP.
- **minimal fix:** Replace `* COALESCE(trade_count,0)` / `SUM(trade_count)` weights with a common-unit volume weight (e.g. `* COALESCE(volume_usd,0)` / `SUM(volume_usd)`, or volume converted to a shared unit) at all five sites and at latestClosedVWAP1mTemplate.
- **best fix:** Centralise the two-direction combine in one helper that volume-weights by volume_usd with a documented fallback when volume_usd is NULL, and add a unit test that fails when trade_count is used as the weight (asymmetric-volume bucket asserts volume-weighted, not count-weighted, result).

### [HIGH/GATED] MNY-22 — Stablecoin-proxy USD-volume gate silently ignores all crypto:USDT/USDC/DAI/PYUSD/USDP legs, wrongly dropping (or permanently blacking out) real high-volume windows
- **locations:** internal/aggregate/orchestrator/orchestrator.go:1146-1181 (fetchForTarget proxy branch), internal/aggregate/orchestrator/orchestrator.go:1213-1235 (usdVolumeForPairPerTrade), internal/aggregate/orchestrator/orchestrator.go:1268-1279 (usdQuoteDecimals), internal/aggregate/orchestrator/orchestrator.go:818-821 (survivorUSD -> dropForMinUSDVolume)
- **failure:** Operator enables enable_stablecoin_fiat_proxy with min_usd_volume=10000 (the shipped default). A native/fiat:USD minute has $3k of Kraken/Coinbase fiat:USD + classic-USDC trades and $500k of Binance native/crypto:USDT trades. survivorUSD is computed as $3k (USDT legs excluded), $3k < $10k, so dropForMinUSDVolume returns true and the whole window is dropped: /v1/price serves the prior (stale) bucket or falls through to a lower authority tier despite $503k of real volume. For an altcoin whose only market is crypto:USDT (no fiat:USD or classic-USDC leg) EVERY window computes survivorUSD=0 and is 
- **minimal fix:** In usdVolumeForPairPerTrade, when usdQuoteDecimals(src.Quote) returns ok=false but src.Quote is a proxiable stablecoin (aggregate.FiatProxy(src.Quote) succeeds to USD), value the leg at that source's per-trade scale (external.Lookup(trade.Source).AmountScaleDecimals()) treating 1 stablecoin unit = $1, so crypto:USDT/USDC legs count toward survivorUSD and volume_usd.
- **best fix:** Make USD valuation a per-trade, per-source computation (scale = external.AmountScaleDecimals(source); peg = FiatProxy OR classic/soroban peg OR fiat:USD) rather than a single per-pair-quote scale, and unit-test a proxy window whose volume is dominated by a crypto:USDT leg to prove it clears an equal min_usd_volume floor.


---

## Confirmed — chunk 2 (ingest/sources, 2026-07-24)

> PARTIAL (hit session limit mid-verify, 26/219 agents failed incl. all 9 convergence finders). First-pass confirmed set below.

| # | Sev | Exp | Dim | Finding |
|---|---|---|---|---|
| 1 | HIGH | LIVE | RFC-2 | Soroban events batch insert drops rows on failure without dead-letter queue |
| 2 | HIGH | LIVE | CON-10 | Sink shutdown-drain budget (90s) exceeds main's shutdown deadline (30s), so the undrained-ledger-range recovery log can never fire and buffered trades |
| 3 | HIGH | LIVE | COR-01 | Negative/invalid SEP-41 transfer or approve amount permanently wedges the sole-writer transfers projector (poison pill) |
| 4 | HIGH | LIVE | COR-11 | Deterministic store validation errors are classified transient, so any non-pq validation failure on a sole-writer domain retries forever |
| 5 | HIGH | LIVE | DAT-10 | claimable_balances + liquidity_pools observers never emit removals → served total/circulating supply permanently over-reported (and the doc's 'conserv |
| 6 | HIGH | LIVE | DAT-09 | TolerateTrailingMissing masks real mid-range archive holes as 'walk-complete' because the tolerance window is anchored to the chunk's `to`, not the li |
| 7 | HIGH | LIVE | REL-08 | On-chain trades are DROPPED (not block-retried) on disk-full / out-of-memory Postgres faults because IsInfraError omits SQLSTATE class-53 members 5310 |
| 8 | HIGH | LIVE | REL-08 | externalRetryBuffer.drainOnce permanently drops external trades that hit an INFRA fault during the data-fault per-row isolation pass, mislabelling rec |
| 9 | HIGH | LIVE | DAT-09 | blend_backstop genesis ledger set ~5.1M ledgers too late (56.6M vs real ~51.5M), blinding gap-detection, reconciliation, and completeness to a window  |
| 10 | HIGH | LIVE | INT-01 | Census counts SDEX trades that the decoder deterministically drops (stricter asset-code validation) → real trade loss + permanent reconcile false-inco |
| 11 | HIGH | LIVE | COR-11 | A Validate-failing OracleUpdate from a live oracle source permanently wedges the sole-writer oracle projector (poison pill via transient misclassifica |
| 12 | HIGH | LIVE | REL-08 | Non-trade served-tier events are dropped on any Postgres infrastructure fault in the dispatcher drain (HandleEvent error discarded, no retry/block/dea |
| 13 | HIGH | GATED | MNY-03 | projected-rebuild -write overwrites stored usd_volume with NULL (missing USD-volume resolver + high derive_generation) |
| 14 | MEDIUM | LIVE | DAT-04 | Pervasive outdated docstrings describe old DO NOTHING behavior while code implements new DO UPDATE with derive_generation guard |
| 15 | MEDIUM | LIVE | REL-02 | Soroban events batch write failures permanently lose events with no recovery |
| 16 | MEDIUM | LIVE | MNY-05 | Streamer dust filter is denominated in raw quote-asset units but assumes the quote leg is ~USD, so it silently drops legitimate BTC-quoted (XLM/BTC) t |
| 17 | MEDIUM | LIVE | REL-01 | WebSocket read loop has no application-level read deadline or ping keepalive; a stalled-but-open venue feed is only detected by OS TCP keepalive |
| 18 | MEDIUM | LIVE | DAT-04 | Stale ON CONFLICT DO NOTHING comments across per-source insert paths the projector writes through now actually DO UPDATE — a claim/code disagreement t |
| 19 | MEDIUM | LIVE | REL-02 | On-chain trades are dropped (not retried) on transient deadlock/serialization contention, contradicting the ADR-0041 'on-chain never dropped' invarian |
| 20 | MEDIUM | LIVE | COR-08 | Phoenix/Soroswap correlation buffers key only on (ledger,tx,op); a sub-complete multi-field emission within one op reuses the stale key and merges two |
| 21 | MEDIUM | LIVE | DAT-06 | Oracle op_index fanout keyed on OperationIndex (always 0 for Soroban) instead of EventIndex — two same-source oracle events in one tx silently overwri |
| 22 | MEDIUM | LIVE | DAT-09 | soroban_events AsyncSink silently discards the entire batch on any write error — permanent raw-landing-zone gap, contradicting its documented never-lo |
| 23 | MEDIUM | LIVE | DAT-09 | Fee-debit LedgerEntryChanges from failed transactions are never delivered to LedgerEntryChangeDecoders, so watched-account balance observations can be |
| 24 | MEDIUM | LIVE | DAT-13 | decoder_stats_5m silently undercounts across a process restart and can drop/merge buckets under ticker jitter |
| 25 | MEDIUM | LIVE | DAT-09 | SDEX keeps one-side-zero claim atoms 'for completeness' but they are unconditionally rejected downstream (DB CHECK + Trade.Validate), causing batch-in |
| 26 | MEDIUM | LIVE | DAT-09 | Archive→live seam can silently drop up to a full 64k-ledger partition: TolerateTrailingMissing swallows a genuine archive hole, then live resumes at a |
| 27 | MEDIUM | LIVE | REL-01 | Live handoff at a fixed `seam` hangs forever if the rolling live bucket has trimmed ledgers below `seam` |
| 28 | MEDIUM | LIVE | DAT-09 | Projector cursor advances past ledgers whose soroban_events rows are not yet durable (async-sink flush lag / drop), silently losing sole-writer sep41  |
| 29 | MEDIUM | LIVE | CON-10 | Pipeline shutdown-drain budget is structurally larger than — and uncoordinated with — the caller's 30s deadline: three stacked independent drainTimeou |
| 30 | MEDIUM | LIVE | MNY-22 | Active fiat-FX feed writes upstream rates into fx_quotes with no deviation/sanity band, so one bad upstream bar mis-scales every fiat-quoted usd_volum |
| 31 | MEDIUM | LIVE | REL-06 | WebSocket read loop has no idle/stall watchdog, so a connected-but-silent venue feed stops delivering trades indefinitely with no reconnect and no dis |
| 32 | MEDIUM | LIVE | INT-11 | IntraLedgerSeq interleaves per-tx fee changes with op changes, inverting the true fee-phase-before-apply-phase order → wrong final balance for watched |
| 33 | MEDIUM | LIVE | DAT-03 | Reflector op_index is computed over the unknown-symbol-COMPACTED vector, unlike band/redstone, so an allow-list change plus re-derive orphans/duplicat |
| 34 | MEDIUM | LIVE | COR-01 | Single-ledger bounded request for ledger < 2 returns success while delivering nothing (from-clamp vs loop bound) |
| 35 | MEDIUM | LIVE | REL-08 | Projector permanently drops (skips) a sink-rejected sole-writer sep41 event on a class-22/23 fault with no dead-letter capture — recoverable only via  |
| 36 | MEDIUM | LIVE | COR-11 | Reflector CEX/FX decoder can emit a self-priced fiat:USD OracleUpdate that fails Validate and wedges the sole-writer oracle projector (poison pill) |
| 37 | MEDIUM | LIVE | trap-15 | Oracle op_index fanout uses OperationIndex only — ignores EventIndex/CallPath — so multiple same-source oracle events or calls in one operation collid |
| 38 | MEDIUM | LIVE | REL-04 | Comment claims the enqueue-advance cursor is 'held behind un-landed writes, nothing dropped', but the cursor advances at enqueue while up to 256 chann |
| 39 | MEDIUM | LIVE | INT-11 | Event-path OpArgs enrichment attaches the operation's top-level InvokeContract args, so an event decoder whose contract is invoked as a nested sub-cal |
| 40 | MEDIUM | LIVE | INT-01 | Dispatcher skips failed-tx entry changes while the lake extract processes them, so live account-balance observations silently miss failed-tx fee debit |
| 41 | MEDIUM | GATED | MNY-06 | InvertScaled uses integer truncation (Div) rather than rounding, introducing a systematic downward bias on every inverted FX rate |
| 42 | MEDIUM | GATED | REL-05 | runPoller goroutines are bound to the parent ctx while teardown only cancels the derived streamer ctx, so a late poller-config error deadlocks Run at  |
| 43 | MEDIUM | GATED | MNY-22 | Coinbase and Bitstamp backfill stamp the entire candle's base volume at the close price (not VWAP), and Coinbase parses close from a JSON float64, bia |
| 44 | MEDIUM | GATED | COR-14 | Census ClassicTradeEffectCount overcounts real trades — it does not mirror the sdex decoder's asset-code / pair validation drops, so its documented "M |
| 45 | MEDIUM | GATED | MNY-13 | Backfill candle-trades and live-stream trades are never deduplicated against each other, so re-backfilling a live-covered window double-counts that vo |
| 46 | MEDIUM | GATED | REL-01 | Live phase blocks ingest indefinitely if LiveSeamLedger is below the live bucket's first ledger |
| 47 | MEDIUM | GATED | DAT-09 | Archive→live seam handoff silently skips up to 64k ledgers when the archive phase tolerates a trailing-missing file (production live indexer, no log,  |
| 48 | MEDIUM | GATED | INT-01 | Cold-tier fallback silently defeated when the cold bucket's galexie schema differs from hot's (schema loaded only from hot) |
| 49 | MEDIUM | GATED | MNY-22 | Chainlink feed decimals are taken from operator config and never cross-checked against the on-chain decimals() value, so a mis-set or upgraded feed em |
| 50 | MEDIUM | GATED | AGT-09 | Soroswap un-seeded-pair swaps are silently dropped at Matches() with no metric; the skippedUnknownPair counter that appears to record them is unreacha |
| 51 | MEDIUM | BRANCH | DAT-09 | CAP-0038 auto-liquidated claimable balances are unresolvable, so their later claim/clawback movements are permanently dropped from the account_movemen |
| 52 | MEDIUM | BRANCH | DAT-09 | LiquidityPoolWithdraw with a zero-rounding leg drops the ENTIRE withdraw movement (both legs), losing a real on-chain event |
| 53 | LOW | LIVE | COR-06 | Aquarius: 11 recognized event kinds are dropped by Matches while Decode's default branch decodes any unrouted-but-recognized event as a fabricated Tra |
| 54 | LOW | LIVE | COR-14 | Census SorobanEventCount can exceed the soroban_events row count because captureEligible omits the marshal-failure drops that contractEventToEventsEve |
| 55 | LOW | LIVE | AGT-08 | walkDataStore claims backend.Close() closes the underlying datastore; it does not — the store is never closed on the success path |
| 56 | LOW | LIVE | COR-01 | Single-ledger bounded Stream with TolerateTrailingMissing returns success while delivering ZERO ledgers when that ledger is missing |
| 57 | LOW | LIVE | INT-01 | BatchInsertTrades godoc claims 'ON CONFLICT DO NOTHING' idempotency but the executed SQL is 'ON CONFLICT DO UPDATE … usd_volume = EXCLUDED.usd_volume' |
| 58 | LOW | LIVE | REL-02 | drainBufferedEvents' final-pass counts projector-SKIPPED trades in undrained_events/undrained_trades and the re-derive ledger range because skipInSink |
| 59 | LOW | LIVE | CON-09 | Racy select in persistWorker can pick the flushTicker or normal `<-in` arm after ctx cancellation and flush an on-chain batch under the already-cancel |
| 60 | LOW | LIVE | COR-01 | Auth-tree walk omits the true top-level InvokeContract call and mislabels a nested call as top-level when the top-level requires no auth |
| 61 | LOW | LIVE | INT-05 | statsflush advances its `last` snapshot even when the Postgres write fails, permanently losing that window's decoder-stats deltas |
| 62 | LOW | LIVE | REL-05 | External trade enqueued to the retry buffer after the buffer's run() goroutine has already finalDrained at shutdown is silently retained and never per |
| 63 | LOW | LIVE | AGT-08 | Comet events.go package doc claims the decoder matches by topic bytes 'not at dispatch time', contradicting the load-bearing contract-identity gate th |
| 64 | INFO | LIVE | AGT-08 | IsNotFound cold-fallback string matching is dead for the S3/MinIO backend and its documenting comment inverts the SDK's actual behavior |

### Detail — chunk-2 CRITICAL + HIGH

**[HIGH/LIVE] RFC-2 — Soroban events batch insert drops rows on failure without dead-letter queue**
- loc: internal/sources/sorobanevents/dispatcher_adapter.go:254-270
- fix: On insert failure, preserve the batch in a dead-letter queue or retry buffer instead of clearing it. Return an error so the caller can decide whether to retry, drop, or halt.

**[HIGH/LIVE] CON-10 — Sink shutdown-drain budget (90s) exceeds main's shutdown deadline (30s), so the undrained-ledger-range recovery log can never fire and buffered trades are lost silently**
- loc: cmd/stellarindex-indexer/main.go:688, cmd/stellarindex-indexer/main.go:733-738, internal/pipeline/sink.go:566-575
- fix: Make main's shutdownCtx (main.go:688) >= the sink's drainTimeout (>= 90s), or derive drainTimeout from a shared constant that is strictly less than main's budget, so worker 0's deadline arm (and its ledger-range ERROR log) always fires before the process exits.

**[HIGH/LIVE] COR-01 — Negative/invalid SEP-41 transfer or approve amount permanently wedges the sole-writer transfers projector (poison pill)**
- loc: internal/sources/sep41_transfers/decode.go:64-96, internal/storage/timescale/sep41_transfers.go:77-79, internal/storage/timescale/errors.go:130-150
- fix: In decodeTransferAmount and decodeApprove reject a negative amount at decode time (return an error), matching sep41_supply/decode.go:61 — a decode error is a deterministic decodeFail that the projector skips (cursor advances) instead of holding.

**[HIGH/LIVE] COR-11 — Deterministic store validation errors are classified transient, so any non-pq validation failure on a sole-writer domain retries forever**
- loc: internal/storage/timescale/errors.go:130-150, internal/projector/projector.go:398-421
- fix: Introduce a recognizable permanent-validation error type/sentinel from the store's own validators and have IsPermanentDataError return true for it (skip + alert) so a deterministic fault cannot wedge the cursor.

**[HIGH/LIVE] DAT-10 — claimable_balances + liquidity_pools observers never emit removals → served total/circulating supply permanently over-reported (and the doc's 'conservative' justification is inverted)**
- loc: internal/sources/claimable_balances/dispatcher_adapter.go:96-112, internal/sources/claimable_balances/decode.go:22-32, internal/sources/claimable_balances/doc.go:14-33
- fix: Make Matches/Decode handle Removed for both observers: emit an Observation with Balance=0, IsRemoval=true. Because a Removed LedgerKey for a claimable balance carries only BalanceId (no asset) and an LP LedgerKey carries only PoolId (no asset pair), the writer must resolve the prior asset_key from t

**[HIGH/LIVE] DAT-09 — TolerateTrailingMissing masks real mid-range archive holes as 'walk-complete' because the tolerance window is anchored to the chunk's `to`, not the live tip — chunked parallel backfills report success while silently skipping up to 64k ledgers**
- loc: internal/ledgerstream/ledgerstream.go:259-306, internal/ops/opsutil/opsutil.go:211-226, internal/ops/ingest/backfill.go:392
- fix: Bound the tolerance to only the FINAL chunk whose `to` equals the current live tip: pass TolerateTrailingMissing=true only when the caller knows `to` is at/above the galexie tip, and false for every interior chunk. In chunked callers, set it only on the last chunk.

**[HIGH/LIVE] REL-08 — On-chain trades are DROPPED (not block-retried) on disk-full / out-of-memory Postgres faults because IsInfraError omits SQLSTATE class-53 members 53100/53200/53000; the cursor then advances, producing a silent served-tier gap — the exact class ADR-0041 was built to prevent**
- loc: internal/storage/timescale/errors.go:66-76, internal/pipeline/trade_sink.go:139-168, internal/pipeline/sink.go:862-896
- fix: Add 53000, 53100, 53200 to the recognised SQLSTATE switch in IsInfraError (errors.go:66-73) — or match on `pqErr.Code.Class()=="53"` — so insufficient-resource faults route to block-and-retry like 53300.

**[HIGH/LIVE] REL-08 — externalRetryBuffer.drainOnce permanently drops external trades that hit an INFRA fault during the data-fault per-row isolation pass, mislabelling recoverable infra failures as permanent 'dropped'**
- loc: internal/pipeline/trade_sink.go:296-332
- fix: In the drainOnce per-row loop, re-queue rows whose InsertTrade error IsInfraError/isCtxErr (requeueFrontLocked) and only count-and-drop rows that are positively permanent data faults (IsPermanentDataError).

**[HIGH/LIVE] DAT-09 — blend_backstop genesis ledger set ~5.1M ledgers too late (56.6M vs real ~51.5M), blinding gap-detection, reconciliation, and completeness to a window that contains real V1 backstop events**
- loc: internal/sources/blend_backstop/events.go:40, internal/storage/timescale/per_source_gaps.go:257, internal/ops/chops/reconciliation_catalogue.go:209
- fix: Change `BackstopGenesisLedger` in blend_backstop/events.go:40 to the true first-possible backstop ledger (~51_499_546, matching FactoryGenesisLedger / the earliest observed lake event) and update the literal at per_source_gaps.go:257 to match.

**[HIGH/LIVE] INT-01 — Census counts SDEX trades that the decoder deterministically drops (stricter asset-code validation) → real trade loss + permanent reconcile false-incomplete (treadmill)**
- loc: internal/dispatcher/census.go:167-219, internal/sdexclaim/sdexclaim.go:34-42, internal/sources/sdex/decode.go:181-192
- fix: Make the census counter and the decoder agree: have claimAtomCount/RealTradeCount count only atoms that would actually decode (validate the asset code the same way), OR relax validateClassicAssetCode to accept every protocol-valid asset code so no real trade is dropped.

**[HIGH/LIVE] COR-11 — A Validate-failing OracleUpdate from a live oracle source permanently wedges the sole-writer oracle projector (poison pill via transient misclassification)**
- loc: internal/sources/redstone/decode.go:73, internal/sources/redstone/decode.go:101-121, internal/sources/reflector/decode.go:96-121
- fix: In the source decoders, drop/normalise a non-G observer to empty before emitting (band already skips USD; add the same USD self-price skip to reflector/decode.go and coerce Observer to "" when scval.AsAddressStrkey yields a non-account strkey).

**[HIGH/LIVE] REL-08 — Non-trade served-tier events are dropped on any Postgres infrastructure fault in the dispatcher drain (HandleEvent error discarded, no retry/block/dead-letter) — the ADR-0041 resilience only covers trades**
- loc: internal/pipeline/sink.go:325-329, internal/pipeline/sink.go:292, internal/pipeline/sink.go:494
- fix: In the four dispatcher-drain call sites, stop discarding the HandleEvent error when it is infra-classified: wrap the non-trade HandleEvent call in retryInfra (block-and-retry, same as persistTrade) so an infra fault gates the drain instead of dropping the write; keep discard only for IsPermanentData

**[HIGH/GATED] MNY-03 — projected-rebuild -write overwrites stored usd_volume with NULL (missing USD-volume resolver + high derive_generation)**
- loc: internal/ops/chops/projected_rebuild.go:127-137, internal/storage/timescale/trades.go:606-631, internal/storage/timescale/trades.go:913-921
- fix: In projected_rebuild.go, after timescale.Open and before any -write, call timescale.InstallUSDVolumeResolution(store, cfg.Trades.USDPeggedClassicAssets, cfg.Supply.SACWrappers) exactly as cmd/stellarindex-indexer/main.go:196 and ch_rebuild.go:172 do.

