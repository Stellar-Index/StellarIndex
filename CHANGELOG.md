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

## [v0.119.0] - 2026-11-15

Tested against Stellar protocol 27. No migrations. No `pkg/*` break. No config change on r1. Host payload changed: the archival-node role's API config template (a test-net inventory sets `stellarindex_api_trusted_proxy_cidrs`; r1 renders the same file as before).

### Fixed

- **api:** a folded OHLC bar (5m, 30m, 2h, 4h, 12h, 3d, 2w) is served only once its last source bucket is materialised, so a closed bar no longer changes after it is first served. The bound is the same for every pair in a request; a quiet pair's finished bar can wait for the next view refresh (about 20 minutes on hour-based bars, up to 12 hours on day-based, up to 2 days on week-based).
- **ansible:** a test-net API trusts the host proxy's address, so clients behind it no longer share one rate-limit bucket.
- **ci:** the migration-compat gate knows `REVOKE` and a `BEFORE` trigger; the lake-dedup gate watches `stellar.contract_events`; the baseline-growth tripwire watches the waiver maps of the two Go lints.

## [v0.118.0] - 2026-11-15

Tested against Stellar protocol 27. No migrations. No `pkg/*` break; `pkg/client` gains `market_cap_lower_bound`. No config change. Host payload changed: the archival-node and redis-sentinel roles, role scripts and the r1 Prometheus rule trees; apply the roles after the binaries. No replay.

### Changed

- **api:** the asset detail carries a stale price and an unread gate; a listing cap on a floor is flagged, and every markets listing carries its volume floor flag. A flagged issuer's SAC row, curated flag and SEP-1 claims are withheld.
- **api:** a nonstandard-decimals token's volume is stated in its own decimals. A dust-only direction sets no OHLC price. A contract with no supply event is not a zero supply on the asset detail.
- **auth:** adding or removing a passkey needs a recent sign-in.
- **explorer:** `/rwa` says when its answer is stale or the set is unknown; figures the API flags as floors carry "≥" on `/assets`, the embed and `/rwa`.
- **alerts:** new rules for rule evaluation, the retention reaper, the fail-open price guard, a withheld supply, a full limiter map, shed stream ticks, unreadable tokens, email-code guessing and registration growth. A price alert does not fire on a bucket the freeze is holding back.
- **ansible:** a secret reaches a task on stdin, never through `environment:`. A superuser session in an app-owned database searches `pg_catalog` only.
- **explorer:** a fee bump is listed in the account history of the account that paid its fee (from the ledger the payer was first recorded). Host step: create `ops_by_source_feepayer_mv` and `account_activity_feepayer_mv` and run the fee-payer backfills in `deploy/clickhouse/ops_by_source.sql`; `ch-index-proof` now requires the first.

### Fixed

- **orderbook:** a tied offer is served from its post-image, never the row that won the tie.
- **ops:** the chunk scan decodes a backend-killing chunk at most twice and decodes a chunk recompressed in place again. Root jobs keep scratch files and the drill's Postgres private and hand psql the owner DSN in the environment. The root archive filler writes only inside the archive root. Projected-rebuild resumes only over checkpoints its own build wrote. `rederive-from` and `ch-rebuild` writes refresh the aggregates over the range they rewrote.
- **clickhouse:** the cap67 event census counts events, not rows.
- **ci:** PR tests include every tree-walking guard and a PR cannot scope its own tests; the migration gates see a dropped arbiter, keyword-less DROP/RENAME and a renamed-down migration.
- **rwa:** the premium history judges a day against the days accepted before it, never a refused one; a thin history stays within 10x of the published value, and a repricing needs an agreeing run at least as long as the accepted days.
- **ops:** the chunk scan decompresses every value, not only the column headers, and re-verifies every chunk once. A host with no probe key does not run the SLA probe. The os dataset's mode agrees with its ownership task.
- **ci:** the migration-compat gate reads a multi-line `ALTER` as one statement and a function the migration calls as migrate-time code; the lake-dedup gate charges a read to its own file and query block and reads a comma join.

## [v0.117.0] - 2026-11-14

Tested against Stellar protocol 27. No migrations. No `pkg/*` break; `pkg/client` gains lower-bound, `parent_key_id` and flag fields. Config: a repeated `supply.sdf_reserve_accounts` entry now fails validation. Host payload changed: the archival-node and HA roles, role scripts, systemd units and both Prometheus rule trees; apply the roles after the binaries. No replay.

### Changed

