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

## [v0.124.0] - 2026-11-17

Tested against Stellar protocol 27. No migrations. No `pkg/*` break; `pkg/client` gains response flags and fields. The OpenAPI contract gains `/v1/lending/rates` and additive flags and fields; nothing is removed. Host payload changed: ClickHouse view definitions (`ops_by_source`, `account_activity`, `ttl_live_until`, `contract_instance_changes`), the API and aggregator unit restart limits, the firewall and Caddy templates (rendered output unchanged unless `http_origin_lock_enabled` is set), and the patroni, redis-sentinel and loki roles. Run the role before deploying. After deploying, `stellar.lending_state_hourly` must be emptied and refilled from lending genesis, then proved (`ch-entry-history -lending-only`, `-prove`).

### Added

- **api:** `/v1/lending/rates` lists every Blend and BLNT reserve with supply and borrow APR, utilisation, supplied and borrowed amounts, factors and caps, filterable by asset and protocol.
- **api:** `stellarindex_api_flagged_responses_total{route,flag}` counts degraded and stale responses; a gauge reports the newest day of the contract census.
- **api:** `/v1/contracts/stats` carries `active_90d_through` and is a lower bound when the census is behind.
- **explorer:** protocol tables show asset codes, asset pairs and curated pool names with the address on hover, and amounts in human units.
- **ansible:** an opt-in origin lock restricts ports 80 and 443 to the CDN ranges, listed extra CIDRs and internal addresses. It is off by default.
- **monitoring:** five new ticket alerts, including a stale baseline refresh and an ingestion sink fault drop.
- **ci:** the lake-dedup lint watches all 31 ReplacingMergeTree lake tables and reads whose table name is spliced in; the docs lint rejects a partial-set rollback command and an any-branch release signer.

### Changed

- **api:** `/v1/ohlc`, `/v1/chart` and `/v1/history/since-inception` are cached on the short band, and `/v1/ohlc` returns the price-withheld 404 for a scam-flagged issuer.
- **api:** folded OHLC intervals serve the buckets that start inside the requested window, as the native intervals do.
- **api:** a published token `decimals` is always a scale read on-chain; an unknown scale is absent.
- **api:** the API refuses to start, and `-dry-run` fails, on an unreadable or invalid holds file.
- **ops:** `listing-sync` refuses a run that would prune more than 20% of the listing directory (floor 5) unless `-accept-churn` is passed.
- **ops:** an external trades backfill refreshes the trades aggregates over the span it wrote.
- **ops:** the API and aggregator units keep restarting through a 15-minute Postgres outage.

### Fixed

- **lending:** hourly lending state is derived from the lake's current rows only; superseded row versions stopped the fill and could have produced wrong hours. The fill runs in 100k-ledger windows.
- **pricing:** a stale headline price, a dust-only market set and a single outlier bar no longer reach asset prices, markets and OHLC unflagged.
- **rwa:** the premium is bounded by the published value on both surfaces, and the reference basis names the read that did not answer.
- **assets:** failed, stale and partial reads reach the response flags on the asset listing, detail and verified routes.
- **api:** account positions disclose a capped venue fold, and a DeFindex row is not closed on a zero net flow; lending earnings serves a capped read as a lower bound instead of a 500.
- **api:** an unchecked peg is flagged rather than served as holding; lending pools, markets and issuers flag a failed or truncated read; a history cursor older than the defaulted `from` is not skipped.
- **auth:** a minted key is bounded by its minter, key validation fails closed when the key store is down, and an interrupted account erasure is finished.
- **lake:** the ops-by-source proof sees fee payers, and the backfill runbooks fail closed.
- **ops:** `data-freshness.sh` fails a probe on a SQL error; `pg-chunk-scan.sh` samples correctly; a balance diagnostic no longer counts superseded row versions.
- **ansible:** the patroni, redis-sentinel and loki roles fail safe on a bad bootstrap, an unauthenticated Redis and an unbounded Loki.
- **monitoring:** five alerts that could not fire now can.
- **ci:** bypasses in the migration gates are closed, and four lints are tightened.
- **explorer:** CSV exports defuse spreadsheet formulas in text cells; floor, derived and stale figures are marked on asset and RWA pages.
- **docs:** 35 operator procedures (rollback, release verification, recovery, credential rotation, privacy requests, runbooks) are corrected against the code they describe.
- **tests:** two intermittently failing tests are deterministic.

