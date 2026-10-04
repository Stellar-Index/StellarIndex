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

## [v0.97.0] — 2026-09-30

36 commits since v0.96.0. Operator-visible: `GET /v1/operations` rejects any
`ledger` parameter with a 400 that names the new
`GET /v1/ledgers/{seq}/operations` route (OpenAPI 1.31.0); two new off-chain
feeds ship disabled — `[external.openexchangerates]` (`enabled = false`,
`OPENEXCHANGERATES_APP_ID`) and `[external.tiingo]` (`enabled = false`,
`TIINGO_API_KEY`), the latter valuing twelve WisdomTree fund shares at their
daily NAV once enabled; the forex worker polls every
`[external.massive] refresh_interval` (default 1h, floored at 10m); a new
`stellarindex-ops wasm-drift` check with two ticket alerts, for which the
repo ships no scheduling unit; `verify-lake`, `verify-contiguity` and
`verify-hashchain` with `-to 0` fail closed when the lake trails the history
archive tip by more than 100 ledgers; `compute-completeness` takes
`-timeout` (default 120m, `PASS_TIMEOUT` in the driver); `sla-probe` paces
at `-max-rps` (default 100); test nets stop serving pubnet reference
listings; the next archival-node apply renders WAL archiving and restarts
Postgres. After deploy, run `projector-replay -source phoenix -from
63295145` to add the Map-schema pool's liquidity rows (#2002). No new
migration.

### Breaking

- **api — `GET /v1/operations?ledger=` retired (#1992, #2007,
  OPERATOR-VISIBLE):** the one-ledger read is now
  `GET /v1/ledgers/{seq}/operations` (limit 1–2000, closed-ledger cache
  band); any `ledger` parameter on `/v1/operations` returns a 400
  `invalid-parameter` problem naming that route, so `/v1/operations` is the
  tip-advancing directory only (ADR-0018).

### Changed

- **ops — lake verifiers' auto `-to` (#1974, OPERATOR-VISIBLE):** with
  `-to 0`, `verify-lake`, `verify-contiguity` and `verify-hashchain` also
  read the tip from `stellar.history_archive_url` and fail closed when the
  lake trails it by more than 100 ledgers, so a Galexie stall no longer
  certifies the lake complete; an unreachable tip warns and is skipped.
