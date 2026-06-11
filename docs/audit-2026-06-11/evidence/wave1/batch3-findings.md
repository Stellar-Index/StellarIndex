# Wave 1 batch 3 — condensed findings

Slices: G9 (48/48), G10 (98/98), G16 (56/56), G17 (55/55), G18 (50/50), G19 (54/54), G20 (8/8). All files read.

## G9 src-observers
- G9-01 CRITICAL data-loss: projector-registered sep41 decoders can NEVER match — synthetic watched-contract defeats Matches; in Phase-4 skip-projected mode NOBODY writes sep41_supply_events/sep41_transfers (registry.go:136-160, dispatcher_adapter.go:41-49, sink.go:179). [= G16-01, mutually confirming] conf=high
- G9-02 HIGH data-loss: sep41_transfers.Decode never populates EventIndex — PK collapses same-op events (event_index always 0) (dispatcher_adapter.go:61, 0047:38). conf=high
- G9-03 HIGH data-loss: sep41_supply_events PK no event discriminator — same-op mints/burns dropped, Algorithm-3 supply corrupted (0015:59). [= G23-01] conf=high
- G9-04 MED correctness: sep41_supply mint/clawback counterparty topic position contradicts repo's own CAP-67 note for post-P23 unified shape (topic[2]=asset string → AsAddressStrkey fails → decode error → events dropped); pinned test encodes contradicting assumption (decode.go:75-97 vs cap-67-unified-events.md:104). conf=med
- G9-05 MED error: forex worker no retry/backoff — one transient failure stales /v1/currencies 1h; names-fetch failure discards fetched rates; zero tests (worker.go:102). conf=high
- G9-06 LOW: EOF detection by string-contains accepts truncated bodies (forex+frankfurter, duplicated loops) (client.go:252). conf=high
- G9-07 LOW: isFiniteFloat f!=f+1 wrong for |f|≥2^53 (cache.go:128). conf=high
- G9-08 LOW DRY: assetKeyFromAsset triplicated with unchecked Issuer.Ed25519 deref (panic on malformed XDR); trustlines nil-guard missing (claimable/decode.go:62, liquidity_pools:135, trustlines:83). conf=high
- G9-09 INFO stale: circulation.csv path wrong; registry cites deleted sep41_transfers_backfill.go; dead removal param; isSEP41BalanceKey perf comment inverted. conf=high
- G9-10 INFO seam: forex/frankfurter outside internal/sources/external/ duplicating Polygon backend (same vendor 2 clients). conf=med

## G10 src-external
- G10-01 HIGH correctness: chainlink dedup drops uint80 phase bits — proxy phase upgrade silently wedges feed until restart (likely THE known chainlink-wedge root cause) (decode.go:42, events.go:198). conf=high
- G10-02 HIGH liveness: chainlink PollOnce swallows all-feeds-failed as "skipped" — staleness gauge bumped, dead firstErr var; dead poller looks healthy (poller.go:150, runner.go:182). conf=high
- G10-03 HIGH concurrency: F-0029 reconnect fix (backoff reset+keepalive+disconnect metric) applied to binance/bitstamp NOT kraken/coinbase — backoff pins at 60s forever after ~6 disconnects (kraken/streamer.go:87, coinbase/streamer.go:74). conf=high
- G10-04 MED security: API keys in URL query leak into logs via url.Error on transport failures (coingecko x_cg_pro_api_key, exchangeratesapi access_key, polygon apiKey, chainlink Alchemy in endpoint) (poller.go:246 etc.). conf=high
- G10-05 MED correctness: X2.5 FX-snap structurally dead — FX sources write oracle_updates but FXQuoteAtOrBefore queries trades; nothing converts (registry.go:148, trades.go:826). conf=med — check r1 counts
- G10-06 MED test-theater: chainlink TestAnswerUpdatedTopic0 never compares vs reference hash (want looks fabricated, only logged); broken keccak would pass (poller_test.go:31). conf=high
- G10-07 MED concurrency: WS streamers no read-idle deadline — half-open connection stalls ingest indefinitely (all 4 streamers). conf=med
- G10-08 LOW: synthetic tx_hash truncates at 32 input bytes — coingecko hash constant per pair for ~11.6-day windows (poller.go:487). conf=high
- G10-09 LOW ADR-0003: coinbase backfill decodes candles as float64 (>2^53 volume loss); kraken uses json.Number deliberately (backfill.go:129). conf=med
- G10-10 LOW stale-docs: README "shared 10^8 scale" wrong for FX pollers (Decimals=6 OracleUpdates) (README.md:98). conf=high
- G10-11 LOW: Run() leaks started streamer goroutines when later Start fails (runner.go:71). conf=high
- G10-12 INFO: chainlink client no response size cap (io.ReadAll) unlike all other connectors (client.go:179). conf=high
- G10-13 INFO: registry presence-guard test list stale — missing chainlink/router/defindex/cctp/rozo (framework_test.go:10). conf=high

