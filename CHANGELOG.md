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

## [v0.138.0] - 2026-11-25

Tested against Stellar protocol 27. No migrations (schema version stays 236). No `pkg/*` break (no change under `pkg/`). No ClickHouse DDL change; an existing host needs `stellar.lending_interest_hourly` from `deploy/clickhouse/lending_interest.sql` (`si-apply-scope: operator`) if it has not been applied since v0.136.0. New metric: `stellarindex_api_degraded_responses_total`. `stellarindex_lending_interest_unexplained_versions` changes meaning (see Fixed).

### Changed

- **api:** `/v1/price/batch` reads the first stage of a request's data as set statements before its rows run: the cached triangulated price, the latest closed 1m bucket and the guard's trailing buckets. A prefetched answer is used only within 2 s and only for the exact key it was read for; anything else is read as before. Requests under 8 ids are not prefetched and only the first 200 ids are. Responses are unchanged (#4387).
- **api:** a `fiat:USD` `/v1/price/batch` also prefetches each asset's 24h-ago anchor bucket and its guard baseline, keyed on the anchor's minute, beneath the per-minute change-24h cache. Responses are unchanged (#4388).

### Added

- **api:** `stellarindex_api_degraded_responses_total{route,reason}` and a `degraded_reason` field on the request log line say why a response was flagged degraded: `refresh_failed`, `refresh_overrun`, `provisional`, `section_dropped` or `other` (#4390).

### Fixed

- **lending:** the interest pass window line reports explained, attributed and unexplained versions separately, with up to five samples of unexplained rows. `stellarindex_lending_interest_unexplained_versions` and the proof report no longer count attributed versions (a Blend V1 interest-auction fill, whose event carries no amount) as unexplained (#4389).

## [v0.137.0] - 2026-11-25

Tested against Stellar protocol 27. No migrations (schema version stays 236). No `pkg/*` break (no change under `pkg/`). Operators of an EXISTING ClickHouse host: `deploy/clickhouse/lending_interest.sql` and `deploy/clickhouse/lending_state.sql` (both `si-apply-scope: operator`) changed in comments and runbook text only, with no DDL change, so nothing needs re-applying before deploying. New operator flag `-interest-max-versions` (default 150,000) on `ch-entry-history`. The hourly interest pass is uninitialised on every host and derive revision 2 replaces revision 1; a host that had initialised it would re-derive from the floor. New metrics: `stellarindex_lending_interest_undecodable_events`, `stellarindex_api_price_batch_prefetch_total`.

### Added

- **lending:** `ch-entry-history -lending-only` has an interest pass that reads every stored reserve version from the lake, folds it into exact hourly rows and writes `stellar.lending_interest_hourly`. It stays idle until an operator runs `-interest-init`, never starts a backfill from the timer, and refuses a pool whose contract code is not listed with its arithmetic (#4373, #4375).
- **lending:** each interest residual is classified against the pool's own same-transaction events (interest-auction fill, gulp, BLNT reconcile loss) and marked explained only on exact equality; an event that cannot be decoded leaves its transaction's residuals unexplained and is counted in `stellarindex_lending_interest_undecodable_events`. Derive revision 2: revision-1 rows prove INCOMPLETE (#4380).
- **lending:** `ch-entry-history -lending-only -prove -interest` is a read-only proof of the stored hourly interest rows against the lake. Optional daily and weekly proof units (off by default) and the alerts `stellarindex_lending_interest_lagging`, `stellarindex_lending_interest_proof_failed` and `stellarindex_lending_interest_proof_stale` cover it (#4375, #4378).
- **api:** a request-scoped price read memo and Redis prefetch seams for `/v1/price/batch`, counted in `stellarindex_api_price_batch_prefetch_total{seam,result}`; a prefetched answer older than 2 s is read again. Nothing fills the memo yet, so responses are unchanged (#4381).
- **timescale:** set forms of six per-key price readers, each returning what its single-key read would; no handler calls them yet (#4384).

### Changed

- **lending:** the interest pass no longer refuses where an operator had no way out: the seed proof runs in chunks of at most 250,000 ledgers, `-interest-max-versions` bounds the derive and the proof and the refusal names it, and a BLNT interest fill with a missing or mistyped `auction_type` is treated as undecodable instead of skipped (#4383).
- **lending:** every interest mode holds the state lock shared and the state pass holds it exclusive, so state rows cannot change under a reading interest pass. A `lending_state` replay with `-from` arms an interest redo that must be re-derived before the pass runs, and seeds are proved against the lake (#4377).
- **explorer:** the static build writes each answered `/v1/price/batch` chunk to disk and reuses it in the second render pass, so each chunk is requested once per build (#4376).

### Fixed

- **api:** an old but healthy cached explorer page is flagged `stale` only, not `degraded`; `degraded` is set for a failed, refused or overrunning refresh, a provisional payload or a failed panel. This covers contract detail, holders, the contracts directory, contract stats and the operations directory; price, supply, market cap, TVL and balance caches are unchanged (#4382).
- **lending:** a `lending_state` replay no longer lets a concurrent interest run seed a reserve from a row before the deleted range, which counted one hour's rate movement twice (#4377).
- **lending:** the interest re-derive delete cutoff uses millisecond server time, matching `derived_at` (#4383).

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