- **ops — compute-completeness (#1879, OPERATOR-VISIBLE):** in `-pass` mode
  sources that re-verify from genesis run after the incremental ones, the
  recognition snapshot is written before the loop, a deadline stop names
  every skipped source, and the deadline is a `-timeout` flag (default
  120m) the driver passes through `PASS_TIMEOUT`.
- **sla-probe — request pacing (#1905, OPERATOR-VISIBLE):** `-max-rps`
  (default 100, 0 = unpaced) caps all workers through one limiter so a run
  no longer trips the per-key rate limit and pages on its own 429s; stats
  carry `failed_by_status` and the availability reason names the most
  frequent cause.
- **api — test-net listings (#1951, OPERATOR-VISIBLE):** on testnet and
  futurenet `/v1/sources` and its health route keep only sources that apply
  to the network, `/v1/assets/verified`, `/v1/external/assets` and
  `/v1/aggregators` return an empty list, `/v1/external/assets/{slug}`
  returns 404, and the pubnet warning stamps are skipped; pubnet and an
  unset network are unchanged.
- **ansible — archival-node WAL archiving (#1731, OPERATOR-VISIBLE):**
  `postgresql.conf.j2` renders `archive_mode`, `archive_command` and
  `archive_timeout`, gated on `pgbackrest_backup_enabled`, so a rebuilt host
  keeps point-in-time recovery; the next apply restarts Postgres with the
  values the reference host already runs.
- **ansible — pg-logrotate (#1897, OPERATOR-VISIBLE):** the hourly
  `pg-logrotate.service` reads a role-owned
  `/etc/stellarindex-pg-logrotate.conf` (stock policy plus `maxsize 500M`)
  instead of the uncapped distro file.
- **ci — deprecations (#1969):** `check-deprecations.sh` fails a
  `// Deprecated:` paragraph with no `vX.Y.Z` removal version in CI,
  `verify.sh` and `lint-changed`; the legacy tier constants are scheduled
  for v2.0.0.
- **release — build provenance (#1945):** `release.yml` attests every
  subject in `SHA256SUMS` (binaries and `migrations.tar.gz`) with
  `actions/attest-build-provenance` after signing, and a failed attestation
  stops the release; `release-process.md` documents
  `gh attestation verify`.

### Added

- **forex — Open Exchange Rates (#2011, OPERATOR-VISIBLE):**
  `[external.openexchangerates]` (`enabled` default false, `app_id` /
  `OPENEXCHANGERATES_APP_ID`) builds a provider for the hourly USD-base
  board, sending the app id only in the Authorization header and refusing a
  malformed board whole; the worker stores it but never fetches it, so
  serving stays massive then ECB. The worker cadence is
  `[external.massive] refresh_interval` (default 1h; under 10m is raised to
  10m and logged).
- **rwa — WisdomTree fund NAV (#2012, OPERATOR-VISIBLE):** a Tiingo poller
  in the indexer (`[external.tiingo]`, disabled by default,
  `TIINGO_API_KEY`, hourly) stores daily NAVs for twelve WisdomTree
  fund-share tokens bound on exact `(code, issuer)` as reference-only
  `raw:<TICKER>` rows; the new `fund_nav` provenance ranks between the
  oracle and listing arms with `decimals_published: 2` and no premium, a
  NAV older than 5 days is `reference_expired`, and oracle rows gain
  `nav_disagreement` past half a cent.
- **ops — wasm-drift (#2004, #2013, OPERATOR-VISIBLE):**
  `stellarindex-ops wasm-drift` resolves the current WASM hash of every
  audited gated source's contracts from the lake and flags any hash not in
  `internal/ops/chops/audited_wasm.json`, writing `wasm_drift.prom`;
  `stellarindex_wasm_drift` and `stellarindex_wasm_drift_stale` (no run in
  2 days) are ticket alerts with a runbook. sushiswap_v3 and upshift gain
  audit logs and manifest hashes.
- **web — legal pages (#2010):** `/terms` and `/privacy` are live, linked
  from the sidebar rail, footer and sitemap; the sign-up form states that
  creating an account accepts them, and `/pricing` describes usage as
  per-account.

### Fixed

- **forex — held rates after restart (#1995):** on its first refresh the
  worker seeds held tickers from the newest `fx_quotes` row within 7 days,
  so a fiat the upstream has not yet republished no longer drops out of
  `/v1/price` after a restart.
- **openapi — envelope declared (#1972):** the 38 enveloped 2xx data
  schemas declare `EnvelopeMeta` (`as_of`, `flags`) as `allOf`; the data
  subtrees are unchanged, and the 18 session-cookie dashboard/auth
  operations are named as the bare-on-the-wire exemption.
- **phoenix (#2002, #2001):** the Map-schema pool's `provide_liquidity` and
  `withdraw_liquidity` events decode into `phoenix_liquidity` rows (replay
  above); a bond-instrument contract that only shares the `"bond"` topic
  word leaves the curated stake set.
- **oracle — raw rows (#1925):** the `oracle_prices_1d` read behind the
  RWA history series drops `raw:` assets in both its keys and its SQL.
- **deploy — cut-over DDL (#1961):** the evidence step no longer refutes a
  ClickHouse DDL whose created objects are declared transient
  (`si-cutover-object`); it leaves it to the operator's acknowledgement.
- **markets (#1950):** empty commit; the SAC-spelling fold in
  `/v1/markets` and `/v1/pools` shipped in v0.96.0 as #1966.
- **docs:** the frozen-price flag docs state what a held value carries,
  including its own `observed_at` and `/v1/price/batch` (#1847); the launch
  plan's D1 freeze passage matches the measured state (#2009); the HA plan
  gains a ClickHouse lake tier (#1767); the design system records the
  Tailwind v4 browser baseline (#1930); the host-down runbook records the
  Hetzner Robot server numbers (#2008); the DNS/email perimeter's
  owner-side steps are closed (#2014); the metrics reference cross-links
  the monthly-quota fail-closed alert (#2020); the coverage doc cites the
  commit behind a reused PR number instead of the number (#1901).

## [v0.96.0] — 2026-09-30

58 commits since v0.95.0. Operator-visible: SDEX fills with one zero leg are
now stored as trades and serve `"price": null` (the field is nullable in
OpenAPI and `*string` in `pkg/client`); catalogue listing cursors name the
slugs already served, so an offset cursor from an older release returns 400;
dashboard sessions idle for over 7 days are revoked; `POST /v1/account/keys`
is limited to `apikey`/`operator` callers and self-service keys expire when
idle; `verify-lake` censuses all seven raw tables on a new daily timer; a
page inhibits only the ticket/info alerts of its own `alert_family`; and the
unread `idx_lec_asset` skip index leaves the ClickHouse schema (a live host
reports it as drift until the operator drops it). New migration: 0191 drops
the two `trades` amount CHECKs — catalog-only DDL that runs with compressed
chunks in place (no decompress) under the deploy's 5 s `lock_timeout`; its
commit carries a `Replay-Plan:` trailer (SDEX step 3f re-derives the served
trades with `ch-rebuild -sdex`).

### Breaking

- **pkg/client — `TradeRow.Price` (#1658):** now `*string`; it is nil for a
  zero-leg SDEX fill, which has no price.

### Changed

- **sdex — zero-leg fills, migration 0191 (#1658, OPERATOR-VISIBLE):** 0191
  drops `trades_base_amount_check` and `trades_quote_amount_check`, and its
  down refuses while compressed chunks or zero-leg rows exist;
  `Trade.Validate` rejects a negative or both-zero fill and admits one zero
  leg (`stellarindex_trades_zero_leg_admitted_total{source}`); price readers,
  the outlier filter and the sdex reconciliation count read only priceable
  rows; migration lint refuses a new division by a trade leg without a
  priceable guard.
- **api — catalogue pagination (#1904, OPERATOR-VISIBLE):**
  `/v1/external/assets`, `/v1/assets?asset_class=` and the unified
  `/v1/assets` catalogue phase page on the served slugs, so a re-rank between
  reads no longer repeats or skips a row; offset cursors return 400.
- **dashboardauth — idle sessions (#1967, OPERATOR-VISIBLE):** a session
  unused for `SessionIdleTimeout` (default 7 days) is revoked; API-key auth
  is unchanged.
- **auth — self-service API keys (#1849, OPERATOR-VISIBLE):**
  `POST /v1/account/keys` returns 403 unless the caller's tier is `apikey`
  or `operator`, and a self-service child key is stored with
  `MirroredKeyIdleTTL`, refreshed on every use, so only an abandoned key
  ages out; operator, admin and signup mints stay persistent.
- **ops — verify-lake (#1980, OPERATOR-VISIBLE):** a raw-table census checks
  transactions, operations, contract_events, results and participants per
  1M-ledger partition, writes `lake_verify.prom`, and runs daily under
  `run-heavy-job` with stale/failed alerts and a runbook.
- **alerting — page→ticket inhibition (#1973, OPERATOR-VISIBLE):** the 34
  family alerts across 11 r1 rule files carry an `alert_family` label and
  both Alertmanager configs inhibit a ticket/info alert only when it shares
  the page's `component` *and* `alert_family`, so one page no longer mutes
  every lower-severity alert on that component; `inhibit-rules-test.sh`
  runs in CI, `verify.sh` and `lint-changed`.
- **clickhouse — `idx_lec_asset` (#1990, OPERATOR-VISIBLE):** the bloom
  index on `ledger_entry_changes.asset` had no reader and leaves the tier-1
  schema and the retrofit script; a host that still carries it shows
  live-only drift in `ch-schema-drift` until step 4 of
  `tier1_skip_indexes.sql` (`DROP INDEX` under `run-heavy-job.sh`) runs.
- **divergence — CoinGecko reference (#1712):** the divergence price
  reference authenticates with `external.coingecko.api_key` /
  `demo_api_key` (Pro key → `pro-api` host) instead of hitting the public
  host anonymously; a failed batch logs status and path only.
- **ops — projector-replay (#1891):** the command's help, its run note and
  the replay decision rule state its generation limit: it writes at
  `derive_generation` 0, so it cannot correct a row a re-derive already
  stamped higher — use `projected-rebuild -write` after a decoder fix;
  `backfill-router`'s comments match what it does.
- **ci — package docs (#1769):** `lint-docs` fails on an `internal/` or
  `pkg/` package with no package comment; the Definition of Done asks for a
  `CAPABILITY-INVENTORY.md` check before new utility code.

### Added

- **clickhouse — live_daemon identity (#1939):** the indexer, aggregator and
  API read `STELLARINDEX_CLICKHOUSE_LIVE_USER`/`_PASSWORD` before falling back
  to `default`; the archival-node role provisions the flag-gated user.
- **api — `flags.pivot_unverified` (#1935):** set on `/v1/price` when a
  composite's pivot leg is made only of stablecoin-proxy prints; served
  prices are unchanged.
- **explorer — market chart (#1943, #1983):** the OHLC series downloads as
  CSV or JSON exactly as served, and an opt-in 1h/4h/24h trailing high/low
  band overlays the candles.
- **monitoring (#1918, #1751):** `stellarindex_sep41_supply_freshness_absent`
  fires when the SEP-41 supply freshness series vanishes, and
  `stellarindex_monthly_quota_fail_closed` pages on a sustained fail-closed
  quota gate.

### Fixed

- **api:** `/v1/markets` and `/v1/pools` fold SAC spellings into one row per
  pair (#1966); a dead domain's held SEP-1 payload stops serving as verified
  (#1875); `/v1/chart` walks read only the missing buckets and stop stamping
  `flags.stale` on healthy windows (#1975); SAC code history answers from the
  instance index instead of timing out (#1977); a computed 0%
  `completeness_pct` is emitted (#1944); `/v1/assets` resolves issuer home
  domains in one batched read and counts LCM fallbacks (#1940); SSE
  subscriptions past the topic-map ceiling get a 503 (#1942).
- **auth:** the API-key index is marked ready only by the build generation
  that walked it, and the API invalidates the index at start so records
  written raw become listable and revocable (#1857).
- **api:** `/v1/ledger/stream` connections share one cursors read per tick
  instead of each polling the store (#1915).
- **pricing / divergence:** the synthetic USD cross requires legs from
  independent publishers (#1929).
- **clickhouse / chops:** compute-completeness floors its scans at the
  network's Soroban genesis instead of the pubnet ledger, so the test-net
  unit no longer scans an inverted range (#1706); the cap67 watermark
  refuses to advance over missing contract events (#1931); hash-chain rows pair their hashes from one row
  (#1944); gated factory children preseed from the ClickHouse lake and an
  empty seed is an error (#1876); the chunk-restamp report counts clean
  chunks and config-assertions flags a paused `trades` compression policy
  (#1873).
- **deploy / build:** the config-apply gate baselines on the region's
  manifest set (#1959); r1 rule applies stage in a per-run `mktemp` dir
  (#1663); Go builder images are pinned by digest (#1949).
- **chaos:** scenario 04 asserts the limiter's MISCONF policy (#1922).
- **docs:** READMEs, runbooks and plans corrected to match the code at HEAD —
  sorocredit and defindex surfaces (#1984, #1981), the automated stale-deploy
  check (#1976), the monthly archive-trim timer (#1934), the withdrawn
  `drop_chunks` drill item (#1965), the phantom Aquarius router gap (#1971),
  and the divergence webhook payload (#1908); the WASM audit logs record the
  Soroswap factory `set_pair_wasm` rotation (#1991), the sorocredit
  early-window walk (#1997) and the Phoenix WASM lineage captured from the
  lake, including a 14th pool the registry did not know (#1996).