## G16 derived-verify
- G16-01 HIGH data-loss: [= G9-01] projector sep41 sources never match; reconciliation catalogue EXCLUDES sep41 so ADR-0033 can't catch it; recognition passes via dispatcher's real decoders — triple blind spot; masked only by persist_per_source default true (registry.go:135, reconciliation_catalogue.go:62). conf=high
- G16-02 MED correctness: projector cursor advances past sink-write failures; SinkFunc doc promises retry-on-next-cycle that doesn't exist (projector.go:80,330). conf=high
- G16-03 MED correctness: divergence cache key collapses quote dimension — div:<base> overwritten per quote pair; last quote wins divergence_warning for the asset (worker.go:257, keys.go:218). conf=high
- G16-04 MED correctness: supply.CrossCheck compares Alg-2 FULL classic total vs SEP-41 wrapped-only total with 1-stroop tolerance — incommensurable; fires always or never wired (crosscheck.go:63). conf=med
- G16-05 MED dead-code: supply.Overlay+MetadataResolver (SEP-1 max_supply, ADR-0011 step 2) dead — no caller; unit hazard if wired (display-units vs stroops) (overlay.go:10). conf=high
- G16-06 LOW stale-docs: archivecompleteness doc.go claims primary-archive check + "all shipped"; only cross-anchor implemented (doc.go:29). conf=high
- G16-07 LOW: projector BatchLimit doc says row cap, code is ledger-span clamp; sep41_supply source has no Topic0Syms prefilter → firehose scan on catch-up (projector.go:63, registry.go:153). conf=high
- G16-08 LOW: PopulateFromFillResult comment vs code — RepairAttempts never counts failures (metrics.go:226). conf=high
- G16-09 LOW: projector lastSeenLedger dead; resolveTip not-found → misleading lag=0 idle forever (projector.go:269,373). conf=high
- G16-10 LOW: supply freshness-query failures swallowed `_ = err` with comment promising WARN that doesn't exist — silently disables F-1236 gate or flips to permanent rejection (storage_classic_reader.go:120). conf=high
- G16-11 LOW perf: CoinGecko divergence batch no negative caching — 429/outage → serial refetch under mutex per pair per tick (coingecko.go:280). conf=high
- G16-12 INFO: alignLastCheckpoint tautological branch `if rem >= 63 || rem < 63` + unreachable return (cross_anchor.go:149). conf=high
- G16-13 INFO: RefreshPair doc claims error on all-fail; actually caches zeroed result CLEARING a firing warning (fail-open) (worker.go:224). conf=high
- G16-14 INFO: supply textfile gauges hardcode XLM naming + 10^7 scaling for all assets (textfile.go:56). conf=med
- G16-15 INFO: absFloat comment teaches false Go cost model (math already imported in package) (worker.go:333). conf=high

