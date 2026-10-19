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

## [v0.116.25] - 2026-10-19

No migrations. No `pkg/*` break.

### Fixed

- **api:** the heavy prewarm refills the sources-stats slots whatever their age, so `/v1/sources?include=stats` no longer lapses between prewarm ticks (#3612).
- **api:** `/v1/issuers` serves the last good list (flagged `stale`, `as_of` at its fill time) while one background refresh runs, instead of blocking a visitor on the rebuild (#3613).
- **api:** `/v1/markets/sources` is cached for a minute behind the same stale-while-refresh cache, so a repeat asset or pair answers from memory instead of a ~2 s aggregate (#3614).
- **ops:** `ch-rebuild -trades-by-account` runs the per-WASM lake gate once per invocation and lands 10k-ledger sub-windows, so a 1M-ledger range no longer pays the gate's whole-history factory walk 100 times (#3617).

## [v0.116.24] - 2026-10-19

No migrations. No `pkg/*` break.

### Fixed

- **api:** the asset-detail prewarm re-renders each verified asset instead of being served its own cache hit, so the entry no longer lapses once per TTL and a visitor no longer waits on the inline rebuild (#3609).
- **api:** the RWA value and premium histories are rebuilt on every prewarm whatever their age, so `/v1/rwa/history` and `/v1/rwa/premium` no longer expire between prewarm ticks and block a visitor on both scan legs (#3610).

## [v0.116.23] - 2026-10-19

No migrations. No `pkg/*` break.

### Fixed

- **api:** the default `/v1/assets` page is refreshed by a background prewarm every TTL, so a client polling minutes apart no longer lands past the cache's max age and waits ~1 s for an inline rebuild (#3605).
- **api:** `/v1/accounts/{g}/entry-changes` streams only the entry groups that can concern the account, about a tenth of the rows on a busy account, instead of decoding every change in the indexed ledgers; the response is unchanged (#3607).

## [v0.116.22] - 2026-10-18

ClickHouse DDL: re-apply `deploy/clickhouse/entry_index.sql` (adds the `signer` value to `entry_index.role`; metadata-only, already applied on r1). No migrations. No `pkg/*` break.

### Added

- **ops:** `ch-entry-index -signers-only -write` backfills the `signer` role alone, from the lake floor up to the `entry_index` watermark, under its own lock and job name, so `/v1/accounts/{g}/signer-on` can be built without re-running the entry index (#3601).

### Fixed

- **api:** `/v1/oracle/latest` reads the last 48 h per request and merges a background-refreshed all-time read for silent streams: ~15 ms instead of 150-480 ms on a cache miss, which burned the 200 ms latency SLO (#3603).
- **sep1:** failures the domain answered (HTTP error, redirect, parse, too large, too deep) no longer count toward the systemic-failure verdict (#3600).
- **clickhouse:** `entry_index.role` declares `signer`, so `ch-entry-index` no longer fails inserting signer rows (#3602).

## [v0.116.21] - 2026-10-18

No migrations. No `pkg/*` break.

### Added

- **explorer:** the asset holders tab has a paged Entry changes panel (trustline, claimable-balance and pool changes with the balance after). It is hidden for native XLM (#3590).
- **api:** `GET /v1/accounts/{g_strkey}/signer-on` and a "Signer on" panel list the accounts a key signs for. `ch-entry-index` writes the `signer` role. The route answers 503 `signer-index-not-built` until the `signer_index` watermark exists, so after deploy run `ch-entry-index -from <lake first ledger> -write` once (#3592).

### Fixed

- **aggregator:** the trade-path USD leg is guarded against a single outlier minute. The newest 41 leg minutes go through the same robust check as the served VWAP, and an XLM-leg tie between asset forms resolves in preference order (#3595).
- **api:** a `/v1/price` miss on an on-chain pair reuses a cached Postgres plan for its last-trade walk: ~5 ms per alias instead of ~50 ms, which kept XLM-leg misses past the 200 ms latency SLO (#3598).
- **sep1:** a domain counts as reached only when its last attempt succeeded, so long-dead domains no longer trip the systemic-failure verdict (#3594).
- **ops:** the pgBackRest wrapper republishes its textfile after every repo, so the off-site repo2 run no longer ages the file past 24h (#3593).
- **ansible:** queued scheduled heavy jobs start in arrival order instead of racing for the lock (#3596).

### Changed

- **explorer:** an asset's entry-changes first page is cached, cut per limit and prewarmed (#3588).
