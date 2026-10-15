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

## [v0.116.0] — 2026-10-15

4 commits since v0.115.1. No migrations. No `pkg/*` break.

### Added

- **explorer:** the asset header shows the asset's identity and its SAC address with copy buttons; a classic asset's SAC page redirects to the classic page, and search resolves a SAC address or a bare issuer to its asset (#3514).

### Changed

- **api:** `/v1/assets?asset_class=all` lists a SAC-wrapped asset once, folding the SAC row into its classic row and counting its 24h volume exactly once; volume that cannot be read or confirmed sets `volume_lower_bound` (#3514).
- **ops:** ch-lake-backup waits up to 20h for the gap between heavy chunks instead of deferring whenever a heavy job holds the lock (#3516).

### Fixed

- **api:** `/v1/oracle/latest` no longer serves the retired Ethereum chainlink feed's last price (#3517).
- **ansible:** the host keeps a zfs module for every kernel it can boot, so a reboot onto a new kernel finds its pool (#3515).

## [v0.115.1] — 2026-10-15

1 commit since v0.115.0. No migrations. No `pkg/*` break.

### Added

- **monitoring:** `stellarindex_usd_volume_xlm_bound_skipped` tickets when verify-usd-volume could not judge a day's XLM-base/quote bounds; the run publishes `stellarindex_verify_usd_volume_xlm_bound_judged` (#3512).

### Fixed

- **ops:** verify-usd-volume builds the day's XLM/USD rate from `prices_1h`, so the 05:12 UTC run judges the XLM bounds instead of skipping them while `prices_1d` is still unmaterialized (#3512).
- **monitoring:** `stellarindex_usd_volume_estimated_tier_high` alerts on the estimated tier's share of valued volume (> 5%) instead of a $10M bar it crossed every day (#3512).

## [v0.115.0] — 2026-10-15

6 commits since v0.114.0. No migrations. No `pkg/*` break. Removes the `[chainlink]` config section (minor, pre-v1).

### Operator actions

- **ingest:** after deploy, run `stellarindex-ops projector-replay -source chainlink-stellar -from 64442968` to fill the new source from its first on-chain round (#3507).
- **config:** the `[chainlink]` Ethereum section and `CHAINLINK_RPC_URL` are no longer read; a leftover key warns at boot instead of failing (#3508).

### Added

- **sources:** `chainlink-stellar` ingests Chainlink Data Feeds published on Stellar, gated on the feed contracts' identity; it is the Chainlink divergence reference (#3507).

### Removed

- **sources:** the Ethereum Chainlink poller, its metrics, alerts and runbook sections (#3508).
- **explorer:** the blog (#3505).

### Fixed

- **explorer:** a route switch no longer crashes a page with `Cannot read properties of undefined`; one React Query key now holds one response shape (#3504).
- **explorer:** link hovers no longer log 404s for the `__PAGE__` prefetch files (#3509).
- **explorer:** status, methodology and service-level pages use the standard page shell; the footer matches the content width; the network switcher stays on screen; the sidebar search box is darker (#3505, #3506).

## [v0.114.0] — 2026-10-15

67 commits since v0.113.0. No migrations. One new ClickHouse table (`stellar.sponsorship_events`). No `pkg/*` break (`pkg/client` gains the account entry-change, sponsorship and claimable-balance routes).

### Operator actions

- **ClickHouse:** apply `deploy/clickhouse/sponsorship_events.sql`; `ch-entry-history` then derives only sponsorship events, under its own watermark and never past `entry_index` (#3438, #3446).
- **ops:** `sep1-refresh` publishes run counters through the node-exporter textfile; `stellarindex_sep1_refresh_*` alerts cover timeouts, failures and staleness (#3448).

### Added

- **api:** `GET /v1/accounts/{g_strkey}/entry-changes` and `GET /v1/assets/{asset_id}/entry-changes` from the entry index (#3435, #3437).
- **api:** an account's sponsorship lifecycle, with a proof against its counters (#3439).
- **api:** `GET /v1/accounts/{g_strkey}/claimable-balances`: the account's unclaimed claimable balances; the explorer account page shows them (#3490).
- **api:** `?include=sparkline24h` fills `price_history_24h` on `/v1/assets` (#3489).
- **api:** `GET /v1/assets/{asset_id}/trustline-history`: trustlines created and removed per UTC day with the open count; the asset page's Holders tab charts it (#3498).
- **explorer:** every page rebuilt on the new design system, with clickable table rows, keyboard focus rings, the converter rework and a data-source disclosures overlay (#3459–#3487).
- **ops:** `verify-network-state` gains a live-state check (#3453).
- **status:** status.stellarindex.io is built from the explorer, with the same chrome; the standalone `web/status` app is gone (#3493, #3494).

### Fixed

- **lending:** Blend backstop and emission KPIs are shown in tokens, not base units (#3488).
- **protocols:** the SDEX 7d block builds again; its raw-trade KPIs (unique traders, average size) are served at 24h only (#3499).
- **dex:** the stubbed Phoenix pool is out of the TVL pool set (#3454).
- **archive:** `galexie-mirror-verify` normalises upgrade entry-change order (#3450); hot-archive mismatch samples carry both ledgers (#3457).
- **monitoring:** the availability-burn, quiet-source and curated-rwa-sync absent alerts no longer fire spuriously (#3452, #3461).
- **monitoring:** the notify failed-send alert shows a whole count (#3497).

### Changed

- **divergence:** served Stellar assets that CoinGecko lists are mapped to their coin ids (#3455).
- **trades:** token/token USD volume is bounded by a per-asset credibility budget charged to the deeper anchor market; a print over budget stores NULL `usd_volume`, never a capped value (#3495).
- **release:** `cut-release.sh` starts the full CI run beside prepush, and the full run's fuzz smoke is sharded four ways (#3492, #3496).
- **deps:** npm and actions minor/patch groups bumped (#3423–#3425).

## [v0.113.0] — 2026-10-13

25 commits since v0.112.0. No migrations. Two new ClickHouse tables (the entry index). No `pkg/*` break (`pkg/client` gains optional credit, price-provenance, vetted-ISIN and `detectors_partial` fields).

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
- **clickhouse:** the entry-change ledger bounds are read in key order instead of aggregating every part, which was timing out the DEX TVL refresh (#3433).
- **supply:** archived SAC balances stay served; only real removals are tombstoned (#3414).

### Changed

- **deps:** go minor/patch group bumped, 11 updates (#3412).
- **test:** 29 small test files folded into their package files; style-only AST and prose guard tests deleted (#3406–#3409).
