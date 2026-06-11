# Wave 1 batch 1 — condensed findings (full agent reports in session transcript)

Slices: G6 (43/43), G7 (36/36), G8 (43/43), G13 (11/11), G14 (39/39), G15 (45/45), G23 (113/113). All files read.

## Wave-0 mechanical gates
- W0-01 medium deps: govulncheck: GO-2026-5039 (net/textproto), GO-2026-5037 (crypto/x509) — fixed in go1.25.11; GO-2026-5026 (x/net idna) — fixed in x/net v0.55.0. Upgrade toolchain + x/net.
- W0-02 medium docs-drift: docs/reference/api/rates-engine.v1.yaml is 54 lines behind openapi/ source (make docs-all dirties it) — spec edited without regen.
- W0-03 low tooling: govulncheck not installed locally despite Makefile pin (make tools gap or doc).
- W0-04 info security: secrets grep over tracked files clean.

## G6 src-dex-a (sdex/sorobanevents/soroswap/router/aquarius)
- G6-01 HIGH correctness: soroswap swap↔sync groupKey omits pair ContractID → cross-pair mis-attribution + silent swap overwrite (decode.go:33, consumer.go:116; fixture test uses the *correct* key shape). conf=med
- G6-02 HIGH security: topic-only Matches in aquarius+soroswap factory path — any contract can inject trades/poison pair registry; comment cites non-existent allow-list; MainnetFactory const defined but unused (aquarius/dispatcher_adapter.go:24, soroswap/decode.go:72). conf=med
- G6-03 MED data-loss: sorobanevents AsyncSink drops failed INSERT batch (≤1000 rows) with WARN only, no retry/counter, contradicting "never silently lost" (dispatcher_adapter.go:254-261). conf=high
- G6-04 MED correctness: soroswap SkimEvent hardcodes EventIndex=0 though migration 0043 PK relies on event_index (dispatcher_adapter.go:174). conf=med
- G6-05 LOW: router DeadlineTs deadline=0 → epoch not NULL (decode.go:150 + sink.go:453 IsZero guard never fires). conf=med
- G6-06 LOW dead/stale: ErrOrphanSync dead; stellar-rpc-era comments; recordNewPair references non-existent method (events.go:89, decode.go:13). conf=high
- G6-07 LOW missing-test: no multi-hop same-op fan-out test for soroswap router path (decode.go:141). conf=high

## G7 src-dex-b (blend/comet/phoenix/defindex)
- G7-01 HIGH data-loss: blend_auctions PK (ledger,tx,op,ts) — no event_index/auction_type/user; multiple same-op auction events silently dropped; 0053/0054 fixed siblings but skipped auctions (0009:93, blend/decode.go:85-126). conf=high
- G7-02 MED data-loss: comet_liquidity, phoenix_liquidity, phoenix_stake_events PKs lack event-index discriminator — same-op same-kind rows dropped (0042:129, 0044:90/159). conf=high
- G7-03 MED error-handling: defindex classified-but-undecoded topics (harvest + 9 vault gov) return ErrUnknownEvent → pollutes decode-error alerting instead of clean (nil,nil) drop (defindex/decode.go:236). conf=high
- G7-04 MED data-pollution: blend topic-only Matches claims generic symbols (set_admin/deploy/claim/supply) from ANY contract → SAC set_admin lands in blend_admin; blend steals set_admin from sep41_transfers (first-match order) (blend/dispatcher_adapter.go:45). conf=med
- G7-05 MED correctness: phoenix groupKey omits ContractID; assign() silently overwrites populated slot (phoenix/decode.go:99-107). conf=med
- G7-06 LOW: comet falls back to time.Now() on EventClosedAt error (mis-timestamps replay) unlike siblings (comet/dispatcher_adapter.go:48). conf=high
- G7-07 LOW stale-docs: 8 comment/README claims contradict code (blend Matches scope, defindex Phase-A vs BackfillSafe:true + shipped persist, phoenix multihop Q4, scval.AsBool). conf=high
- G7-08 LOW dead-code: defindex.Sink dead w/ stale rationale; blend classify()/comet classifySwap() test-only. conf=high
- G7-09 INFO process: phoenix BackfillSafe=true scoped to 8 swap fields only; liquidity/stake decoders replay unaudited WASM field sets (registry.go:36 vs README "Pending"). conf=high
- G7-10 INFO missing-test: no EventIndex propagation regression test for blend Decode (the 0053/0054 load-bearing fix). conf=high

