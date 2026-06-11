# Wave 1 batch 2 — condensed findings

Slices: G1 (40/40), G2 (37/37), G3 (31/31), G4 (13/13), G5 (63/63), G11 (67/67), G12 (18/18). All files read.

## G1 api-core
- G1-01 MED security: XFF parser takes LEFTMOST (spoofable) entry once peer is trusted — breaks per-key IP allowlists + per-IP throttles under appending proxies; example.toml suggests the unsafe 10.0.0.0/8 shape (remoteip.go:87, keypolicy.go:77). Fix: rightmost-untrusted walk. conf=high
- G1-02 MED correctness: Auth middleware has no health/metrics exemption — strict auth modes 401 k8s probes + Prometheus (auth.go:86, server.go:866). conf=high
- G1-03 MED security: /metrics loopbackOnly guard ineffective behind Caddy (proxied requests arrive from 127.0.0.1); docstring overstates protection (server.go:944). conf=high
- G1-04 MED security: anonymous rate-limit bucket keyed on IP+User-Agent hash — UA rotation mints unlimited buckets, bypassing per-IP throttle (auth.go:262, ratelimit.go:227). conf=high
- G1-05 LOW: Bearer scheme match case-sensitive contra RFC 6750 (auth.go:250). conf=high
- G1-06 LOW: handleAccountMe 401s valid Subject with empty Tier — fragile inference (account.go:180). conf=low

## G2 api-pricing
- G2-01 HIGH correctness: /v1/observations #29 fast-path suppresses real CEX observations — premise "trades.quote_asset never stores fiat:*/crypto:*" is FALSE (binance XLMUSDT/XLMBTC/BTCEUR write crypto:/fiat: quotes); test pins wrong behavior (observations.go:111). conf=high
- G2-02 HIGH correctness: SEP-40 lastprice/x_last_price force stale=false on fallback chain — F-1254 regression analog on sibling surface (oracle_sep40.go:82,333). conf=high
- G2-03 MED invariant: rc.89 XLM alias loop missing from batch/tip/SEP-40/fallback paths — only handlePrice primary read has it (price.go:1040, price_tip.go:142, oracle_sep40.go:72). conf=high
- G2-04 MED perf: /v1/observations/stream lacks the 8s timeout AND short-circuit — unbounded LatestTradePerSource scan (>60s observed) per connection per tick; DoS vector (observations_stream.go:97). conf=high
- G2-05 MED correctness: OHLC Truncated computed after outlier filter — false negatives (ohlc.go:143 vs vwap.go pre-filter capture). conf=high
- G2-06 MED contract: price_type="peg" violates OpenAPI enum [vwap,twap,last_trade] (price.go:663, spec:4178). conf=high
- G2-07 LOW: peg-self $1 intends single_source=true per F-1232 comment but wires false (price.go:660). conf=high
- G2-08 MED concurrency: CachedOracleReader single-flight leader runs on caller ctx — leader disconnect poisons all waiters with 500s on SLA-probed path (oracle_cache.go:155); history_cache does it right. conf=med
- G2-09 MED contract: observations sources from map iteration — unsorted/non-deterministic, breaks byte-identical contract (observations.go:136). conf=high
- G2-10 LOW: OHLC 1w snap epoch-aligned (Thursday) vs doc+CAGG Monday (ohlc_series.go:261). conf=med
- G2-11 LOW DRY: tradesInRangeWithStablecoinFallback ≡ ohlcTradesWithStablecoinFallback byte-identical; peg-walk loop ×6 near-copies (vwap.go:196, ohlc.go:218). conf=high
- G2-12 LOW: TWAP + OHLC lack IsCacheUnavailable→503 branch siblings have (twap.go:66). conf=high
- G2-13 LOW: since-inception stablecoin fallback doesn't set triangulated flag, unlike every sibling (history.go:578). conf=high
- G2-14 LOW stale-comments: chart "trailing 1y" comments vs 25-year code; fiat path skips granularity validation; Truncated grace mis-sized (chart.go:251,524). conf=high
- G2-15 INFO: float64 in fiat cross-rate/market-cap strings (inherited float sources, boundary OK); parseSupply iterated division risk (price.go:757, chart.go:282). conf=high
- G2-16 LOW: batch fallback drops triangulated flag; observations stream skips unknown-source 400 guard (price.go:1050, observations_stream.go:78). conf=high

