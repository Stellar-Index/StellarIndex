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

## [v0.116.26] - 2026-10-19

No migrations. No `pkg/*` break.

### Fixed

- **api:** `/v1/contracts/{id}/transfers?from=…` on a high-volume token answers 200 with `searched_from` set to the widest window it finished, instead of a 503 when the full-history read runs out of time (#3620).
- **api:** the 60 s self-prewarm also warms the `/v1/assets` listing reads that home, /assets and /rwa fire on first load (#3623), and the home page's `/v1/markets?limit=3` slot (#3624).

### Changed

- **scripts:** `api-latency-sweep.sh --pages` covers the main explorer pages (#3622).
- **deps:** explorer npm minor bumps (#3616) and zizmor-action (#3615).

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
