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

## [v0.106.0] — 2026-10-08

336 commits since v0.105.0. Five migrations (0208–0212), all additive. No `pkg/*` break; `pkg/client` gains `volume_lower_bound` fields.

### Migrations

- **0208:** `freeze_events.released_by` records who closed a freeze (#2391).
- **0209:** `oracle_updates.published_price` keeps the publisher's integer for inverted feeds (#2420).
- **0210:** `spectra_events` and `spectra_markets` hypertables (#2516).
- **0211:** `cagg_late_refresh_windows` persists the late-trade cagg refresh window (#2531).
- **0212:** `asset_volume_24h.unpriced_trades` (#2599).

### Added

- **spectra:** new source — factory-anchored decoder, tables, projector wiring and replay audit (#2516, #2520, #2537, #2544).
- **api:** `GET /v1/assets/{asset_id}/movements`, with CSV on `Accept: text/csv` (#2555, #2593); `GET /v1/ledgers/at` resolves a timestamp to its ledger (#2587, #2590); `/v1/operations` filters by operation type (#2580); `If-None-Match` answered with 304 via a body-hash ETag (#2528).
- **coverage:** USD volume pricing coverage published as its own axis (#2495).
- **indexer:** each hashdb sweep re-verifies a random slice of older history (#2452).
- **pipeline:** rozo promoted to projector sole writer (#2567).
- **ops:** disk-gated ordinal re-derive wrapper and `ch-backfill -changes-only` (#2511); non-forced `trades-cagg-refresh` (#2532); DNS cutover gated on the USD volume pricing bars (#2525); every bespoke writer behind the shared `-write` gate (#2551, #2597, #2661).
- **wasmaudit:** blend V1 factory, 40 phoenix and 22 aquarius WASMs audited for replay (#2719, #2721, #2732).
- **web:** thin-market prices carry a low-confidence badge (#2660).
- **alerts:** ticket when a node has needed a reboot for 7 days (#2581).

### Fixed

- **volume:** XLM legs and per-venue/Soroban volume valued at trade time, not today's price (#2498, #2512, #2527, #2540); listing and detail 24h volume flagged as a lower bound when trades went unpriced (#2599).
- **canonical:** the native XLM SAC and alias baseline derive from the installed network (#2428, #2432, #2585, #2586).
- **ingest:** trades caggs refreshed over trades written past a policy's lookback (#2492, #2531, #2571).
- **api:** `as_of` stamped with the cache fill time (#2534, #2570); idempotency replay never carries stored CORS headers (#2598); CORS wildcard+credentials rejected at config load (#2402).
- **external:** coinbase, kraken, binance and bitstamp candles parsed exactly or the page fails (#2562, #2566); kraken REST pair param (#2395).
- **ops:** backfills record projection dirty windows (#2411, #2449); backfill-router gated on WASM audits (#2450); chunk restamp lock budget charges waiting only (#2417); verify-archive tier D measures self coverage from `history/` (#2722).
- **ansible:** needrestart is list-only, so security upgrades no longer restart Postgres (#2579); TimescaleDB pinned to the test-harness version (#2487).
- **security:** unused anonymous registrations are erased (#2418).

### Changed

- 177 documentation commits strip history notes from comments and docs.

## [v0.105.0] — 2026-10-05

33 commits since v0.104.0. No migrations, no `pkg/*` break.

### Added

- **api:** `GET /v1/stablecoins` (#2351).
- **wasmaudit:** every Soroban replay is gated on per-WASM-hash audits (#2354).
- **rwa:** Centrifuge deRWA tokens deJTRSY and deJAAA bound by contract id (#2378).
- **ops:** `verify-archive` Tier C (`-tier sdf-sample`) cross-checks sampled ledgers against SDF's dataset (#2361).
- **ansible:** ClickHouse `ops_monitor`/`ops_admin` users and root-only client credential files (#2380).

### Fixed

- **deploy:** the asset-character roll is cancelled before migrate (#2385).
- **blend:** V1 pool `new_auction`, `fill_auction` and `bad_debt` decoded (#2356).
- **api:** carried-forward and partial 200s on status notices, RWA history and ingestion diagnostics are no-store (#2362); no cache refresh starts after the loop's context is cancelled (#2381).
- **sla-probe:** tip fallback responses held to the closed-bucket freshness bound (#2383).
- **monitoring:** `textfile_producer_stale` deferred for lock-gated producers while the heavy lock is held (#2359); tiingo gets the daily-feed freshness threshold (#2371).
- **ops:** lake backup disabled on the test nets (#2365); `config-assertions` false failures on the test nets stopped (#2369, #2373).
- **ansible:** `pgbackrest_exporter` config read access, telemetry path and per-host scrape/removal fixed (#2364, #2360, #2366, #2370).
- **smoke:** price and verified-asset checks skipped on test nets (#2384).

### Changed

- Go bumped to 1.27.1 and Node to 24 LTS (#2357).

## [v0.104.0] — 2026-10-05

41 commits since v0.103.0. Two migrations (0206, 0207). No `pkg/*` break.

### Migrations

- **0206:** clears projected-rebuild checkpoints that a table-emptying migration strands (#2339).
- **0207:** `price_source_contributions.window` is NOT NULL; the API now requires it (#2182).

### Added

- **history:** per-point provenance on derived XLM/USD bars (#2168).
- **api:** thin-market flag with substance evidence on `/v1/vwap` and `/v1/twap` (#2335); operator under-review hold, hot-reloadable from file (#2200).
- **aggregate:** `excluded_sources` read-time VWAP kill-switch (#2232).
- **aggregator:** alert when `change_summary_5m` stops refreshing (#2217).
- **clickhouse:** `movements_by_asset` added and `account_movements` re-keyed asset-first (#2345). Operator: apply the Step-1 DDL in `deploy/clickhouse/movements_by_asset.sql` on r1, testnet and futurenet before deploy.
- **rozo:** relayer bridge flow classified over `account_movements` (#2349).

### Fixed

- **completeness:** defindex's projection read is scoped to its gated contract set; sep41_transfers' re-proof stays out of the pass (#2332).
- **deploy:** the ch-schema-drift intent is re-stamped on every binary deploy (#2343).
- **ops:** scheduled heavy jobs queue behind the host lock (#2175); ch-instance-backfill records a genesis watermark on a complete run (#2327).
- **api:** `proxy_deviation` set on the primary price endpoints (#2342); classic assets' derived-SAC supply labelled net on `/supply` (#2329); `stale` and `as_of` stamped on `/v1/pools` SWR serves (#2226); degraded 200s are no-store (#2328); overflowing OHLC day counts rejected (#2245).
- **lake:** unreadable txs counted in `ledgers.tx_count` (#2334).
- **chops:** `ch-rebuild -write` records an ADR-0033 dirty window (#2338).
- **aggregator:** fiat EUR/GBP windows held to the `min_usd_volume` floor (#2337).
- **changesummary:** a wild 1m bucket stays out of the ATH/ATL ratchet (#2227).
- **timescale:** continuous aggregates refresh in day-sized bucket-aligned calls (#2336).
- **explorer:** contract events re-read unbounded when the active-ledger walk yields a short page (#2333); contract-page sidecars read concurrently (#2330).
- **external:** CEX trades with an implausible vendor timestamp dropped (#2247).
- **forex:** history stuck streak counted per refresh (#2233); only fresh upstream rates count as source entries (#2211).
- **monitoring:** alert when the asset-character rollup stops succeeding (#2230); heartbeat oracle sources get a heartbeat-based stale budget (#2263).
- **timescale:** the `soroban_events` gap scan is chunk-pruned by close time (#2350).
- **ci:** push and PR identity checks scoped to their own commits (#2341, #2326).

### Documentation

- NOTICE added with the goxdr dual-license note (#2347).

### Tests

- **load:** k6 smoke scenario that fails on any `http_req_failed` (#2348).

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

