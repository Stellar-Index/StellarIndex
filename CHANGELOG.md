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

## [v0.116.48] - 2026-10-25

No migrations. No `pkg/*` break (`LendingPool` gains `Summary`, `SummaryPending`, `SummaryError`). No config change. New ClickHouse tables (`account_op_type_counts`, `account_movement_counts`, `account_claimables`, `claimable_removals`, `sponsorship_totals_buckets`) and the holders rollup's `circulating` column are applied on every host before deploy; backfills follow their `deploy/clickhouse/*.sql` steps.

### Fixed

- **explorer:** dead links found by a browser crawl of all three sites are gone: ticker links render as text when no page exists, unregistered sources show an empty state, incident docs link correctly; the crawl runs daily (#3840).
- **api, explorer:** testnet serves its own Circle USDC and asset catalogue, so `/assets/usdc` and `/assets/xlm` resolve there (#3839).
- **xdrjson:** muxed auth-credential addresses render instead of an empty string (#3779).
- **timescale:** no-op trade replays stay off compressed chunks (#3825).

### Changed

- **explorer:** first visits are faster: API preconnect, first-wave preloads on `/markets/` and `/accounts/{G}`, fewer duplicate calls on account and home (#3831, #3834, #3838).
- **api:** the lending pools list carries each pool's summary; lending pools, reserves and the markets sparkline batch are cached (#3832, #3833, #3836).
- **explorer:** account pages read per-account indexes and rollups (claimables, op-type counts, sponsorship buckets, movement partitions) instead of scanning the lake (#3780, #3781, #3807-#3812).
- **perf:** contract, ledger, supply and protocol reads memoised or bounded (#3793, #3794, #3813-#3824, #3826).
- **ops:** G6 page-panel sweep every 30 min, derived-index and fallback alerts, chunk-destroying-call guards, pgBackRest repo options, heavy-job pool floors (#3768-#3778, #3783-#3806).

## [v0.116.47] - 2026-10-21

No migrations. No `pkg/*` break. No config change.

### Fixed

- **api:** identical in-flight `/v1/price/batch` rows are resolved once and shared across requests, so the explorer build's sharded identical batches stop queueing behind each other past the 200 ms latency SLO (#3766).

### Changed

- **tests:** guardscan scanner fixtures build in a temp module (#3765).
- **docs:** config-assertion, incident and six operator docs re-verified; two dead multi-region docs deleted (#3763, #3764).

## [v0.116.46] - 2026-10-21

No migrations. No `pkg/*` break. No config change.

### Fixed

- **api:** after startup the API warms every asset detail on the top-500 listing once, so the explorer build's first pass after a deploy no longer reads cold and burns the latency SLO (#3759).

### Changed

- **docs:** archival-node bring-up, Hubble event counts and nine WASM audits re-verified against the code and r1 (#3757, #3760).

## [v0.116.45] - 2026-10-21

No migrations. No `pkg/*` break. No config change.

### Fixed

- **pricingguard:** the substance gate's verdict cache drops expired verdicts before resetting at its size cap; an explorer build measures more pairs than the cap in one TTL, so each reset sent the hot pairs back to Postgres mid-build and re-logged every withheld pair's first-observation WARN (#3755).
- **wasmaudit:** the Spectra order engine's in-place upgrade at ledger 64,907,566 (WASM `69e90533…`) is audited and admitted; `wasm-drift` reported it as unaudited (#3756).

## [v0.116.44] - 2026-10-21

No migrations. No `pkg/*` break. No config change.

### Fixed

- **wsclient:** the ping watchdog counts a stall only when the ping times out; a venue closing the socket under the ping is reported as the close it is, not as `ErrStreamStalled`, so a routine Kraken reconnect no longer fires `cex_stream_stalled` (#3750).
- **explorer:** embed widgets render from the build's one `/v1/assets` listing call instead of one `/v1/assets/{id}` each; the ~500 cold detail calls per build tripped the latency SLO burn during every explorer deploy. The asset page's listing call asks for `include=sparkline24h`, the token the API matches (#3752).

### Changed

- **api:** the markets, history and network_stats caches log the error behind a refresh failure (#3747).
- **ci:** doc-links scoped checks are timed against the slowest full scan (#3748).
- **docs:** supply history corrected for eviction tombstones (#3743); two stale runbooks, the self-hosting guide (#3749), the design-system doc (#3751), the WASM audit README and the r1 archive section (#3753) re-verified.
