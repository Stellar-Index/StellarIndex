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

## [v0.113.0] — 2026-10-13

21 commits since v0.112.0. No migrations. Two new ClickHouse tables (the entry index). No `pkg/*` break (`pkg/client` gains optional credit, price-provenance, vetted-ISIN and `detectors_partial` fields).

### Operator actions

- **ClickHouse:** apply `deploy/clickhouse/entry_index.sql`, then run `ch-entry-index` to build `stellar.account_entry_ledgers` and `stellar.asset_entry_ledgers`; the `entry-index` timer keeps them current and `stellarindex_entry_index_lagging` fires when it falls behind (#3418).
- **ops:** `verify-usd-volume` runs nightly; `stellarindex_usd_volume_estimated_tier_high` and `stellarindex_verify_usd_volume_stale` are new alerts (#3416).
- **config:** EURC and PYUSD are declared fiat-pegged legs (EUR, USD) in the host template (#3415).
- **ansible:** `compute-completeness-sep41` is installed only on hosts that watch SEP-41 contracts (#3405).
- **ingest:** `backfill-external` replays a vendor daily XLM/USD close CSV into `trades` as `coingecko_daily`; its write gate defaults to dry-run (#3427).

### Added

- **api:** lone-vendor values are credited and fiat listing rows carry FX provenance (#3420); the peg walk's withheld reason is served instead of a bool (#3417); `/v1/mev` flags `detectors_partial` off a detection heartbeat (#3413).
- **rwa:** a hand-vetted ISIN binding is the fourth classic basis (#3419).
- **api:** fiat-to-fiat chart series and oracle prices fed by one licensed source carry its credit; a declared ISIN that contradicts a hand-vetted binding is refused (#3431).
- **trades:** declared fiat-pegged legs are valued off the fiat rate within a 2% band (#3415).

### Fixed

- **api:** vendor-only price buckets are skipped on the inception anchor and point-in-time VWAP, their volumes are blank on the wire, and non-VWAP sources stay out of the raw trade window (#3428).
- **mev:** frozen XLM/USD-proxy minutes are excluded from the anchor and refused at ingest, anchor-valued notionals are flagged, and a stored `usd_volume` stamped inside a freeze is set aside (#3411, #3426, 9c4c6ccf8).
- **freeze:** a lost `freeze_events` row is retried, counted and logged (#3422).
- **supply:** archived SAC balances stay served; only real removals are tombstoned (#3414).

### Changed

- **deps:** go minor/patch group bumped, 11 updates (#3412).
- **test:** 29 small test files folded into their package files; style-only AST and prose guard tests deleted (#3406–#3409).

## [v0.112.0] — 2026-10-12

214 commits since v0.111.0, most of them test-file folds and comment clean-up. One migration (0215). One new ClickHouse table. No `pkg/*` break (`pkg/client` gains optional `stale` and `triangulated` price fields).

### Migrations

- **0215:** trigger-maintained current trustline balance for classic supply; the reader seeds lazily, no replay (#3322).

### Operator actions

- **ClickHouse:** apply `deploy/clickhouse/trades_by_account.sql`, then run `ch-rebuild -sdex -trades-by-account` to fill it (#3395).
- **ops:** `galexie-mirror-verify` reports decode-equivalent mirror objects as a separate `equivalent` result class, apart from real divergence (#3402).
- **ops:** flags removed because nothing passes them: `detect-gaps -rpc-max-staleness`, `issuer-flags -chain-recheck-limit`, `state-snapshot -throttle-ms` (#3401).
- **config:** five never-set keys removed, including `redis_closed_bucket_channel` and `background_statement_timeout`; defaults are inlined (#3392).

### Added

- **explorer:** account trades below the Postgres horizon are served from `stellar.trades_by_account` (#3395).
- **api:** asset listing rows carry `contract_id`; a non-fiat asset in a crypto ticker is priced through its USD legs (#3314, #3312).
- **notify:** Resend tracking pinned off; suppression derived from bounce and complaint events (#3394).
- **ops:** daily age-encrypted config and secrets backup timer (#3313); `galexie-mirror-verify` prints each missing-local object (#3298).
- **status:** `status.stellarindex.io` is a standalone incident page (#3316).

### Fixed

- **explorer:** static-build pages price through `/v1/price/batch` (#3393); build-time API requests are capped per worker (#3317); account movements warn only while served coverage is incomplete (#3390).
- **archive:** holes inside the trim are accepted, and the weekly upstream check is excluded from the 24h stale alert (#3391).
- **ops:** `galexie-archive-fill` queues behind a held heavy lock instead of deferring (#3319); `config-backup` pg_dump switches to postgres and logs why it fails (#3318); lake-backup orphans are swept from a started record (#3291); `directory-sync` is pinned to a reviewed commit (#3308).

### Performance

- **completeness:** the recognition shape scan is split by `topic[0]` kind (#3356).

### Changed

- **config:** unreferenced script removed; CLI flags and config keys nothing sets are folded into their defaults (#3389, #3392, #3401).
- **test:** several hundred small test files folded into their subject files (#3360–#3403).

## [v0.111.0] — 2026-10-10

104 commits since v0.110.0. No migrations, no `pkg/*` break. Removed config keys no deployment sets (#3119, #3120).

### Added

- **api:** account trades carry `base_decimals`/`quote_decimals`; Blend positions carry `decimals` (#3165, #3166).
- **web:** native liquidity pool pages, external-asset price charts, contract state-archival verdict, ledger page charts with CSV/JSON export (#3136, #3139, #3135, #3132).
- **web:** CSV/JSON download on panels, the RWA set, the MEV feed and freeze timeline, and `/network` (#3128, #3146, #3145, #3144).
- **web:** status page 90-day incident strip and backup age-vs-SLO bars; latest-ledgers close gap; `/operations` type filter; verified-currency chips on `/assets` (#3149, #3148, #3147, #3141, #3137).
- **web:** tx page shows fee bumps, the failing op and memo type; protocol pages link website, docs and source; muxed M- and C-addresses route (#3129, #3131, #3127).

### Fixed

- **api,web:** oracle feeds named by the classic asset their SAC wraps (#3134).
- **web:** same-code pool sides told apart and linked by issuer (#3140, #3133); duplicate SDEX volume chart dropped (#3142); XLM `total_coins` labelled as a counter (#3138).
- **ops:** projected-rebuild stays inside the ClickHouse query budget (#3125).
- **ansible:** the core auto-upgrade apt update is scoped to SDF's list (#3091).

### Changed

- **web:** figures render at a readable precision with the exact value on hover (#3159–#3166); XLM is "Stellar Lumens", never "native" (#3155).
- **web:** page descriptions, eyebrows and long explainers cut; one style for section headings and panel labels; one "External references" footer (#3150–#3158, #3130, #3126).
- **ops:** ClickHouse and Postgres ZFS snapshots keep one day (#3167).
- **config:** unset `[divergence]` tuning keys and worker tuning keys removed; unused service flags hardcoded (#3119, #3120, #3124).
- **ops:** `backfill-router`, `backfill-index`, `sdex-claim-audit`, the CoinGecko historical-backfill path, unreferenced scripts and 18 Makefile targets deleted (#3115, #3118, #3121–#3123).
- **test:** split test subjects consolidated, doc-prose pins dropped, go/ast tests frozen by a shrink-only baseline (#3062–#3114).

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
