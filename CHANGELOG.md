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

## [v0.104.0] — 2026-10-05

36 commits since v0.103.0. Two migrations (0206, 0207). No `pkg/*` break.

### Migrations

- **0206:** clears projected-rebuild checkpoints that a table-emptying migration strands (#2339).
- **0207:** `price_source_contributions.window` is NOT NULL; the API now requires it (#2182).

### Added

- **history:** per-point provenance on derived XLM/USD bars (#2168).
- **api:** thin-market flag with substance evidence on `/v1/vwap` and `/v1/twap` (#2335); operator under-review hold, hot-reloadable from file (#2200).
- **aggregate:** `excluded_sources` read-time VWAP kill-switch (#2232).
- **aggregator:** alert when `change_summary_5m` stops refreshing (#2217).

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
- **ci:** push and PR identity checks scoped to their own commits (#2341, #2326).

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