## G17 account-security
- G17-01 MED security: legacy API-key records and PG read-through cache rows share apikey:<sha256> namespace — SCAN mutators (UpdateRateLimit/MarkEmailVerified) can strip cache-row TTL (revocation backstop defeated) + silent overwrite on rebuild; ListKeys double-counts (apikey_postgres.go:261, store_update.go:47). conf=high
- G17-02 LOW security: SEP-10 replay guard keys raw signed-XDR bytes — signature reorder mints fresh dedupe slot (validator.go:44,273). Fix: key on network tx hash. conf=high
- G17-03 LOW security: customerwebhook isInternalIP misses 192.0.0.0/24, 198.18.0.0/15, TEST-NETs, 240.0.0.0/4 (ssrf.go:68); mirror copy in dashboardwebhooks must stay identical. conf=high
- G17-04 LOW: webhook worker drains customer response body without size cap (worker.go:252; notify/resend.go does it right). conf=high
- G17-05 LOW: signup IP throttle missing sub-second-window divide-by-zero guard ratelimit.New has (signup_ip_throttle.go:107). conf=high
- G17-06 LOW concurrency: SCAN+read-modify-write key mutations not atomic — concurrent mutators lose updates (store_update.go:72). conf=high
- G17-07 LOW: webhook Worker ErrAlreadyRunning dead; Stop-before-Run deadlocks; double-Run panic only incidental (worker.go:139,336). conf=high
- G17-08 LOW security: magic-link template renders caller UserAgent/IP verbatim — phishing-content injection into legit email (templates.go:115). conf=med
- G17-09 LOW: incidents parseFile silently drops unparseable resolved_at — resolved incident renders ongoing (incidents.go:149). conf=high
- G17-10 INFO stale-docs: ratelimit doc.go omits F-0050 dwell-time fail-closed; 4 other stale comments. conf=high
- G17-11 MED missing-tests: SSRF guard ZERO direct coverage (every worker test bypasses the dialer); retry-exhaustion + backoff-cap unpinned (ssrf.go:27, worker_test.go:99). conf=high
- G17-12 INFO DRY: import-keeper idioms; dead ErrFanoutNotConfigured. conf=high
- VERIFIED-GOOD: key entropy/hashing, JWT pinning, Lua atomic ratelimit + documented fail-open→closed, SETNX replay, GETDEL tokens, HMAC webhooks + redirects disabled.

## G18 types
- G18-01 MED concurrency: discovery AsyncSink.Start doc "idempotent" but double-Start → two workers + panic on Stop (sink.go:94,169). conf=high
- G18-02 MED security: SEP-1 SSRF guard bypassed when HTTP(S)_PROXY set — DialContext validates proxy IP not target; comment claims opposite (sep1.go:124,375). conf=high
- G18-03 LOW security: marketcap refresher CG key in URL query → leaks via url.Error into logs (refresher.go:231). [pattern = G10-04] conf=high
- G18-04 LOW: FanoutOpIndex silently masks/wraps out-of-range inputs — PK-collision primitive unguarded (trade.go:93). conf=high
- G18-05 LOW: OracleUpdate.Validate accepts NaN confidence (oracle.go:139). conf=high
- G18-06 LOW: verified-currency loader never CRC-validates issuer strkeys nor parses asset_id — typo in trust surface loads silently (verified.go:216). conf=high
- G18-07 LOW: normaliseNumeric drops TOML float max_number values; test pins the false premise "TOML lib doesn't emit floats here" (sep1.go:352, helpers_test.go:21). conf=med
- G18-08 INFO stale-docs: 7 stale comments (3-vs-6 asset shapes, deleted <source>-backfill refs, nonexistent strict param, retired alias, wrong timeout). conf=high
- G18-09 INFO missing-tests: internal/currency/marketcap zero test files (backoff/Retry-After/applyResponse). conf=high
- G18-10 INFO: metadata.Currency Decimals vs DisplayDecimals semantics undocumented at source — feeds the G3-02 misuse. conf=high
- G18-11 INFO: ParseAsset prefix dispatch shadows classic code "fiat"/"crypto" colon-alias; SEP-1 Resolve accepts issuer-controlled host:port. conf=med

