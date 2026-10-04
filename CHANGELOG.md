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

## [v0.103.0] — 2026-10-04

7 commits since v0.102.0. No migrations, no `pkg/*` break, no API change.

### Fixed

- **lake:** persistent evictions are no longer recorded as removed (#2324).
- **explorer:** bare fiat `/assets` and `/protocols/sdex` forms 301 to their
  canonical paths (#2272); issuer SEP-1 icons are proxied through the
  same-origin `/icon` (#2280).
- **web:** sign-in, embed price and incidents fetches are bounded at 15s
  (#2275).

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