## G3 api-catalogue
- G3-01 HIGH pagination: coins-backed /v1/assets NEVER emits next cursor — overfetch-by-one comment without the overfetch (hasMore unreachable); only first ≤500 of ~440K assets listable (assets.go:483, coins.go:109). conf=high
- G3-02 HIGH integrity: SEP-1 display_decimals used as UNIT SCALE in market-cap math — verified-asset market_cap_usd/fdv inflated up to 10^5-10^7×; issuer-controllable manipulation vector (assets.go:1558, assets_f2.go:226). conf=high
- G3-03 MED pagination: unified listing limit=500 → store clamp resets 501→100, silent truncation + dropped cursor (assets.go:1002, coins.go:110). conf=high
- G3-04 MED resource: reader caches (markets/coins) unbounded on user-controlled keys — no LRU/TTL eviction, monotonic heap growth (markets_cache.go:86, coins_cache.go:78). conf=med
- G3-05 MED error: CachedSourcesStatsReader waiters get (nil,nil) when leader fetch fails (sources_stats_cache.go:63). conf=high
- G3-06 LOW: /v1/pools silently empty for unknown source vs /v1/markets 400 (documented anti-pattern); base/quote unvalidated (markets.go:181). conf=high
- G3-07 LOW: /v1/sources invalid-class message omits lending+router classes (sources.go:79). conf=high
- G3-08 LOW: coins overlay overwrites fresher F2 price_usd; ordering comments backwards; duplicate LatestPrice round-trips (assets_f2.go:205, assets.go:1214). conf=high
- G3-09 INFO: godoc references deleted findMatchingCurrency (assets.go:1565). conf=high
- G3-10 LOW: offset-cursor paths silently ignore unparseable cursors → restart page 1 (assets.go:624,803,960). conf=med
- G3-11 LOW: fanOutAssetMarkets drops per-asset errors silently when any rows merged (markets.go:652). conf=high
- G3-12 INFO perf: fiat market-cap fan-out ~19 FX queries per request uncached (assets_global.go:558). conf=high

## G4 api-diag-streams
- G4-01 MED error/SSE: streampublish poll loop suppresses DeadlineExceeded as "shutdown" while parent alive — chronic slow PG stalls /v1/price/stream silently, no failure counter (publisher.go:161). conf=high
- G4-02 MED stale-docs+dead: diagnostics_ingestion.go documents deleted cursor-first coverage model as authoritative (×3 blocks); dead rawCursors field stashed-never-read (diagnostics_ingestion.go:62,665,889). conf=high
- G4-03 LOW stale-docs: two comments claim 90-day trades retention (removed by 0031; invariant-8 drift seed) (diagnostics_ingestion.go:94,917). conf=high
- G4-04 LOW: Atom feed <updated> = time.Now() per request contradicting its own RFC-4287 comment (incidents.go:92,102). conf=high
- G4-05 MED missing-tests: zero tests for coverage/backfill aggregation helpers (multi-table MIN, name mapping, parseBackfillSub counts malformed cursor as COMPLETE) (diagnostics_ingestion.go:717-1148). conf=high
- G4-06 LOW DRY: /v1/network/stats lacks F-0094 error mapping + timeout that cursors got (network_stats.go:67). conf=high
- G4-07 INFO dead: methodology.go no-op blank var citing non-existent canonicalFiatUSD (methodology.go:212). conf=high

## G5 api-remainder
- G5-01 HIGH correctness: Stripe webhook dedup claims event row BEFORE processing — transient first-attempt failure → all retries dup-acked 200, paid customer NEVER upgraded; mark-processed guard comment stale (stripe_webhook.go:852,886, billing_store.go:55). conf=high
- G5-02 LOW stale-docs: asset_detail_cache claims AsOf splice + X-Ratesengine-Flags header that don't exist; dead flags field (asset_detail_cache.go:119). conf=high
- G5-03 LOW: signup ReserveEmail not released on mint failure — email locked 5min with 409 (signup.go:169). conf=high
- G5-04 LOW resource: dashboardauth touchTracker map never evicted (middleware.go:150). conf=med
- G5-05 LOW: magic-link 6-digit code emailed but no route consumes it (dashboardauth/auth.go:64). conf=med
- G5-06 LOW: HandleLogin success response missing Content-Type (handlers.go:237). conf=high
- G5-07 INFO DRY: writeProblem/writeJSON triplicated across dashboard packages (deliberate but drift-prone). conf=high

