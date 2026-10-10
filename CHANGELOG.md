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

## [v0.110.0] — 2026-10-10

84 commits since v0.109.0. Two migrations (0213, 0214). No `pkg/*` break.

### Migrations

- **0213:** least-privilege `stellarindex_api` Postgres role and its grants (#3027).
- **0214:** pins `apply_api_role_grants()` `search_path` (#3041).

### Added

- **api:** per-asset ledger-entry history from `stellar.asset_entry_changes` (#3029); asset holders and `/v1/history` as CSV on `Accept: text/csv` (#3022); `asset_id` and `issuer` on `/v1/assets/verified` (#3026).
- **web:** `/convert` landing page opening on XLM to USD, verified Stellar assets convertible, alias slugs redirected (#3006, #3038, #3020).
- **web:** asset-tab visuals: trades plotted with exact amounts, venue ranking, pair volume bars, average trade size, holder share, oracle spread, issuer active span, archived storage-read supply (#2995–#3005).
- **web:** MEV routes, divergence trends, trade scatter, anomaly freezes, ledger close cadence, counterparty protocol mix, and served-data visuals on contract, diagnostics, cohort and home (#2989–#2994, #3058, #3060, #3061).
- **build:** `THIRD_PARTY_NOTICES` generated, shipped with releases and drift-gated in CI (#3031).

### Fixed

- **api:** the exact dedup read behind `/v1/assets/{id}/entry-changes` is floored (#3032); a stablecoin depeg warns past 1%, not 5% (#3011).
- **explorer:** fees are no longer reported as a movements coverage gap (#3057); the status page no longer labels the archive watermark as served (#3028); a blank market cap says why (#3007).
- **ecb:** the poll fails when the reference date does not parse (#3025).
- **ops:** compute-completeness stays under `max_query_size` (#2985) and scopes the `sep41_supply` lake read to `topic_0_sym` (#3024).
- **ansible:** `vm.max_map_count` raised to 4,194,304 on archival nodes (#3013); ClickHouse writes no core dump on crash (#3003).

### Changed

- **web:** every page renders the shared page shell, with a guard test (#3008–#3018, #3033–#3037); dev design-system routes deleted (#3040).
- **web:** headline prices, supplies and market caps round from wire strings, never doubles (#2976–#2988).
- **web:** insight and status prose folded into tooltips (#2991, #3059); privacy policy names the off-site backup provider and region (#3001).

## [v0.109.0] — 2026-10-09

12 commits since v0.108.0. No migrations, no `pkg/*` break.

### Added

- **wasmaudit:** the defindex vault, factory and strategy WASM hashes are audited, so the defindex replay gate admits them (#2974).
- **web:** charts for creators, sponsors, signers, trustline use and issuer activity (#2963); ledger operations as bars (#2964).

### Changed

- **web:** USD volumes, SDEX KPIs, pair and DEX-source volume and TVL, network XLM figures, issuer volume and accounts wealth render from exact decimals, never doubles (#2966, #2967, #2969–#2973, #2975).
- **web:** prose cut on the RWA and ledger pages (#2964, #2968).

## [v0.108.0] — 2026-10-09

31 commits since v0.107.0, mostly explorer charts and exact-decimal money on the web. No migrations, no `pkg/*` break.

### Fixed

- **wasmaudit:** defindex factory events are walked by their `DeFindexFactory` topic[0] prefix, so the replay gate admits defindex (#2962).
- **api:** coverage `earliest`/`latest` come from the completeness verdict (#2938).
- **ansible:** the exchangeratesapi block renders, gated on its vault key (#2941).

### Changed

- **web:** trade volume, pool reserves, protocol TVL, RWA history and the MEV feed rank, total and format with exact decimals, never doubles (#2958, #2959, #2960).
- **web:** charts across ledger, transaction, network, contract, account, asset, market, MEV, status and source pages (#2951–#2957); prose walls cut (#2949, #2950).
- **web:** SAC contract pages show the wrapped asset (#2947); the contracts page shows deployments per month and 90-day actives (#2946); breadcrumbs on every content page (#2942).

## [v0.107.0] — 2026-10-09

190 commits since v0.106.0, almost all comment and doc clean-up. No migrations, no `pkg/*` break.

### Fixed

- **timescale:** a zero USD-pegged base leg is valued at exactly $0 (#2890); SEP-41 cursor indexes clamp to smallint instead of wrapping (#2881).
- **dashboardauth:** a signed-in browser gets its own code-lockout budget (#2878).
- **clickhouse:** a large gated contract set is sent as an external table (#2807).
- **indexer:** the hashdb history slice, which compared our bytes to SDF's, is dropped (#2806).
- **ops:** pgbackrest backups get a 900s archive timeout (#2761).

### Changed

- Go bumped to 1.27.2 and golang.org/x/net to v0.60.0 (#2899).

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