## [v0.123.0] - 2026-11-17

Tested against Stellar protocol 27. Two migrations, both additive on `blnt_events` (0225, an index on the user column; 0226, a nullable `tx_index` column). No `pkg/*` change. The OpenAPI contract gains seven lending paths and breaks none. Host payload: a new ClickHouse table `stellar.lending_state_hourly`, a `lending-state` unit and timer (not enabled by default) and one alert rule. After the deploy an operator rebuilds BLNT so existing rows gain their `tx_index`: `stellarindex-ops projected-rebuild -source blnt -from 65134154 -to <blnt projector cursor> -write`.

### Added

- **api:** BLNT V3 pools, reserves and positions are served as their own protocol on `/v1/lending/pools`, `/v1/lending/pools/{pool}/reserves` and the account positions endpoint.
- **api:** `/v1/lending/pools/{pool}/reserves` carries each reserve's configuration, the pool configuration, the rate modifier and a USD price; `/v1/lending/pools/{pool}/config-history` lists every configuration change.
- **api:** `/v1/lending/pools/{pool}/backstop` (size, shares, queued withdrawals, per-tier balances for BLNT) and `/v1/lending/pools/{pool}/auctions` (liquidation, bad-debt and interest auctions with their fills).
- **api:** `/v1/lending/pools/{pool}/reserves/{asset}/history` and `/v1/lending/pools/{pool}/backstop/history` serve hourly state decoded from the lake. Only hours the lake has fully covered are served, so a served point never changes.
- **api:** `/v1/lending/pools/{pool}/emissions` serves emission rates and expiry per reserve side and for the backstop, with claim totals. Blend V1 emission storage and Blend V2 per-pool backstop claims are named as unavailable, not estimated.
- **api:** `/v1/lending/accounts/{account}/earnings` serves interest earned and paid, backstop gain and emissions claimed per account. A figure is exact only when the balance folded from events equals the on-chain balance, no auction moved it, the projection is not behind the chain and the source's history is verified from genesis; otherwise it is withheld with a reason and the raw components are still returned.
- **lake:** `stellar.lending_state_hourly` keeps the verbatim storage entry of every Blend and BLNT pool and backstop key per hour changed; `ch-entry-history -lending-only` fills it and `-prove` checks it against current state.
- **explorer:** lending pages are protocol-aware and show pool configuration, the rate model, configuration history, the backstop, auctions and reserve history charts.

### Changed

- **sources:** each BLNT event row stores its transaction's apply position in the ledger, so same-ledger configuration changes are ordered as they were applied.

### Fixed

- **tests:** two ClickHouse test helpers declared twice in one package broke the build of that package's tests on main.

## [v0.122.0] - 2026-11-16

Tested against Stellar protocol 27. One migration (0224, a new table; nothing existing is altered). No `pkg/*` change, no OpenAPI change. Host payload: the default source list gains `blnt`, and the supply observers and asset map gain the BLNT asset and its SAC, so apply the role after the binary. After deploying, replay two sources: `projector-replay -source comet -from 65134000` (two newly gated Comet V1.1 pools) and `projector-replay -source blnt -from 65134154`.

### Added

- **protocols:** BLNT V3, a Blend-derived lending protocol, is listed as its own protocol (`blnt`) with its factory, pools, backstop, emitter and two backfill contracts. Contributed by @levinson.
- **sources:** a `blnt` source decodes every BLNT V3 event into typed columns in a new `blnt_events` table: pool, user, asset, backstop tier and i128 amounts, with the rest of each payload under the contract's own field names. The 52-event ABI is declared as data and checked against one real event of every kind seen on chain.
- **sources:** the BLNT/USDC and BLNT/XLM Comet V1.1 pools are in the Comet identity gate and WASM audit, with real-event fixtures.
- **assets:** the BLNT classic asset and its SAC are in the verified currency list and the supply observers.

### Fixed

- **ops:** `ch-entry-history -totals-only` starts a first run at the lake's first ledger. It asked for ledger 1, which no lake holds, so the sponsorship totals could never begin.
- **ops:** the restore drill counts distinct ledgers in its lake window. It counted rows, so a freshly re-derived window read as twice its size and failed a sound restore.

### Changed

- **ops:** the test-net KVM host accepts SSH keys only.

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
