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

## [v0.131.0] - 2026-11-24

Tested against Stellar protocol 27. No migrations (schema version stays 236). No `pkg/*` break; `pkg/client` gains lending types and splits `types.go` by domain with no API change (#4279, #4311, #4315, #4321). Not run by the deploy: after rolling the binary, run `stellarindex-ops backfill-external -source coingecko -pair all` once on r1, dry run first, to fill the CoinGecko outage window (#4326); to catch up ledger-upgrade entry changes use `ch-backfill -ledgers FILE` (#4288). Host payload changed: the HA, archive-job and archival-node roles, alert rules and the new optional edge probe, which is off unless a host sets `edge_probe_targets` (#4327, #4304, #4276, #4272). Behaviour changes for API clients: `/v1/oracle/prices` serves a USD-peg book, flagged triangulated, when an asset's own `fiat:USD` book is refused only for being thin, and answers 404 `price-withheld` otherwise (#4336); the holders board is stale only when its rollup is behind its own schedule (#4335); a freeze that holds on buckets the scorer cannot evaluate now escalates to operator review and no longer auto-releases if healthy buckets return (#4337). Behaviour change for operators: the indexer saves its cursor only through ledgers whose rows were written, so a restart after an unclean exit replays from the oldest unwritten ledger (#4333).

### Added

- **api:** `GET /v1/lending/pools/{pool}/top-accounts` ranks a Blend or BLNT pool's largest suppliers and borrowers per reserve, reconciled against the reserve supply and marked as a floor when a missing account could outrank a row (#4311).
- **api:** `GET /v1/lending/pools/{pool}/history` serves pool TVL and borrowed in USD per hour or day for Blend V1, V2 and BLNT pools, each bucket valued at its own price; reserve history gains `supplied_usd`, `borrowed_usd` and the price used (#4315).
- **api:** `GET /v1/lending/pools/{pool}/liquidations` lists user liquidations and bad-debt fills with collateral seized, debt repaid and filler profit in dollars at the fill; BLNT user liquidations are valued the same way as Blend V2 (#4321, #4322).
- **auth:** a delegated key records its ancestors' record hashes at mint; `stellarindex-ops apikey-lineage-backfill` records the chain on existing keys, and a lineage record outlives an idle ancestor's credential; no authentication decision changes (#4275, #4292).
- **ops:** `backfill-external -source coingecko` fills an outage window from the vendor's hourly history without overwriting live rows (#4326).
- **ops:** `ch-backfill -ledgers FILE` and `-list-upgrade-ledgers -dry-run` catch up ledger-upgrade entry changes without walking every ledger (#4288).
- **ops:** an off-host probe of a deployment's public `/v1/readyz` and `/v1/assets?limit=1` edge, with certificate-expiry checks, off by default (#4327).
- **monitoring:** ticket alerts for NVMe health series that vanish, a heavy-job lock that ages, per-drive NVMe vanish, and a decoder writing a money field NULL (`stellarindex_source_amount_degraded`) (#4294, #4313, #4332).
- **ci:** the price-route census fails when coverage moves, and a pre-pull of integration images with a mirror fallback runs before shards (#4316, #4323).

### Changed

- **api:** `/v1/oracle/prices` falls back to a qualifying USD-peg book only when the asset's own book is refused for being thin; otherwise it is `price-withheld`, and an unread peg leg is an error rather than a stand-in price (#4336).
- **api:** price routes refuse stand-in prices; a peg fallback is served only behind a checked peg that passes the substance floor, is within its band and is not excluded (#4336, #4287).
- **api:** source exclusions reach the asset listing and detail, chart routes, price-at, price changes and history, the OHLC kill switch and peg-served oracle and change series; recomputes say so (#4300, #4298, #4293, #4281, #4317).
- **api:** the fiat FX leg of asset prices is judged against exclusions, and declared-peg prices, held assets and metadata are handled on the asset routes (#4331, #4310).
- **api:** the asset and source pages give one verdict per figure, and the asset page shows no withheld or partial figure as exact (#4329, #4308).
- **api:** RWA references are judged against exclusions and stale history is bounded; RWA members, premium days and history report what was left out (#4334, #4314).
- **api:** a rollup-backed holders board is judged against the rollup cadence, 70 minutes, instead of 300 seconds; a board scanned live from the lake keeps 300 seconds (#4335).
- **api:** a response served from a background-refreshed cache is flagged `stale` only when it is past the route's age bound or its last refresh failed, not on every serve after the refresh interval; affects network stats, contract detail and its sibling views, the contracts and operations directories, network throughput and classic supply. Position data keeps a 90-second bound (#4339).
- **api:** classic supply agrees across the listing, detail and supply routes; floor supply, same-market 24-hour change and exact asset ids are fixed (#4299, #4289).
- **api:** peg totals and freeze release are corrected; derived market-cap charts measure both legs; no price fill may replace a withholding it cannot answer (#4287, #4286, #4284).
- **api:** lending liquidation prices refuse a partial price read: the leg is null with `price_unread` and profit is served only when both sides are exact (#4332).
- **api:** a case variant of a code never inherits a declared asset's identity (#4297).
- **api:** unread counts, FDV reasons, unread pools and a 503 are reported as such; explorer positions, movements and wasm reads, and movements and the source page, no longer serve a holed page as complete (#4302, #4307, #4280).
- **api:** `/readyz` fails once Redis is down past the rate-limit dwell and tracks limiter writes; first sign-ins are capped per IP (#4274, #4303).
- **aggregate:** an unscored freeze hold escalates to operator review after `MaxExtensions` unscoreable slides instead of sliding forever (#4337).
- **indexer:** the persisted cursor and observer watermarks advance only through ledgers whose rows were written; `stellarindex_cursor_durable_lag_ledgers` shows the gap (#4333).
- **data:** lake answers the derived tables cannot prove are qualified, seam holes in the lake are found, and retired webhook keys are erased (#4309, #4305).
- **ops:** the root metric-writer hardening is finished on live hosts; the heavy-job lock race, root audit trail and probe writes are fixed; `projector-replay` needs a stated mode (#4312, #4306, #4273).
- **deploy:** HA roles fail closed on TLS, slots, restarts and ACLs, log to the journal and check the whole VIP address; archive jobs are sandboxed; wabt output is capped; the role cannot write a config the installed binary rejects (#4304, #4276, #4272, #4283, #4285).
- **monitoring:** four alerting gaps are closed and fast aborts leave the SLO; unmeasured pairs retry instead of sending; backup and drift gaps are covered; the failed-auth alert is widened (#4278, #4290, #4328).
- **security:** account export is scoped and the explorer honours scale refusals (#4277).
- **client:** `pkg/client` `types.go` is split by domain with no API change (#4279).
- **deps:** OpenTelemetry is bumped to v1.47.0 for GO-2026-6974 (#4330).
- **ci:** the baseline-growth, immutability, migration, secret, replay and lake-read gates judge by the base copy and close blind spots; exclusion-named waiver maps are scoped to test files; rollback passes the immutability gate (#4295, #4291, #4320, #4324).
- **docs:** passages on security, pricing and 14 other claims corrected to match the code; secrets-file copies are ignored (#4328, #4282).

### Fixed

- **api:** `/v1/oracle/prices` no longer serves an unqualified peg book for every withheld reason, and fiat-quoted OHLC bars for tokens with non-standard decimals are adjusted exactly (#4336).
- **api:** oracle, RWA and pool read fixes under the kill switch (#4281).
- **obs:** a per-drive NVMe vanish alert, a webhook row bound and corrected auth docs (#4313).
- **test:** the exclusion cache stub's observed-at instant is pinned, and the account operations page is bounded absolutely in the integration test (#4325, #4301).