## G8 src-oracles (band/reflector/redstone/cctp/rozo)
- G8-01 MED correctness: oracle ts u64→int64 conversions clamp only low end — far-future/overflow passes through (band decode.go:105, redstone:315, reflector:88); same class as fixed router deadline_ts (batch-drop risk). conf=high
- G8-02 MED: reflector OracleUpdate.Observer never populated in prod — README Q4 relayer-compromise detection doesn't exist (no caller passes WithDecoderObserver). conf=high
- G8-03 MED correctness: reflector DEX quote hardcoded XLM vs discovery doc "probably USD, TBC" — open item never settled (decode.go:130 vs discovery/oracles/reflector.md:161). conf=LOW — needs on-chain base() check
- G8-04 LOW dead/stale: redstone write_prices function-name guard documented but not implemented; ErrWrongFunctionCall dead (events.go:62,101). conf=high
- G8-05 LOW stale-docs: reflector README teaches disproven body shape its own events.go refutes; sec-vs-ms comment (README:31, events.go:113). conf=high
- G8-06 LOW stale-docs: redstone README "u64 seconds" (is ms); band README cites non-existent PublishedAt field. conf=high
- G8-07 INFO dead-code: cctp/rozo ErrUnknownEvent never returned; redstone test-indirection vars unused; write_timestamp decoded then discarded. conf=high
- G8-08 LOW coverage: CctpForwarder in contract set but no forwarder topic classified — possible partial-event decoder (cctp/dispatcher_adapter.go:37). conf=LOW
- G8-09 INFO: band all-USD/all-zero relay → ErrEmptyRates contradicts README "expected-no-op"; zero-rate skips unmetered. conf=med
- G8-10 INFO consistency: reflector/redstone substitute time.Now() on EventClosedAt error (wrong-data path); cctp/rozo fail closed. conf=med

## G13 agg-orchestrator
- G13-01 HIGH math: VWAP over windows >10k trades computed on OLDEST 10k only (ORDER BY ts ASC LIMIT) — biased stale 1h/24h VWAP, no truncation detection; cap-rationale comment self-contradictory (orchestrator.go:464,704; trades.go:645). conf=high
- G13-02 MED math: crypto-stablecoin backer trades contribute zero to MinUSDVolume gate + SourceUSDVolume under proxy expansion (usdVolumeForPairPerTrade misses crypto:USDT/USDC) (orchestrator.go:1041). conf=high
- G13-03 MED freeze-contract: frozen LKG VWAP expires out of Redis mid-freeze (freeze skips Set; TTL=window) — ActionFreeze "serve LKG" only honored for freezes shorter than window (orchestrator.go:760-780). conf=high
- G13-04 MED stale-doc+perf: package doc claims per-(pair,window) goroutine parallelism; Tick is sequential (races if "restored"); 24h window rescanned per 30s tick (orchestrator.go:41,582). conf=high
- G13-05 LOW stale-doc: prevVWAPs "Reset to nil on ActionFreeze" contradicts code+tests (orchestrator.go:479). conf=high
- G13-06 LOW: divergence min-interval stamps before attempt regardless of outcome (failed pass suppresses retry up to 10min); Redis outage labeled no_vwap (divergence_refresh.go:53,74). conf=high
- G13-07 LOW config-validation: triangulation Target overlapping cfg.Pairs silently overwrites direct VWAP each tick + stale provenance marker on chain removal (triangulate.go:137). conf=med
- G13-08 LOW observability: triangulation outcome labels misclassify FX-store DB errors as redis_error, compute failures as parse_error. conf=high
- G13-09 INFO dead-code: windowUSDVolume/usdVolumeForPair dead; canonicalTrade alias justified by non-existent import cycle. conf=high
- G13-10 LOW math: approxUSDVolume divides by 1e7 claiming stablecoin convention is 7 (external uniform is 1e8) — confidence LiquidityFactor 10× off for CEX, mixed-scale sums (confidence.go:215). conf=high
- G13-11 INFO: staleness keyed by base asset only — fresh write on any quote masks others (orchestrator.go:824). conf=med
- G13-12 INFO: staleness_emit_test comment documents live r1 symptom the passing test disproves — unresolved discrepancy breadcrumb. conf=low

## G14 agg-rest
- G14-01 MED math: computeAcceleration inverts for negative deltas — steady downtrend labeled "increasing" (changesummary/rollup.go:391; zero test coverage). conf=high
- G14-02 MED math: ATL corrupted by `|| atlValue == 0` reset when zero value mid-series ([100,5,0,90]→ATL=90) (rollup.go:252). conf=high
- G14-03 HIGH data-loss: supply stale-component gate permanently rejects dormant assets (MinComponentLedger frozen, gap grows forever — live PHO 1017→1324); alerts aggregate across assets so never fires; only escape is manual per-asset override (supply/refresher.go:227, rules/supply-refresh.yml:41). conf=high
- G14-04 MED invariant: ComputeGlobalPrice tier-1 doesn't loop assetAliases — rc.89 XLM dual-form bug recurs on /v1/assets/{slug} global view (global.go:189, main.go:1988). conf=med — check r1 pair-set form
- G14-05 MED staleness: global-price tier-1 VWAP has no freshness bound — weeks-old bucket beats fresh tier-2 (global.go:183 vs MaxAggregatorAge tier-2 only). conf=med
- G14-06 LOW stale-comments: averageAggregatorPrices claims rejection but truncates; deltaPct claims NULL but stamps 0% (global.go:296, rollup.go:285). conf=high
- G14-07 LOW dead-config: confidence weights "[anomaly.weights] operator-tunable" never wired; no negative-weight validation (confidence/score.go:83). conf=high
- G14-08 LOW dead-code: freeze_test.go nonsense var block "platforms where this file is only partially read". conf=high
- G14-09 INFO math-precondition: VWAP/TWAP/OHLC assume uniform amount-scale; CEX 1e8 vs SDEX 1e7 mixing biases volume weights ~10:1 — undocumented precondition (vwap.go:29). conf=low live / high doc-gap
- G14-10 INFO semantics: TWAP skipped-trade slots create time holes vs textbook hold-last-price — deliberate but undocumented (twap.go:54). conf=high