- **api:** partial totals carry a lower-bound flag: RWA and stablecoin totals, lending TVL, `/v1/protocols` TVL, 24h USD volumes on `/v1/assets`, `/v1/markets`, `/v1/network/stats` and `/v1/aggregators`, and listing valuations built on a floor supply.
- **api:** a read that failed is no longer a clean answer. Price, peg, supply, catalogue, guard, oracle and breakdown read failures now return an error or a stale, degraded, uncached response instead of a 404, an empty series, a zero or a declared 1.0.
- **api:** cached and carried figures are dated by when they were read: DEX TVL, catalogue rows, lending reserve valuations and fiat charts carry `stale` when the source is old.
- **api:** wide or filtered reads cost more rate-limit tokens: `/v1/vwap`, `/v1/twap`, single-bar `/v1/ohlc`, windowed `/v1/chart`, sparkline includes, address-filtered transfers and `/v1/markets?include=inception`. `/v1/divergence` rejects `window_days` above 30.
- **api:** `org_verified` is described as a domain link. A flagged issuer's self-written name, logo and anchor fields are cleared on asset surfaces.
- **api:** Spectra joins the protocol directory. An RWA classic member's SAC is no longer a second row. `/v1/rwa/history` names its supply basis. DeFindex deposit and withdraw volume is valued in USD.
- **auth:** a key records the key that minted it and shows in the owner's dashboard. A tier lowering clamps key quotas. Dashboard-minted keys no longer age out.
- **dashboard:** sign-ins, logouts, webhook and price-alert changes write audit rows.
- **explorer:** renders the flags the API serves (under review, stale, degraded, lower bound, derived supply), keeps withheld figures withheld, and labels the org badge "Domain linked".
- **ansible:** `/var/lib/stellarindex` and the textfile directory are root-owned with root-created subdirectories. Root stages installs outside `/tmp`. The Redis lockdown ACL covers what the binaries use.

### Fixed

- **pricing:** a lone leg minute prices no trade; `/v1/changes` ignores a bucket the served price refused; the transitive far-leg gate checks the market the hop was priced from; a pinned divergence entry reads as unchecked.
- **supply:** no SEP-41 supply is published until the contract's Soroban-era events are shown to be in the served tier. A failed reserve read is an error, not a static figure. A flowless contract with no reading is unmeasured, not zero.
- **freeze:** a failed marker write no longer releases the freeze; an operator release lands and is not undone by a retried write. `anomaly.freeze` fan-out runs off the price tick.
- **decimals-guard:** re-enumerates every six hours, retries failed corrections and reports scales it could not read.
- **auth:** passkey removal ends other sessions. A denied IP spends no other address's sign-in budget. A failed sign-up leaves no account holding the address. A full limiter map throttles the flooding regions only. Every `X-Forwarded-For` line is read.
- **alerts:** a missed webhook delivery is retried once; a webhook URL's path is never logged.
- **mev:** a detector that could not read is flagged partial; a sandwich victim must trade on the front leg's venue and direction.
- **projector:** two rejected rows in one ledger no longer wedge a source.
- **protocols:** a fanned event is counted once; breakdown reads carry an "other" bucket.
- **blend:** V1 user-liquidation auctions count on the account and protocol pages.
- **external:** id-less CEX trades are refused; the Bitstamp backfill walks past short pages; a baseline-less FX history is banded or held.
- **ops:** root writers use exclusive temp files and never follow a planted link. `pg-chunk-scan` decodes one chunk per transaction. Snapshot rotation survives a pinned snapshot. Tip readers use the live cursor. `rederive-from` refreshes the trades aggregates. Archive-fill retries unconfirmed partitions. The schema snapshot restores in order. The gap finder fails loudly. The node healthcheck lists one partition. `stellarindex-migrate force` takes a migration number that exists, or `none`. The route sweep fails on an unexpected 404. A failed migration names its recovery step.
- **deploy:** the config-apply gate diffs from the config the host has. The deploy job outlasts its rollback.
- **ansible:** refuses a pool create over a labelled disk, an un-acked disabled off-site repo and an un-acked galexie change before notifying. Redis re-applies keep the live topology. Promtail ships to an address Loki listens on.
- **ci:** the lake-dedup, i128 and migration gates catch what they let through; monitors report what they measured.
- **explorer:** per-network API host in built redirects, a live derived price, one verified identity per asset, and no Function call per RSC prefetch.

### Security

- **go:** toolchain 1.27.4 (GO-2026-6859).

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
