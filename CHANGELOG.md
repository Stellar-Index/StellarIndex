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

## [v0.116.51] - 2026-11-07

No migrations. No `pkg/*` break. Config: `[api].anon_rate_limit_per_min` is 1000 in the r1 template, already applied on r1. No replay.

### Changed

- **api:** the hosted anonymous limit is 1000/min per IP, and new free keys and the free tier ceiling are 6000/min. Keys already minted keep their stamped limit. The key form, pricing, terms, signup, contact, docs and landing pages state the new limits.

### Fixed

- **explorer:** a signed-in visitor who opens `/dashboard` directly stays there instead of being sent to `/signin`.
- **explorer:** the degraded banner no longer renders in the app shell, and `/contracts/{C}` preloads its first API calls.
- **ops:** `rederive-from` takes its lock and claims atomically, so concurrent runs cannot race on Ubuntu 26.04.

### Security

- **deps:** `klauspost/compress` 1.20.2 (GO-2026-6865) and `andybalholm/brotli` 1.2.7 (GO-2026-6888).

## [v0.116.50] - 2026-10-25

No migrations. No `pkg/*` break. No config change. Run `stellarindex-ops projector-replay -source chainlink-stellar -from 65079956` after the deploy to re-key the three USPC NAV oracle rows.

### Fixed

- **chainlink-stellar:** the USPC NAV feed (data_id `016e0640…`) is recorded as `raw:USPC` instead of an unknown `raw:<hex>` id, clearing the unknown-symbol alert (#3847).
- **explorer:** `/icon` stores successful issuer-icon responses in the Cloudflare edge cache, so a cold visitor no longer waits on the issuer's host (#3848).

## [v0.116.49] - 2026-10-25

No migrations. No `pkg/*` break. No config change. The weekly `compute-completeness-sep41` unit gains a second run for `sep41_supply`; the deploy's ansible step installs it.

### Fixed

- **completeness:** `sep41_supply` is re-proved weekly beside `sep41_transfers` instead of in the nightly pass, where its from-genesis re-derive hit the per-source timeout (#3843).
- **explorer:** a read the API sheds with `Retry-After` of 8 s or less is retried once, so account panels no longer show "warming or failed" for a busy cold read; the site crawl judges each API URL by its last response (#3844).
- **explorer:** links only to asset pages that exist (#3842).

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
