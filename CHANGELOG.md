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

## [v0.116.21] - 2026-10-18

No migrations. No `pkg/*` break.

### Added

- **explorer:** the asset holders tab has a paged Entry changes panel (trustline, claimable-balance and pool changes with the balance after). It is hidden for native XLM (#3590).
- **api:** `GET /v1/accounts/{g_strkey}/signer-on` and a "Signer on" panel list the accounts a key signs for. `ch-entry-index` writes the `signer` role. The route answers 503 `signer-index-not-built` until the `signer_index` watermark exists, so after deploy run `ch-entry-index -from <lake first ledger> -write` once (#3592).

### Fixed

- **aggregator:** the trade-path USD leg is guarded against a single outlier minute. The newest 41 leg minutes go through the same robust check as the served VWAP, and an XLM-leg tie between asset forms resolves in preference order (#3595).
- **sep1:** a domain counts as reached only when its last attempt succeeded, so long-dead domains no longer trip the systemic-failure verdict (#3594).
- **ops:** the pgBackRest wrapper republishes its textfile after every repo, so the off-site repo2 run no longer ages the file past 24h (#3593).
- **ansible:** queued scheduled heavy jobs start in arrival order instead of racing for the lock (#3596).

### Changed

- **explorer:** an asset's entry-changes first page is cached, cut per limit and prewarmed (#3588).

## [v0.116.20] - 2026-10-18

No migrations. No `pkg/*` break.

### Added

- **explorer:** the accounts list shows the directory name beside each address and flags scam-tagged entries, from one batched `/v1/directory` request per page (#3587).
- **ansible:** `clickhouse_default_user_locked` (off by default) gives ClickHouse's `default` user a random password that is never stored (#3580).

### Fixed

- **explorer:** `contract_active_ledgers` records its own genesis watermark. A contract's activity feed trusts a proven-complete index: an empty walk is an empty page and a short page is the whole history, with no lake fallback (#3586). After deploy, run `ch-contract-ledgers-backfill -full -write` once to record the watermark.
- **timescale:** `ExactTierUSDVolume` values a zero pegged base at exactly $0, matching the served path and the restamp (#3585).
- **ops:** four ClickHouse ops scripts run the deployed `stellarindex-ops` instead of a stale hand-built copy (#3582).
- **ansible:** the test nets run the lake self-healer (`ch-live-catchup`) (#3584).

### Changed

- **deps:** Go and explorer minor bumps, regenerated third-party notices, and a refreshed golang base image (#3583, #3502).

## [v0.116.19] - 2026-10-17

Migrations 0217 and 0218, both additive. No `pkg/*` break.

### Fixed

- **aggregate:** a liquid pair's window VWAP no longer mixes legs that cover different spans. `TradesInRange` caps each leg at its newest 10,000 rows (XLM/USD 24h was 83 bps off). A capped leg now fills the rest of its window from the new `trade_sums_1m` continuous aggregate (migration 0217), and the local outlier filter weighs trades, not rows (#3575).
- **supply:** classic supply freshness is anchored on a processed-ledger watermark (migration 0218) instead of the oldest of four component tables. A quiet claimable-balance table no longer makes the supply gate reject current classic snapshots as dormant (#3576).
- **ops:** galexie resumes from the newest partition instead of a full recursive bucket listing, and fails loudly when the listing fails. Job locks created by root are chowned to the service user (#3578).
- **ops:** `ch-live-catchup` is installed on every ClickHouse host, r1 included, so it no longer runs a stale copy as the `default` user (#3579).

## [v0.116.18] - 2026-10-17

### Fixed

- `/v1/price` for a pair with no recent 1-minute bucket bounds its last-trade fallback to the 400-day lookback the primary path uses; the unbounded walk planned every `trades` chunk and took 0.5-1.8 s per miss. A pair idle longer than 400 days now answers not-found rather than withheld (#3571).
- **ops:** `ch-backfill` reads archive ranges trimmed to the cold tier through to it, so a re-derive over them needs no rehydrate first (#3570).

### Changed

- **alerts:** comet is no longer on the low-volume DEX silence rule; its pools can sit dormant for days, and projector lag and completeness already watch it (#3569).

## [v0.116.17] - 2026-10-17

### Fixed

- `stellarindex_trade_inserts_total` no longer counts a trade refused by the thin-market gate as `usd_volume_populated="no"` when its rate came from a cached miss that outlived the hourly substance verdict; bursts on thin pairs had pulled the on-chain USD-volume coverage ratio under its alert threshold (#3567).
