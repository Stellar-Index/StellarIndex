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

## [v0.136.0] - 2026-11-25

Tested against Stellar protocol 27. No migrations (schema version stays 236). No `pkg/*` break. New ClickHouse table for operators on an EXISTING host: `deploy/clickhouse/lending_interest.sql` creates the empty `stellar.lending_interest_hourly`; nothing writes or reads it yet, so applying it can wait for the release that does.

### Added

- **blnt:** `DecodeProtocolFees` reads a pool's `ProtocolFees` entry and `AccrueBetween` recomputes the interest between two consecutive stored versions of a reserve including the protocol fee taken before the backstop share, in exact integers. The rule (fee rate 20000 at 7 decimals, floor with a carried remainder) is inferred from chain data, since the contract source is not public; it reproduces every stored version of the three mainnet pools except one transaction, which it reports as a residual. Nothing serves it yet (#4365).
- **lake:** `stellar.lending_interest_hourly` holds per pool, asset and hour the gross interest, backstop share, supplier amounts and protocol fee as Int256 sums, with counts of versions that did not reconcile and their residuals. It is empty: no writer exists yet (#4368, #4369).

### Changed

- **explorer:** the static build prices `/convert` pages from one `/v1/price/batch` per quote instead of one request per page, reads a pair's 24h volume from the markets listing instead of calling `/v1/pairs` per pair page, and routes embed pages through the shared build fetch. On production the build's batch requests fell from about 1,280 per 20 minutes to 182 per build and its `/v1/pairs` calls from about 700 to 3. The build prints its request count per route after export (#4366).
- **explorer:** the build sends `/v1/price/batch` requests one at a time. Four in flight put 64 row workers on a database pool of 25 shared with live price reads, and 14 of 182 took over 200 ms (#4372).

### Fixed

- **supply:** a second concentrated-liquidity pool (`CBFY4Q26…SSZ2OFQ`) is judged a non-token. Its one-topic `mint` and `burn` carry a u128 liquidity `amount`, which the supply classifier skipped under an alerting reason; the contract emits no transfer, so no token supply was lost (#4371).
- **test:** the holders stale-bound test holds the refresh while it asserts the stale flag; an instant stub refresh could finish first and the fresh value was, correctly, served clean (#4367).

## [v0.135.0] - 2026-11-25

Tested against Stellar protocol 27. No migrations (schema version stays 236). No `pkg/*` break. Behaviour change for operators on an EXISTING ClickHouse host: run `deploy/clickhouse/ledger_entry_changes_codecs.sql` once, as the operator (no bootstrap applies it); it is metadata-only and idempotent, and only new parts take the codecs until `recompress-lec.sh` rewrites the old ones. Fresh hosts get the codecs from `tier1_schema.sql` (#4358).

### Added

- **obs:** `stellarindex_ratelimit_decisions_total{tier,result}` counts each API request once by its worst outcome (`allowed`, `fail_open`, `limited`, `unavailable`), and `stellarindex_ratelimit_utilisation_ratio{tier}` is a histogram of the share of the subject's per-minute budget used after each allowed request, including re-prices and cold-fill surcharges (for SEP-10, the larger of the account and per-IP shares). Tiers are `anonymous`, `apikey`, `sep10`, `operator`, `custom` and `other`. Both use the count the limiter already returns, so there is no extra store call; no label identifies a client; limiter decisions, status codes and headers are unchanged (#4363).
- **blend:** `AccrueBetween` recomputes the interest a pool accrued between two consecutive stored versions of a reserve with the contract's own integer arithmetic, and returns the gross, backstop and supplier amounts with two checks against the stored next version: whether `b_rate` matches, and the `backstop_credit` change the accrual does not explain. A pure function, not yet wired to any reader or route. Replayed over 6,093 consecutive mainnet versions, every pair reconciles to the base unit except two V1 interest-auction fills, which are reported with their exact residual (#4362).

### Changed

- **ops:** `recompress-lec.sh` converts `ledger_entry_changes` partitions with `ALTER TABLE ... REWRITE PARTS IN PARTITION`, which rewrites each part one-for-one and keeps duplicates; the earlier `OPTIMIZE ... FINAL` merged and deduplicated a ReplacingMergeTree, so it changed the stored rows. The codecs are ZSTD(3) on `account_id`, `tx_hash`, `asset` and `balance`, and Delta+ZSTD(3) on `intra_ledger_seq`. The driver refuses to start unless all five are in `system.columns`, skips the live tip partition, and releases a partition's freeze only when its row count, per-part rows and all-column hash match before and after; a mismatch keeps the freeze and exits non-zero. A pilot on production partition 30 gave identical signatures, 35.5 to 27.0 GiB (#4358).
- **ops:** `recompress-lec.sh` queries through `RECOMPRESS_CH`, default `/usr/bin/clickhouse-client --port 9300`, because unauthenticated HTTP on `localhost:8123` is refused on r1. No credential is in the script, its defaults, its logs or its argv. A startup probe of `system.mutations` aborts with a clear log line, before any ALTER, on a transport or auth problem (#4361).

### Fixed

- **api:** a prewarm render waits for the refresh it starts or joins and is judged on the entry as it then stands. An asset detail value read only by the prewarm expired at about 180 s, past the 150 s stale bound, so the page took the old value with its stale flag while a healthy refresh had only just started; the flagged body was cached and `/v1/assets/native` cycled between clean and stale/degraded responses with no refresh failure behind it. Client requests are unchanged: served at once, flagged when past the bound (#4360).

## [v0.134.0] - 2026-11-25

Tested against Stellar protocol 27. No migrations (schema version stays 236). No `pkg/*` break. Behaviour change for API clients: an uncached (cold) read now costs extra rate-limit tokens, and a request that cannot pay is refused with 429 (#4357).

### Added

- **api:** a request that owns a new cold fill pays 5 extra rate-limit tokens before it claims a refresh slot: contract detail, asset holders and account state, including the issuer lookups behind asset pages. An uncached `GET /v1/contracts/{contract_id}` makes three such reads and costs 16. A request that cannot pay gets the existing 429 and no read starts; an asset page whose issuer lookup is refused is served with its stale flag. Cached reads, stale serves and requests joining another fill still cost one (#4357).
- **ops:** `stellarindex_ratelimit_cold_fill_refused_total` counts the refusals, and the ticket `stellarindex_ratelimit_cold_fill_refusals_sustained` fires when they exceed 1 a second for 30 minutes, with a runbook section; the ticket alert count is 334 (#4357).

### Changed

- **api:** a contract page whose activity panel failed or was refused is flagged degraded and not cached; a request that joined a refused owner's fill retries once on its own budget (#4357).

### Fixed

- **api:** a cold asset holders or account state read waits for a refresh slot until its request deadline or 4 seconds, with a bounded queue, before answering 503; it answered 503 at once when its class was full. A stale read still skips rather than waits, and concurrency against ClickHouse is unchanged (#4356).

## [v0.133.0] - 2026-11-24

Tested against Stellar protocol 27. No migrations (schema version stays 236). No `pkg/*` break. A host role run from this tag replaces the API's ClickHouse user definition and hot-reloads it, with no restart (#4351).

### Added

- **ops:** `stellarindex_aggregator_window_truncated` raises a ticket when a pair and window keep serving a truncated trade window for 30 minutes; `stellarindex_aggregator_window_truncated_total` gains `pair` and `window` labels, and the warn line names the pair, window, rows used and `covered_from` (#4353).

### Changed

- **api:** a cold `GET /v1/contracts/{id}` read waits for a refresh slot until its request deadline or 4 seconds, with a bounded queue, before answering 503; concurrency against ClickHouse is unchanged (#4354).
- **api:** the asset holders cache, the op-type stats panel on `/v1/operations` and the native liquidity-pool listing flag a serve as stale only past the refresh TTL plus the refresh budget, or after a failed, timed-out or skipped refresh. The OpenAPI text for `/v1/assets/{asset_id}/holders` states the real bounds: 300 seconds for a scanned board, 70 minutes for a rollup board (#4352).

### Fixed

- **ops:** the API's ClickHouse user is granted `SELECT` on `stellar.*` and `system.parts` explicitly, so the count rollup plan is readable and account reads stop falling back to the source scan; the role verifies the grants as that user (#4351).

## [v0.132.0] - 2026-11-24

Tested against Stellar protocol 27. No migrations (schema version stays 236). No `pkg/*` break. Galexie moves to v30.0.1: a host role run from this tag restarts the exporter and needs `galexie_restart_ack=true` (#4348).

### Added

- **supply:** when a watched SEP-41 contract's served history starts after its first mint, burn or clawback in the lake, the aggregator names the contract, the ledger window and both `projected-rebuild` commands, sets `stellarindex_aggregator_sep41_history_gap_from_ledger` and raises `stellarindex_aggregator_sep41_history_gap` until the totals check passes (#4345).
- **ingest:** a reviewed list of judged non-token contracts relabels a listed contract's supply-decode skip to `judged_non_token`, which stays counted and does not alert; an unlisted contract with the same shape still alerts, and an alerting skip now names its contract in the log (#4345).

### Changed

- **api:** a cached read is flagged stale only when it is past its refresh TTL plus the refresh budget, or its last finished refresh failed, timed out, panicked or was skipped. This now covers markets, pools, sparklines and token supply (#4340); RWA history, premium history, membership and the assets view, protocol detail, the asset list page, the asset catalogue and explorer contract stats (#4343); and the account state and wealth caches behind `/v1/accounts` (#4344). The OpenAPI text for both account routes states the rule.
- **api:** a page built from another cache is judged on its inputs: the asset list page ages from when its rows were observed, and the RWA assets view, value history and premium history re-judge their membership set and catalogue rows on every serve (#4343).
- **ops:** `verify-served-values` checks the served USDC total supply against the issuer's public stablecoin figure for the Stellar chain, skipping (never passing) on a missing, unparseable, non-positive or day-old figure (#4341).
- **ops:** galexie is pinned to `galexie-v30.0.1`, with its version string and binary sha256 (#4348).

### Fixed

- **api:** the RWA assets view and a replayed asset list page are flagged as soon as the asset catalogue's refresh fails, where they previously judged its rows by age only (#4346).
- **api:** the asset-id resolve fallback no longer claims the asset list refresh slot without starting a refresh (#4343).
- **ops:** the `trades-by-account` freshness bound is 9 days, matching a rollup that writes once per 7-day trades chunk; the 72-hour bound fired `stellarindex_data_source_stale` for about four days of every seven (#4342).
- **tests:** the VWAP/TWAP source-exclusion test pins one closed window (#4346), and three explorer stale-serve tests hold the refresh until the stale serve is observed (#4347).
