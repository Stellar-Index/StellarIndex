# Changelog

All notable changes to Stellar Index will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to SemVer (`vX.Y.Z`) — for binary releases
as well as the `pkg/*` compatibility promise. See
[docs/architecture/semver-policy.md](docs/architecture/semver-policy.md)
for the rationale.

Every release lists the Stellar protocol version it was tested
against.

Only the five most recent releases are kept here; older sections
live at their tags (`git show v0.89.0:CHANGELOG.md`) and on GitHub
Releases. `[Unreleased]` is written at the release cut from commit
subjects, not per PR — see CONTRIBUTING.md §Changelog.

---

## [Unreleased]

## [v0.102.0] — 2026-10-04

2 commits since v0.101.0. No migrations, no `pkg/*` break, no API change.

### Fixed

- **completeness:** blend's projection read is scoped to its factories plus
  its registered and deploy-announced pools, through the gated prefilter
  (#2322).

### Documentation

- **sla:** weekly SLA proof for 2026-10-04 (not proven).

## [v0.101.0] — 2026-10-04

5 commits since v0.100.0. No migrations, no `pkg/*` break, no API change.

### Added

- **chops:** lake state is verified against the network's hot archive (#2201).
- **ops:** upstream re-export of the Galexie mirror is detected (#2198).

### Fixed

- **entrywalk:** entry changes are walked in canonical ledger-key order (#2202).
- **completeness:** comet's projection read is scoped to its pool set and each
  source's budget is bounded (#2320).

### Performance

- **explorer:** account/recent readers are windowed to keep read-in-order (#2191).

## [v0.100.0] — 2026-10-04

16 commits since v0.99.0. No migrations, no `pkg/*` break. One API retirement
under Changed (`POST /v1/signup`).

### Added

- **api:** USD anchor served over a single-venue fiat book (#2186); asset-scoped
  pool reserves, supply flows and order book (#2190); opt-in per-source
  breakdown on `/v1/vwap` (#2313).
- **history:** `source` filter on `/v1/history` and the explorer overlay (#2166).
- **auth:** email-less passkey signup (#2162).

### Changed

- **api:** `/v1/history` and `/v1/observations` serve on-chain trades only
  (#2312). `POST /v1/signup` is retired and answers 410 Gone, closing an
  email-existence oracle (#2310).
- **monitoring:** substance-refused trades are labelled "thin" and excluded
  from the USD coverage ratio (#2317).

### Fixed

- **api:** the remaining degraded exits are marked no-store (#2318).
- **chops:** a frozen ledgerstream cursor is refused as the verdict tip (#2153).
- **timescale:** re-derive writes fail on a USD-volume resolver error (#2159).
- **rwa:** hand-vetted fund-NAV bindings are admitted to RWA membership (#2314).
- **ops:** the per-query memory cap no longer kills the lake backup (#2315);
  `apply-rules` no longer fails on rule groups slower than the verify window
  (#2311); `external_poller_stale` stops firing hourly on Tiingo (#2316);
  `no_log` on the MinIO env template task (#2309).

## [v0.99.0] — 2026-10-03

150 commits since v0.98.0. Seven new migrations, 0199–0205. No breaking API
change is declared; behaviour changes are listed under Changed. One migration
carries a REQUIRED follow-up (0203). Deploy notes:

- 0203 `sushiswap_v3_position_events` (REQUIRED-FOLLOWUP): decompress the
  trades chunks, then `stellarindex-ops projector-replay -source
  sushiswap_v3 -from 61487379`. The deploy gate refuses to run without
  `followups_acknowledged=true`.
- 0204 seals customer-webhook signing keys at rest. Configure the API seal
  key before the binary swap; existing raw keys are sealed at API startup.
  Rolling the binary back after rows are sealed leaves them unreadable to the
  old worker (deliveries fail terminally as `no_secret`), so roll back only
  together with a restore of the raw keys.
- 0202 `sushiswap_v3_pools`, 0199 (entry-walk version on balance
  observations), 0200 (webhook previous secret), 0201 (completeness
  projection evidence) and 0205 (5 s `lock_timeout` on the trades compression
  job) are additive.
- Patroni/etcd now run over mutual TLS by default (#2155): supply the PEM
  variables before applying the role. galexie is pinned to v29.0.0 for
  Protocol 29 (#2112). The Galexie archive off-site mirror is retired and torn
  down where disabled (#2222).

### Changed

- **api — price (#2194, #2144, #2214, #2179, #2101, #2099, OPERATOR-VISIBLE):**
  vetted classic assets carry `global_market` and, when no Stellar price
  survives the substance gate, price from it (`price_basis=global_market`),
  with `stellar_divergence_pct` and `depeg_warning`; `/v1/assets/{id}` adds
  `issuer_behaviour`. `include_thin` is opt-in on the headline price surfaces.
  A declared USD peg is no longer crossed through an FX fixing, triangulated
  USD prices flag proxy deviation, and a rate needs market substance before it
  values a trade.
- **api — supply (#2209, #2128, #2196, OPERATOR-VISIBLE):**
  `/v1/assets/{id}/supply` answers 404 `supply-incomplete` instead of a zero
  total when there are no flows and the contract-storage fallback declines;
  `max_supply_basis` carries SEP-1 provenance; contract-storage balances are
  judged against their TTL.
- **api — behaviour (#2160, #2163, #2208, #2147, #2299, #2261, OPERATOR-VISIBLE):**
  degraded 200s carry `Cache-Control: no-store`; `/v1/chart?timeframe=all`
  is charged by granularity; row-cap truncation is flagged per source read on
  chart paths; anonymous signup keys expire when idle and free their email;
  price-withheld 404s carry a machine-readable reason; observation stream
  ticks read past the SWR history cache.
- **webhooks (#2136, #2148, OPERATOR-VISIBLE):** `POST
  /v1/dashboard/webhooks/{id}/rotate-secret` rotates in place with a 24 h
  overlap (previous key signs only the `-Previous` signature headers); signing
  keys are sealed with AES-256-GCM at rest.
- **ops — deploy and recovery (#2149, #2203, #2199, #2157, #2267, #2268):**
  migrations that blank data name their rebuild in a `REQUIRED-FOLLOWUP`
  header, enforced by lint and a deploy gate; new range-sharded
  re-derive-from-ledger driver; protocol-upgrade golden-ledger drill; restore
  drills run under the heavy-job wrapper and record evidence on any exit.
- **clickhouse (#2097, #2161, #2181, #2292, #2282):** ZSTD(3) on the three
  large XDR columns, and recompress refuses LZ4; tx-index miss authority
  needs a coverage marker; the contiguous-watermark gap scan is bounded; sink
  open refuses when operator-scope columns are missing.
- **pricealerts and alerts (#2094, #2131, #2126, #2122, #2116, #2213, #2241,
  #2301, #2125):** alerts fire once per crossing; a fire claim is refused when
  the rule changed mid-sweep; new alerts for persistent cache refresh failure,
  SDEX order-book reload failure, unavailable composite corroboration and ZFS
  pool predicted fill; divergence alerts read a live gauge.
- **ops — platform (#2096, #2129, #2119, #2290, #2295, #2286, #2296, #2293,
  #2184):** ZFS Postgres auto-snapshots kept 3 days, not 7; `RuntimeMaxSec`
  dropped from oneshot units; lake backup units fail when unconfigured; test-net
  edge redacts logs and sets security headers; Loki pin and loopback bind
  asserted; MinIO root credentials guarded against shell metacharacters; Tiingo
  fund-NAV poller enabled when the key is present.

### Added

- **sushiswap_v3 (#2169, #2180):** position events projected into their own
  table; pool token identities persisted.
- **assets (#2189, #2188, #2197, #2120):** SEP-1 currency standing and backing
  served; daily per-asset holders snapshot; trust score and factor breakdown on
  asset detail; ISINs declared by more than one issuer surfaced.
- **chops and movements (#2195, #2138):** account- and asset-keyed
  entry-change history; CAP-67 mint, burn and clawback derived and served by
  range.
- **api (#2146, #2102, #2113):** rendered `/v1/assets` listing page cached;
  independent cold reads run concurrently on two slow routes; explorer keyset
  cursors bounded on the leading key.

### Fixed

- **sources:** Phoenix and Soroswap drain open groups at range end (#2154);
  Upshift and Phoenix events counted by `source_recognition_failing` (#2224);
  Coinbase drops rejected products and resubscribes (#2206); Binance and
  Bitstamp skip dust trades without a decode error (#2239); Frankfurter rejects
  non-USD-base ranges (#2210); exact ECB parse and strict EOF (#2124); empty
  Reflector/Band batch is a no-op (#2047); external pollers keep staleness
  honest during throttle cooldown (#2215); sorocredit statement amount flagged
  unit-unconfirmed (#2254).
- **projector and dispatcher:** cursor held below a carried output's ledger
  (#2253); cycle budget escalates for a floor-stalled source (#2164); ledger
  upgrade entries and unreadable evicted-key lists are counted and logged
  (#2204, #2123).
- **supply and completeness:** genesis baseline auto-seeded for newly watched
  SAC wrappers (#2205); projection claim records when it was last proven
  (#2137); reconcile floored at genesis (#2134); `/v1/coverage` flagged stale
  when the ledgerstream cursor stalls (#2121); priceless candidates enumerated
  on both trade legs (#2221).
- **timescale and forex:** XLM-leg volume valued with a robust XLM/USD median
  (#2223); classic first/last ledger kept across batches (#2156); entry-walk
  version stamped on balance observations (#2135); forex history refresh
  decided from the newest bar across tickers (#2234, #2242).
- **freeze and divergence:** VWAP reseeded from the last published value
  (#2165); escalation history kept when a ladder is retired (#2216);
  divergence warning latch released when the hook fails (#2229).
- **explorer and web:** PEG badge limited to catalogue-vouched stablecoins
  (#2274); drawer no longer stacks two search modals (#2278); raw polls gated on
  tab visibility (#2277); failed sign-out surfaced (#2151); volume character and
  holder gaps shown in trust facts (#2192); OG cache keyed on path only
  (#2270); P24 fee-pool credit excluded from daily fee burn (#2127).
- **other:** oracle price bounded at 10^15 quote units per base (#2089);
  stablecoin SACs ranked and scam tags keyed on contra (#2140); classic pool
  placement in the DEX TVL headline settled (#2106); streaming flags a
  foreign-clock resume cursor with `stream_gap` (#2150); price staleness
  labelled by quote (#2147); config example ships a same-origin CORS default
  (#2262); asset-character roll delayed and its timeout raised (#2185).

### Internal

- Tests: firing tests for 16 page alerts (#2298), Blend position field order
  (#2251), tradeFromEvent persist guard (#2256), gap-detector decision for
  observation tables (#2257), explorer home-domain link safety (#2264),
  dust-floor quote check (#2218); CI: branch-protection status is a gate
  (#2088), Prometheus rule validation timeout 20m (#2152), prepush fixture
  stops spawning git maintenance (#2107).
- Docs and decisions: ADR-0049 accepted (#2090); licensing, RWA basis, price
  R1, W8-13, privacy rights runbook, SLA-proof sections, runbook and godoc
  corrections (#2093, #2098, #2100, #2104, #2103, #2110, #2170, #2176, and
  others); pre-migration `#N` citations repointed to commit shas (#2139).

## [v0.98.0] — 2026-10-02

67 commits since v0.97.0. Operator-visible: `/v1/divergence` is regrouped by
pair and `/v1/divergence/series` rejects `reference=` (OpenAPI change, see
Breaking); single-provider `source=` selectors for data vendors return 400
`off-chain-source-filter`, while exchange venues stay selectable; closed fiat
crosses on `/v1/price`, batch and SEP-40 convert at a stored vendor FX fixing
and carry `fx_rate` / `fx_as_of` / `fx_source` / `fx_resolution`; `/v1/status`
freshness counts are nullable beside a new `freshness_status`; `/v1/admin`
routes enforce operator tier and `X-Reason` at the mount; `/v1/price/stream`
emits `price_withheld`; the CoinGecko Pro, Tiingo, OpenExchangeRates and
ExchangeRatesAPI keys render onto r1 from vault variables; the ZFS ARC cap is
codified in the archival-node role; the archive trimmer is gated on
`galexie_archive_trim_enabled`; compression of `trades` chunks now waits 15
days (about 50 GB more uncompressed on r1). Six new migrations, 0192–0197,
all additive; none carries a REQUIRED follow-up, but four need a follow-up
run after deploy:

- 0192 `defindex_admin_events`: `stellarindex-ops projector-replay -source
  defindex` fills history.
- 0195 Phoenix stake-lifecycle, factory-config and blend events: `stellarindex-ops
  projector-replay -source phoenix -from 51572016 -refresh-caggs=false`
  (decompress the affected chunks first), then `compute-completeness`.
- 0196 `trades.tx_index`: `stellarindex-ops tag-tx-index` tags history; run
  it before the chunks pass `compress_after` (`-write` refuses compressed
  chunks).
- 0193 `fx_fixings` fills from `scripts/ops/fx-history-backfill` and the
  forex worker; closed fiat crosses withhold with `fx_leg_unavailable` until
  a fixing exists.

### Breaking

- **api — `/v1/divergence` and `/v1/divergence/series` (#2075,
  OPERATOR-VISIBLE):** `/v1/divergence` returns `data.pairs[]`, one entry per
  pair with our price once and every reference nested under it (the limit
  counts pairs, so a pair is never split across pages). `/v1/divergence/series`
  drops `reference=`, which now returns 400; each bucket carries our price plus
  a per-reference list. Neither route serves one provider's data alone.
- **api — off-chain `source=` selectors (#2075, #2076, OPERATOR-VISIBLE):**
  on `/v1/oracle/latest`, `/v1/observations`, `/v1/observations/stream` and
  `/v1/markets`, a data-vendor name (aggregators, FX providers, Tiingo,
  Chainlink, sovereign anchors) returns 400 `off-chain-source-filter` and an
  unknown name 400 `unknown-source`. Exchange venues (registry
  `Class=Exchange`, `Subclass=CEX`) and on-chain sources stay selectable.

### Changed

- **price — closed fiat crosses (#2025, OPERATOR-VISIBLE):** a closed fiat
  cross on `/v1/price`, batch and SEP-40 binds to the newest hourly vendor
  bar at bucket end minus 3h from the new `fx_fixings` table (migration
  0193), in exact rationals, so an answer no longer drifts with the live FX
  snapshot or differs by region. Responses carry `fx_rate`, `fx_as_of`,
  `fx_source`, `fx_resolution` and `usd_leg`; a missing fixing withholds with
  `fx_leg_unavailable` and a store error is a 503. `/v1/price/tip` keeps the
  live rate.
- **api — `/v1/status` freshness (#1917, OPERATOR-VISIBLE):** a failed
  freshness query is returned, not skipped; `active_sources` and
  `total_sources` are nullable and set only when measured, and
  `freshness_status` (`ok` / `degraded` / `unknown`) reports the block's trust
  state. An active-below-total shortfall reads `degraded` and does not move
  `overall`. OpenAPI and `pkg/client` follow.
- **api — `/v1/admin` mount (#1844, OPERATOR-VISIBLE):** all seven admin
  routes mount through `Server.handleAdmin`, which applies the operator tier
  and, for non-GET methods, `X-Reason` before the handler; a syntax-tree test
  fails any admin route registered another way.
- **api — `/v1/price/stream` (#1752, OPERATOR-VISIBLE):** a pair withheld
  mid-connection emits a `price_withheld` event keeping the bucket's id and
  `as_of` instead of dropping buckets silently; the publisher emits one when a
  pair becomes withheld or its reason changes and republishes once served.
- **api — source-filtered `/v1/markets` (#1831, OPERATOR-VISIBLE):**
  `?include=sparkline` and `?include=inception` are omitted under `?source=`,
  since neither reader has a per-source grain.
- **api — account trades (#2038, OPERATOR-VISIBLE):** `GET
  /v1/accounts/{g}/trades` admits one in-flight scan per caller; a second
  concurrent request is shed at once with the existing retryable 503, so one
  caller no longer holds all four slots. The global cap is unchanged.
- **api — asset ATH (#2033, OPERATOR-VISIBLE):** a day counts toward
  `/v1/assets/{id}` all-time high only if its pair cleared $100 volume and 3
  trades; a display filter, not a manipulation defence.
- **api — latest-trade reads (#2056, #2035):** the limit `LatestTradesForPair`
  binds is capped, and same-ledger ties break on a stable order.
- **supply — band breach (#1938, OPERATOR-VISIBLE):** the refresher keeps the
  last written `total_supply` per asset and counts a move over 10x in either
  direction in `stellarindex_supply_write_band_breach_total{asset_key,direction}`.
- **projector — dropped decode rows (#1697, OPERATOR-VISIBLE):** a row
  `sorobanevents.Reconstruct` rejects is logged (ledger, tx, index, contract,
  error; first and every 20th per cycle) and counted as `reconstruct_error`,
  with an alert for sparse sources the 0.1/s decode-error rate cannot catch.
- **aggregator — bootstrap cap (#1842):** a released pair is re-capped only
  below 27.0 days-equivalent of 1-minute density (`BootstrapReengageDensityDays`),
  so a dense pair no longer snaps back to 0.5 after about 30 hours of gaps.
- **indexer — shutdown loss (#1947):** after the sink stops, the cursor
  rewinds to the lowest abandoned trade's ledger minus one, so the next start
  re-walks it (served-tier writes are `ON CONFLICT` idempotent).
- **ratelimit — dwell clock (#2030):** the Redis-backed fail-closed gates
  clear their clock after a failure-free dwell, so one old failure no longer
  arms an unrelated blip.
- **notify — Resend (#1845):** 429 and 5xx responses retry with bounded
  backoff.
- **ops — ch-rebuild (#2042, #2039, #2049, #1887, #2036):** flags build
  through the shared write gate; projected-rebuild selects its source by
  name; upshift share transfers are re-derived; `-ch` reconciles seed
  factory-child gates from the lake; the lake decode path applies the
  execution-corroboration gate.
- **ops — tag-tx-index (#2072):** `-write` refuses compressed trades chunks
  (naming them) and checks again before each window, and the tagger verifies
  the lake row's ledger before trusting a `tx_hash_index` hit.
- **ops — recognition census (#1883):** `compute-completeness`,
  `verify-recognition` and `ch-recognition` register the SEP-41 decoders, so
  watched-contract transfers stop reading as unhandled topics.
- **timescale — usd_volume restamps (#2045, #2055, #2044, #2060):** a
  restamp aborts on an FX read error and skips a row another writer moved
  between plan and apply; an XLM quote leg anchors like an XLM base;
  maker-less AMM swaps key into the volume-character pair; same-issuer
  unpriced classic trades are labelled `usd_volume_populated="unroutable"`
  and leave the coverage alert's denominator.
- **storage — sep41 supply fold (#2069, #2053):** the fold reset re-folds
  each contract to the settled cursor in one transaction, naming any that
  fails, and `projected-rebuild` resets it.
- **ansible (#2034, #1728, #1958, #1860):** vendor keys render into
  `/etc/default/stellarindex` from `vault_*` variables with empty defaults;
  the ZFS ARC cap is codified; the archive trimmer file renders only when
  `galexie_archive_trim_enabled`; `galexie-archive-fill` runs as the archive
  writer, not root.
- **ci (#1921, #1744, #2078, #2083):** a `scripts/ci` self-test that no
  workflow or `verify.sh` runs fails CI; the go test job runs when a
  trigger-guard input changes; the main-CI health listing uses the unfiltered
  run list; the fleet-drift catch-up prints each region's full binary set.
- **monitoring (#1734):** `probe_stale` catches a frozen binary-version
  textfile.

### Added

- **defindex — admin events (#2021, OPERATOR-VISIBLE):** vault `rescue`,
  `paused`, `unpaused` and the fee-receiver and manager rotations persist to
  `defindex_admin_events` (migration 0192, no retention); historical fill
  above.
- **trades — `tx_index` (#2071, OPERATOR-VISIBLE):** migration 0196 adds a
  nullable `trades.tx_index` (catalog-only, even on compressed chunks); the
  indexer's `pipeline.RunTxIndexTagger` (trailing 30 minutes) and
  `stellarindex-ops tag-tx-index` fill it from the lake. No reader keys on it
  yet.
- **migrations:** 0192 `defindex_admin_events`; 0193 `fx_fixings`
  (append-only, keyed on vendor bar time, no retention); 0194 re-issues
  `sep41_supply_rollup`'s stored comments (metadata only); 0195 admits the
  Phoenix stake-lifecycle, factory-config and blend events and adds nullable
  `phoenix_admin_events.value`; 0196 `trades.tx_index`; 0197 moves the
  `trades` compression policy to 15 days (`alter_job`, no table lock). The
  deploy applies them; each takes a brief catalog lock.

### Fixed

- **phoenix (#1865):** stake-lifecycle, factory `Updated Config`, blend-pool
  settings and Map-body liquidity events decode, and the early stake WASMs'
  unbond reassembles, which had stopped completeness at ledger 53,329,393;
  a non-Phoenix contract leaves the curated stake set (replay above).
- **defindex (#1889):** genesis floors at the earliest routed factory,
  55,484,403, restoring 1.57M ledgers to every completeness check.
- **external (#1863, #2040, #2084, #2085):** backfills drop unsettled and
  unclosed candles; a 200 with zero rows is not a successful poll, and a
  poller with no applicable pairs reads idle, not fresh.
- **storage (#2080, migration 0197):** `trades` compression waits until a
  chunk is past the asset-character roll's 14-day window, so the roll no
  longer starves the compression policy of its lock.
- **timescale (#2031):** transit reads pick the latest bucket across both
  directions.
- **explorer (#1742):** the wealth price walk runs once under singleflight
  with a 60s fill timeout, and an empty result is cached for 30s.
- **api (#2041):** the RWA premium day reads bind pubnet genesis instead of
  an open-ended range; served output is unchanged.
- **deps (#2081):** `otel` bumps to v1.45.0 (GO-2026-6505).
- **docs:** the `supply-assets-stale` runbook covers the unseeded SAC wrapper
  (#2079); the ADR-0038 Phase B/C data jobs, the r1 follow-ups that closed,
  the redstone archived-WASM comparison, R-016's fix commit, the same-ledger
  OHLC tie-break, the FX as-of day-bucket rule, the SEP-41 base-anchor scope,
  the Kraken `/OHLC` depth limit and the SEP-40 method-name mirroring are
  recorded (#2018, #1874, #2052, #1953, #2051, #2029, #2057, #2046, #1776).

### Internal

- **refactor — FX snapshot (#1982):** the write-only float circulation and
  `History7d` fields leave `CurrencyEntry` and `CurrenciesSnapshot`; the
  struct is in-process, so no wire or OpenAPI shape changes.
- **test (#2065, #2070):** the since-inception limiter clock is pinned, and
  the sep41 rollup comment integration test pins version 193.
