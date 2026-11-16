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

## [v0.121.1] - 2026-11-16

Tested against Stellar protocol 27. No migrations, no config keys, no host payload, no `pkg/*` change.

### Fixed

- **api:** `/v1/status` answered 200 with an empty body on a host whose price routes had no successful request in five minutes (seen on testnet and futurenet at v0.121.0). A latency quantile with no observations is now treated as no measurement. A response body that cannot be encoded is a 500 problem document, never an empty 200.

## [v0.121.0] - 2026-11-16

Tested against Stellar protocol 27. No migrations. No `pkg/*` break; `pkg/client` gains `includes_unread` on the envelope flags. New config keys, both with safe defaults: `api.register_max_per_hour` (60) and `api.register_disabled` (false). Host payload changed: the archival-node role (Caddy access-log redaction, the `wasm-drift` service and timer), alert rules in both rule trees, and scripts under `scripts/ops`; these reach a host with the role converge, not with the binary. After the converge on r1, start `wasm-drift.service` once. The explorer deploy's shadow-Worker delete step has not run yet; it first runs on the next manual explorer deploy.

### Changed

- **api:** `/v1/price/stream` accepts only a `window_seconds` that something publishes (the aggregator's windows, plus 60 where streaming pairs are configured); any other value is a 400 `invalid-window` that lists them. It used to open a silent stream.
- **api:** `/v1/register` has an hourly cap across all callers, an off switch and an audit row.
- **api:** `/v1/status` measures latency over the price routes its targets are set for, so the reported p95 and p99 drop where slow explorer reads used to count. The SLA probe times the whole response body, so its latency figures rise slightly.
- **api:** `/v1/sources` names an `include` section it could not read under `includes_unread`; a sparkline ends at its read hour. The 24h change anchor is read behind the price withholding gates.
- **auth:** an account with no email keeps its last passkey (409). A plaintext API key in the revoke path is refused and redacted. A delegated key lists the monthly quota that auth enforces.
- **streaming:** a `stream_gap` is sent when events were lost before the stream (the aggregator link reconnected or dropped a message, or polls failed across closed buckets) and on a resume from before the last restart. An auth or quota outage no longer ends an open stream as revoked.
- **positions:** DeFindex vault shares are served as net flows, not a share count.
- **explorer:** partial data is labelled: an incomplete MEV detection window, a ledger's capped transaction list, a stale embed price, a price whose poll stopped succeeding, an unfinished ledger, and a stalled lake behind a cached read. The integrity warnings the server sets on a composed price are shown.
- **ci:** a PR that removes a published path, schema, property or enum value, or adds a required input, fails unless a commit carries an `OpenAPI-Break:` trailer.

### Fixed

- **ingest:** a factory child's gate admission is retried until its row is written. A contract removed from a curated set stops being trusted. Venue streamers get their own sink lane. An event the sink abandons at shutdown names its ledger. A polled price update with an implausible timestamp is dropped. A live tail waiting on a hole ends with an error that names it.
- **projector:** a replay rewind voids give-ups, a given-up row's retryable fault is held, and a carried poison output holds the cursor below its own ledger.
- **ops:** the archive trim deletes a hot file only when the cold tier holds the same bytes, and refuses a cold tier that is the hot tier. An archive check that verified nothing fails. The lake backup survives a dropped poll and bad retention. The D3 cutover pauses lake writers below the tip. The phaseD backfill drivers record the end a window ran to. `ch-entry-history` re-derives remove rows earlier runs wrote. `wasm-drift` runs daily.
- **monitoring:** new alerts for an external poller that never succeeded, a source's decode-failure share, stalled or dropped customer webhook deliveries, a failing FX fixings ticker, and (r1) a lake backup that never wrote its textfile. Projector loss alerts count evicted groups and gate drops. A silent oracle asset keeps its alert series across an indexer restart.
- **explorer:** the og image rate limit keys IPv6 on the /64 and no longer resets itself. The icon proxy caches by icon. A cold contract WASM fill takes a refresh-gate slot. Shell crashes and errors thrown outside render are reported. The shadow Worker is removed and test nets resolve their own API.
- **edge:** the access log redacts an API key in the request path.
- **deploy:** the config-apply baseline ranks a pre-release below its release.

## [v0.120.0] - 2026-11-16

Tested against Stellar protocol 27. Migration 0223 (replaces the audit-erasure function; catalog only, no table rewrite). No `pkg/*` break; `pkg/client` gains `unread` on the price batch envelope. No config change. Host payload changed: the API, indexer and aggregator units and eight archival-node job units (a timer tick no longer starts a stopped datastore), and the r1 Prometheus rule tree (two alert descriptions).

### Changed

- **api:** a failed price read is no longer a miss: `/v1/price` and the tip answer an error, and `/v1/price/batch` lists the ids it could not read under `unread`. A markets list missing a failed leg is flagged `truncated`.
- **api:** an RWA class or issuer total that is missing members is flagged as a lower bound. Aquarius DEX TVL is marked stale when its source stops, curated pools the lake cannot read are counted as missing, and a withdrawn figure is not served from cache.
- **api:** a holders board read from the rollup carries the rollup cycle's time as `as_of`, omits `as_of_ledger`, and is flagged `stale` once that time is more than 300s old.
- **auth:** the staff kill switch revokes keys under `auth_backend=postgres`. An inherited monthly quota is never unmetered, and a revoke evicts the Postgres key cache. Lowering or clearing a rate-limit override lowers the keys minted under it.
- **privacy:** erasing an account also removes its webhook hosts, minted-key slug and passkey names from the audit log.
- **ingest:** every lake read has a 60s deadline, so a hung storage request fails and is retried instead of holding the live tail or a backfill. `stream_path_total` reports `hot` where it reported `sdk`.
- **explorer:** the asset page states identity only where it is proven, and five panels no longer state more than the data behind them.

### Fixed

- **forex:** a feed that keeps re-serving an old bar is not treated as live, and a held series does not rewrite the day it was fetched on.
- **coinbase:** a product the venue refuses raises the rejected-subscription gauge. **bitstamp:** the history backfill walks from an off-grid start and refuses a body with no candles.
- **alerts:** a price alert that delivered nothing does not start a cooldown. A stalled customer webhook endpoint holds its own lane, not the queue.
- **ops:** a backfill window counts as done only through its recorded end and only while the lake still holds it. `fx-history-backfill` leaves the live worker's last 7 days alone. The SLA proof holds each endpoint to the window's coverage floor, the DNS perimeter check reads records tag by tag, and two deploy checks stop passing on no evidence.
- **clickhouse:** two side-by-side rebuild runbooks cover the MV-creation ledger, catch up with a margin after cutover, and close the gap on a post-cutover rollback.
- **docs:** the config reference says what the failed-auth limit does: past the cap a rejection answers 429 instead of 401; it does not stop a lookup or a correct guess.
- **ci:** a lake read in a double-quoted Go string fails `lint-lake-dedup`.

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