## G15 ingest-spine
- G15-01 HIGH concurrency: Dispatcher.Stats() (statsflush goroutine, 5-min) races ProcessLedger map writes — unguarded maps, fatal concurrent map read/write crash class (dispatcher.go:413,830; flusher.go:122; indexer main.go:273). conf=high
- G15-02 HIGH data-loss: persistWorker flushes trade buffers with CANCELED parent ctx on shutdown — up to 8×200 buffered trades lost every SIGTERM while cursor already advanced (sink.go:163,147); matches SDEX dual-sink drop class. conf=high
- G15-03 MED seam: TolerateTrailingMissing in archive phase lets StreamArchiveThenLive jump to seam, silently skipping undelivered ledgers below seam (seamed.go:50, datastore.go:60). conf=med
- G15-04 MED dead-code: hashdb has ZERO production callers — drift detector in package doc/CLAUDE.md/ADR-0016 is unwired (hashdb.go:14). conf=high
- G15-05 MED coverage: tx-level TransactionEvents (CAP-67 V4 fee events) dropped by BOTH dispatcher and census — mutually-confirming blindness; violates EVERY-event policy undocumented (dispatcher.go:516, census.go:88). conf=high
- G15-06 MED forward-compat: GetTransactionEvents errors silently swallowed both sites — future meta V5 zeroes Soroban ingestion with completeness still "complete" (dispatcher.go:516, census.go:88). conf=high
- G15-07 LOW observability: ledgerstream Config.Registry never set by any caller — tiered/SDK buffer metrics inert; naive wiring would panic on repeated MustRegister (tiered.go:81). conf=high
- G15-08 LOW: drainRemainder counts only trade-shaped events — non-trade shutdown loss reported as "no trade events undrained" (sink.go:340). conf=high
- G15-09 LOW dead-code: consumer.Orchestrator/Source fully dead (456 lines), shape contradicts invariant 6; only Event interface load-bearing (orchestrator.go:124). conf=high
- G15-10 LOW stale-comments: fee-change TxHash claim wrong; "4 workers" vs PersistWorkers=8; walkDataStore "closes store" claim wrong (SDK Close doesn't close datastore — store handle leaks per call); sink_test 30s-vs-90s drainTimeout. conf=high
- G15-11 INFO missing-tests: statsflush has no behavioral tests; flushAt references non-existent clock injection. conf=high
- G15-12 INFO: a375e0ad single-ledger fix correct; walkDataStore inherits SDK from<2 clamp/PrepareRange mismatch (parity, latent doc-vs-behavior gap). conf=med

## G23 migrations
- G23-01 HIGH PK-grain: sep41_supply_events PK lacks event_kind/event_index — same-op mints/burns collapse, corrupting Algorithm-3 supply sums (0015:59); sibling 0047 sep41_transfers got event_index, supply didn't. conf=high
- G23-02 HIGH PK-grain: blend_auctions skipped by 0053 fix — PK lacks pool/event_kind/event_index (0009:93). [= G7-01] conf=high
- G23-03 MED PK-grain: sdex_offer_events PK lacks offer_id/kind — N crossed offers per op collapse; latent if writer unshipped (0026:127). conf=med
- G23-04 MED PK-grain: phoenix_liquidity/stake PKs no per-event discriminator (0044:90,159). [= G7-02] conf=med
- G23-05 LOW PK-grain: comet_liquidity/cctp_events/rozo_events rely on partial discriminators (event_kind/token/event_type) not event_index (0042:129, 0038:83, 0039:62). [≈ G7-02] conf=med
- G23-06 MED DR-bootstrap: migration-defined chunk_time_interval still 1 day on trades/soroban_events/oracle_updates/blend_* — fresh bootstrap recreates 3445-chunk incident; 0049 comment admits drift (0001:64, 0041:149). conf=high
- G23-07 LOW ADR-0015: no CAGG pins materialized_only — closed-bucket serving version-dependent across TimescaleDB versions (0002:35, 0034:38, 0036:38). conf=med
- G23-08 MED docs: migrations/README rows 0016–0024 describe nine files that DON'T EXIST (fabricated names); rows 0046–0056 missing (README.md:103+). conf=high
- G23-09 INFO: 0005 header says 1-day chunks, DDL says 7 days. conf=high
- G23-10 LOW suspicion: trades PK vs multi-ClaimAtom SDEX ops — decoder must synthesize op_index per atom; verify sdex/decode.go and document (0001:45). conf=low