## G11 storage-timescale
- G11-01 HIGH perf/correctness: getCoinBySlugSQL + getNativeCoinSQL xlm_usd CTE missing the 24h floor the list query documents as REQUIRED (~40s scans observed; can serve days-stale XLM/USD as current) (coins.go:1183,1410 vs :306). conf=high
- G11-02 MED perf: FindSorobanEventsLedgerGaps full-hypertable DISTINCT scan — missing SorobanEventsTimeBound pruning; docstring cites non-existent index; same for per-source gap finder on soroban_events (soroban_events.go:312). conf=high
- G11-03 MED stale-docs: FIVE comments assert trades/prices_1m retention that 0031 removed — invariant-8 drift seeds (diagnostics.go:135,177,238, row_counts.go:11, aggregates.go:288). conf=high
- G11-04 LOW stale-docs: per_source_gaps trailer claims defindex+router "NOT registered" while both ARE (80 lines above); 5-min vs 13-min timeout comment (per_source_gaps.go:256,169). conf=high
- G11-05 MED resource: VWAPUSDFXResolver cache unbounded (asset×minute keys, negative results included, never evicted) on trade-insert hot path (usd_fx_resolver.go:57,222). conf=high
- G11-06 MED perf: fx resolver queryDB has no lower bucket bound — miss walks prices_1m to genesis though freshness rejects >1h (usd_fx_resolver.go:255). conf=med
- G11-07 LOW precision: NUMERIC/1e7 float-literal coercion in volume sums (sources_stats.go:65,143, markets.go:340) — ADR-0003 boundary. conf=high
- G11-08 LOW: InsertTrade registry hook swallows errors `_ = regErr` while comment claims log+skip (trades.go:345). conf=high
- G11-09 LOW perf-suspicion: LatestTradePerSource DISTINCT ON cost claim wrong — full index walk per pair; needs EXPLAIN on r1 (trades.go:556). conf=low
- G11-10 LOW perf: LedgerRangeToTimeRange ledger-only predicate scans all trades chunks; O(log N) claim wrong (diagnostics.go:95). conf=med
- G11-11 INFO: multi-row INSERT builders lack 65535-param guard (implicit caps OK today) (trades.go:382). conf=high
- G11-12 INFO stale-docs: package doc "0001-0015 applied" (now 0056); defindex godoc omits event_index from PK (doc.go:3). conf=high

## G12 storage-other (clickhouse, redisclient; NO minio adapter exists in this slice)
- G12-01 MED resource: LiveSink underlying Sink buffers unbounded during sustained CH outage (channel bounded 512, slices not) — monotonic heap growth on shared host (sink.go:182, live_sink.go:143). conf=high
- G12-02 MED observability: LiveSink written/dropped/errored counters never exported as metrics despite comment; written bumped on enqueue not durable flush (live_sink.go:181, indexer main.go:385). conf=high
- G12-03 MED lake-fidelity: stellar.ledger_entry_changes schema'd+flushed but NEVER populated — LedgerEntry-based observers (supply) have no lake substrate; ADR-0034 re-derive promise gap; stale "Phase-2 PoC" framing (extract.go:19, sink.go:325). conf=high
- G12-04 LOW: "max_execution_time: 0" annotated as memory bound (it's a disabled time limit) — unlimited exec time on the query class that wedged CH on 2026-06-11 (sink.go:161). conf=high
- G12-05 LOW security-hardening: sqlQuoteList no escaping; "compile-time constants" doc already outdated (caller-supplied contractIDs/topics inlined) (event_reader.go:38,101). conf=high
- G12-06 LOW stale-docs: "dispatcher emission order" claim wrong — ORDER BY tx_hash ≠ apply order within ledger (event_reader.go:50). conf=high
- G12-07 LOW concurrency: PushLedger racing Stop can strand an extract uncounted (live_sink.go:107). conf=med
- G12-08 INFO: ContractEventRow.InSuccessfulCall hardcoded 1 — assumed not verified (extract.go:271). conf=low
- G12-09 LOW missing-tests: no tests for load-bearing flush ordering (commit-marker), LiveSink drop/drain, extract event paths (sink.go:197). conf=high
- G12-10 INFO naming: openRead used for DDL + batch INSERTs; per-call dial for projector's per-tick watermark poll (gate.go:44). conf=high