## G19 platform
- G19-01 HIGH config/security: `default:` tags drift from Default() — SignupRequireEmailVerification doc=true actual=false; CookieSecure doc=true actual=false; TLSCertProbeHosts doc=3-hosts actual=empty (F-0051 probe disabled); DivergenceMinIntervalSeconds doc=300 actual=0 (F-0030 quota fix defeated) (config.go:598-671, load.go:33). conf=high
- G19-02 HIGH config: SupplyConfig.Validate() never called by Config.Validate(); aggregator_refresh_enabled=true w/o cadence → time.NewTicker(0) PANIC at runtime; buildSupplyRefreshers docstring claims it validates (validate.go:55, aggregator main.go:738). conf=high
- G19-03 MED config: ApplyEnvOverrides writes secret VALUE into S3AccessKeyEnv/S3SecretKeyEnv fields consumed as env-var NAMES; test pins wrong behavior; latent break on LoadWithEnv migration (load.go:75, trim_galexie_archive.go:302). conf=high
- G19-04 MED config: redis_password_env documented as env-var NAME but consumed as literal password (config.go:413, redisclient.go:49). conf=high
- G19-05 LOW: APIKeyStore.Revoke violates idempotence contract — second revoke overwrites audit fields (apikey_store.go:376). conf=high
- G19-06 LOW: quota-cap zero semantics differ webhook (defaults 10) vs apikey (disables) (webhook_store.go:65 vs apikey_store.go:159). conf=high
- G19-07 LOW security: parseCIDRArray silently drops malformed allowlist entries — possible fail-open on empty-after-drop (apikey_store.go:118). conf=low
- G19-08 INFO stale-docs: status-class label comment, RecordAttemptFailed name, missing store_test.go ref. conf=high
- G19-09 INFO: Load() doc claims env overrides that only LoadWithEnv applies; trim/rehydrate use bare Load (load.go:12). conf=high

## G20 cmd-daemons
- G20-01 HIGH durability: indexer advances cursor + writes ledger_ingest_log "fully done" BEFORE async sink persists events (256-buffer + 8×200 batches) — crash loses ~2k trades for ledgers marked complete; comment claims AFTER (indexer main.go:1194-1211, sink.go:121). conf=high
- G20-02 MED concurrency: close(events) without joining ledgerstream producer at SIGTERM — send-on-closed-channel panic swallowed by decoder-panic recover as bogus "dispatcher panicked" (main.go:526-551). conf=high
- G20-03 MED shutdown: error-return paths close stores BEFORE cancel (LIFO defers) — workers run against closed pools; drain sequence happy-path only (all 3 binaries) (indexer:143, aggregator:149). conf=high
- G20-04 MED: API -dry-run launches external fetchers + heavy SQL before the gate (forex/prewarm/marketcap/coverage at :601-736 vs gate :922); indexer/aggregator gate first. conf=high
- G20-05 LOW: API binary joins NONE of ~10 background goroutines at shutdown (no WaitGroup). conf=high
- G20-06 LOW: CH dual-sink ExtractLedger errors silently swallowed (no else/log/counter); enable-log prints raw possibly-empty addr; 9300 fallback duplicated (main.go:515,391). conf=high
- G20-07 LOW dead: aggregator "anomaly enabled but no Redis" branch unreachable (main.go:171,281). conf=high
- G20-08 LOW: four godoc blocks detached from functions by spliced insertions (warnOpenCORS security rationale invisible) (aggregator:970, api:1116,2485). conf=high
- G20-09 LOW DRY: buildDivergenceReferences + USD-peg parser duplicated across binaries, manual lockstep, no contract test. conf=high
- G20-10 LOW: change-24h peg fallback swallows real SQL errors as "unavailable" (api main.go:2366). conf=high
- G20-11 INFO suspicion: SIGTERM stops soroban_events sink early while in-flight ledger can still get "fully done" substrate row — deploy-restart edge (main.go:342,1203). conf=low
- G20-12 INFO missing-tests: zero shutdown-sequencing tests; TestRecordCursorMetric asserts nothing. conf=high
