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

## [v0.92.0] — 2026-09-27

Nine days, ~1,600 commits since v0.91.0 — the longest gap between two
tagged releases to date (previous max: 4 days). Grouped by area below
rather than itemized; migration 0187 needs an operator re-materialization
pass and is called out first.

### Fixed

- **storage/migrations — price CAGGs restrict to priceable trades; `volume_quote`/`volume_priced` replace `vwap * volume` (migration 0187, OPERATOR ACTION REQUIRED):**
  SDEX can settle a trade with one leg rounded to zero stroops; every
  view dividing `quote_amount` by `base_amount` — `prices_1m/15m/1h/4h/1d/1w/1mo`,
  `twap_1h/1d`, `pools_per_source_1h` — either failed the refresh on a
  zero-base row, priced a zero-quote row at 0, or let a one-leg row skew
  vwap. All ten views are recreated `materialized_only`, filtered to
  `base_amount > 0 AND quote_amount > 0`, and gain `volume_quote`
  (sum of quote over every row) and `volume_priced` (sum of base over
  priceable rows only); `volume` is unchanged. Readers that reconstructed
  quote volume or a volume weight as `vwap * volume` moved to the new
  columns (`OHLCSeries`, `combineDirVWAP`'s feeders, `TimedVWAPsForPair1m`,
  `DailyMarketDays`, `MonthlyUSDVWAPs`, the asset price snapshot, the USD
  FX direct-leg dust floor) — the old formula was wrong on any bucket
  holding a zero-leg trade. **The migration leaves all ten views EMPTY.**
  Deploy order: apply 0187, then re-materialize recent-first, `prices_1m`
  before the TWAP views, each grain windowed and forced (see the
  migration's header for the exact `refresh_continuous_aggregate` calls),
  then restore `pools_per_source_1h`'s real-time flag once it is whole.
  Until re-materialized, price/volume endpoints backed by these views
  read empty.

- **api — `/v1/price?window=` retired (ADR-0018, BREAKING):** the
  parameter dispatched to the aggregator's rolling Redis VWAP cache
  while the default served the closed 1-minute Timescale bucket — one
  query parameter switching consistency surfaces under a single URL,
  which ADR-0018 prohibits. Any value other than the implicit default
  now 400s, at the top of the handler before the reader-nil check; the
  dead windowed-read path and its OpenAPI parameter are removed and the
  three derived artifacts regenerated (#762).

- **api — price-serving correctness sweep:** `?window_days=` now 400s on
  an out-of-range or unparseable value instead of silently falling back
  to the endpoint default; the router's composite quality flags
  (`diverged`/`rerouted`/etc.) now reach a direct-served headline, not
  only a triangulated one; the cached VWAP fallback alias-walks so
  `native` reaches the `crypto:XLM` composite; a sub-cent price decline
  renders `0.00`, never `-0.00`; a positive fiat cross-rate is never
  served as `"0"`; point-in-time price read failures answer 5xx, not a
  no-data 404; readiness now fails when a continuous aggregate is still
  serving its open bucket; the 404 price-withheld response is now
  declared in the spec for `/chart` and `/history/since-inception`
  (previously only `/price` and its stream), so a spec-driven client no
  longer treats the undeclared code as endpoint-not-found and fails over
  to the wrong route (#1145).

- **aggregator/price-alerts/divergence:** the price-alert evaluator's
  VWAP now applies the same decimals correction the price path does, and
  an alert crossing built off a stale VWAP bucket is rejected;
  alert-pair resolution walks every asset alias spelling; every alert's
  cooldown floor is raised to 300s (migration 0181); router
  corroboration no longer widens the freeze source-count leg or gets
  miscounted against a pivot leg that is two different USDs; a frozen
  composite keeps its provenance/meta alive for the whole hold; each
  divergence reference now persists its own observation time (migration
  0186) and exports its outcome as its own gauge, and an unpublished
  firing streak restarts correctly after an evaluation gap.

- **explorer/api — SSE streaming resilience:** streams resume from the
  last event id and drop out-of-order frames instead of restarting cold;
  replay is budgeted with headroom rather than clamped to full capacity;
  the route cache re-checks auth on an already-open stream; go-redis
  `PubSub.Channel` drops are counted, not only logged; SSE subscriber
  drops, open streams, cap refusals and hub topics are metered; Hub
  topic idle-TTL and max-topic-count are wired from
  `api.streaming.topic_idle_ttl`/`max_topics` config instead of running
  the compiled-in defaults (15m / 4096) with no operator override
  (#1128).

- **api — status, diagnostics and protocol metadata:** `/v1/status`
  heartbeats filter Prometheus's `up{...}` series on `up==1` before
  reading its timestamp, so a crashed indexer/aggregator/api process's
  failed-scrape sample no longer reads as a fresh heartbeat and the
  outage now goes stale within the existing 60s rule; `/v1/diagnostics/ingestion`
  and `/v1/sources/{name}/health` both gate on their shared background
  snapshot's age instead of serving it forever once its refresher
  goroutine dies to a panic, falling back to an inline build past the
  staleness window; the `token` protocol category is dropped from the
  spec's enum — no registry entry has ever emitted it.

- **security/auth:** credentialed CORS and the CSRF write-bypass are
  scoped to a narrower allow-list; the login throttle key now folds
  `+tag`/Gmail-dot re-spellings so they share one bucket; a login code is
  charged against its attempt cap before comparison, and only the newest
  mint is accepted; a stored WebAuthn sign count is never lowered; the
  magic-link send is detached from the request context and its format
  characters escaped; closing an account now revokes every member's
  dashboard session, not just API keys; every key/alert/webhook create
  is retry-safe via `Idempotency-Key`; a Postgres DSN missing its `//` is
  refused before `lib/pq` quotes it silently wrong; the signup
  verification link is built from configured `external_base_url`, never
  the request's client-supplied `Host`; the signup-race reaper is
  asserted against the Postgres account store instead of the dashboard
  bundle, so a Postgres-only deployment with no `[api.dashboard]`
  configured now actually reaps its orphaned accounts; the in-process
  login-throttle's per-email spend is gated behind a passing per-IP
  check, so requests against an already-exhausted IP can no longer burn
  through a distinct email's own budget.

- **webhooks:** customer-webhook delivery signatures (v2) now bind the
  delivery id and event type, not just the payload; every event-type
  copy derives from one canonical list; delivery-lane panics are
  recovered per endpoint instead of taking the lane down; webhook URLs
  are capped at 2048 bytes / counted in code points, bound to port 443,
  and unique per (account, url) (migration 0180); `DELETE` on a missing
  webhook now 404s.

- **money/supply/canonical:** `sep41_transfers` refuses a negative or
  missing amount on every write path, backed by a database CHECK
  (migration 0174), and refuses a `Void` address topic outside
  `set_admin`'s optional admin; `audit_log` is enforced append-only by a
  database trigger refusing UPDATE/DELETE/TRUNCATE (migration 0179); a catalogue asset is
  ranked and priced from the newest observation in both directions;
  `crypto:XLM` ranks equal to `native` for pair orientation; the static
  XLM reserve fallback is labelled, bounded and fails closed; SAC-balance
  and claimable-balance seed passes now record what they established as
  evidence, not just which table they read (migrations 0183/0184);
  `Amount.Scan` refuses SQL `NULL` and `ParseAsset(String())` round-trips
  are pinned; a partially-priced portfolio value renders as a lower
  bound, per the money invariant.

- **rwa/pricing-guard:** a curated RWA reference is refused when its
  declared ISIN contradicts a constant-NAV binding; each admitted
  contract counts as its own issuer in `summary.issuers`; `/v1/rwa/history`
  stops at the last closed day; a flagged issuer's unregistered SAC price
  is withheld; the curator's own company name is no longer served as
  `issuer_directory_name`.

- **clickhouse/timescale — ingest and backfill correctness:** `ch-rebuild`
  now fails on an invalid trade other than a one-side-zero fill and
  reports/batches what actually landed; every windowed CH backfill
  refuses an implicit full-history run; alias spellings are folded
  before markets/pools orientation; soroban 24h USD volume is valued per
  trade, not per bucket; `contract_instance_changes` is now keyed per
  transaction (`(contract_hash, ledger_seq, tx_hash, change_index)`) so a
  same-ledger, same-contract upgrade in two transactions no longer
  collapses into one row — **r1 needs the separate operator migration**
  `deploy/clickhouse/contract_instance_changes_tx_key.sql` plus an
  `ch-instance-backfill` re-key, not covered by `stellarindex-migrate`;
  `stellar.transactions` gains fee-bump columns (`inner_tx_hash`,
  `fee_account`, `fee_bump_fee`, `inner_result_code`) and `GET /v1/tx/{hash}`
  resolves the inner hash; the sandwich detector's lake tx-order lookup
  (`TxIndexReader.TxIndexes`) chunks its `stellar.tx_hash_index` IN-list
  at 500 keys instead of 2000 — a wide scattered IN-list against that
  table's unmerged parts was hitting `MEMORY_LIMIT_EXCEEDED` roughly 50
  times/day — and a chunk that still fails now counts on
  `stellarindex_mev_lake_order_lookup_skipped_total` instead of only
  logging, so a sustained failure rate is visible on a dashboard rather
  than only in logs.

- **completeness/coverage:** the priceless-popular tripwire and the
  transitive-price/explorer paths now ask the shared substance/scam gate
  instead of re-deriving their own withheld verdict, closing gaps where
  each disagreed with `/v1/assets`; a failed directory read is now
  surfaced as `directory_unavailable: true` instead of reading wire-
  identical to "not listed"; `/v1/changes` reads closed buckets only, so
  an in-progress minute can no longer set a permanent ATH/ATL; per-source
  genesis ledgers are locked in step between the reconciliation catalogue
  and the gap detector.

- **ansible/deploy:** Postgres DSNs are percent-encoded; `prometheus_port`/
  `alertmanager_port` vars replace hardcoded 9090/9093; MinIO bucket
  provisioning routes through `minio_buckets`; the chainlink divergence
  RPC URL no longer leaks into `stellarindex.toml`; `stellar-core-auto-upgrade`
  drops privileges after its apt install; indexer/aggregator deploys gate
  on a schema-head `/readyz`; release tagging rejects a multi-line tag and
  gates on green CI; `ch-lake-backup`'s install/enable tasks carry their
  own ansible tag instead of inheriting `postgres`/`pgbackrest`/`backup`,
  so it can be applied alone; Prometheus rule changes under
  `configs/prometheus/rules.r1/**` now apply to r1 on push instead of
  waiting for the next full binary deploy — the gap that left r1 frozen
  at 232 loaded rules against a 286-rule repo for 9 days — and the
  rule-drift check runs daily instead of Thursdays-only, opening (and
  auto-closing) one tracking issue on drift instead of just going red
  with nobody watching.

- **monitoring/redis — 2026-09-16 outage follow-through:** that incident
  (Redis MISCONF for 2h53m, 2,034,194 requests 503'd, ~1.78M
  usage-counter increments lost) is corrected in its own postmortem — it
  was the same root-fs fill breaking Redis's own bgsave and tripping the
  rate limiter's fail-closed path API-wide, not three routes failing on
  Postgres reads alone — and left three observability gaps, now closed:
  `stellarindex_redis_command_errors_total{class}` counts every failed
  Redis command by reply-class on every client (a go-redis v9 hook in
  `redisclient.Build`); `stellarindex_usage_units_dropped_total{counter}`
  counts billable/detail usage units lost to a failed counter write; and
  `stellarindex_ratelimit_fail_closed_total{limiter}` counts requests
  503'd once a limiter's fail-open dwell time elapses, across the main
  rate limiter, the failed-auth throttle and the signup per-IP throttle
  (its lint `KNOWN_INERT` placeholder is dropped now that it has a
  producer). Alert-rule fixes: `stellarindex_ratelimit_fail_open` is
  rewritten from a 10-minute sustained-rate condition the limiter's own
  30s fail-open dwell made structurally unreachable to a 15-minute
  increase threshold, paired with a new `stellarindex_ratelimit_fail_closed`
  page; `stellarindex_redis_write_rejected_oom` widens past `err="OOM"`
  to also match `READONLY` and `NOREPLICAS` (deliberately not `MISCONF`/
  `EXECABORT`, which `stellarindex_redis_writes_blocked` already pages
  on — now documented, so the omission reads as a decision, not a gap);
  `stellarindex_textfile_producer_stale`'s 24h catch-all no longer
  false-fires on three producers with a different expected cadence
  (`ops_job_*.pidN`, `ops_job_backfill`, `restore_drill*` — the last gets
  its own 35d rule); and four previously-uncovered host-substrate
  signals (`stellarindex_md_array_degraded`, `stellarindex_filesystem_readonly`,
  `stellarindex_nvme_critical_warning`, `stellarindex_wal_archive_stale`)
  gain rules and runbooks.

- Additionally: several hundred more fixes across dashboard-auth,
  ClickHouse metadata decoding, forex/oracle sources (Chainlink, Binance,
  Kraken, Bitstamp, RedStone, reflector), sorocredit, the explorer web
  frontend, and CI/test infrastructure — see the commit history since
  v0.91.0 for the full list.

### Added

- **dashboard:** webhook delivery log.
- **dashboard-auth:** passkey and dashboard-key credential changes are
  audited.
- **ops:** `stellar-core` auto-upgrades from apt with tip verification
  and rollback; ClickHouse lake gains a native off-site backup; an
  emptied clean-slate window can be filed with its verdict.
- **monitoring:** alerts on a permanently dropped projector row and on
  i128 overflow.

### Changed

- **monitoring — alerting surface expanded:** two new rule files,
  `api-security.yml` (CORS-wildcard-in-prod and related) and
  `cross-region.yml` (`stellarindex_cross_region_divergence`,
  `_fetch_errors`, `_check_stale`), plus substantial rule growth across
  `storage.yml`, `api.yml`, `divergence.yml`, `ingestion.yml`,
  `freeze-lifecycle.yml`, `projector.yml`, `supply.yml` and most of the
  remaining rule files, mirrored in both the r1 and multi-host trees
  (65 files, +5,044/-606 lines). Also: a Tier E staleness alert, and
  `stellarindex_alertmanager_not_notifying` now aggregates across webhook
  receivers instead of firing per-instance.
- **perf:** `lint-docs.sh` 64s → 22s and `lint-docs-test.sh` 769s → 44s;
  `usage_daily` rollups sweep a bounded window and upsert only changed
  rows, as chunked multi-row statements; `markets`' fresh-close read uses
  `last()` instead of an ordered aggregate.
- **config:** the CoinGecko/Massive/Dune keys are declared in the config
  schema; a retired key is tolerated at boot instead of hard-failing;
  the closed-bucket Redis channel is operator-configurable; substance
  floors are held to the window's reachable bounds.
- **migrations — 24 more besides 0187** (0163–0186): freeze-event window
  ladders; disarm the cctp/rozo replay double-count; widen `prices_1m`'s
  refresh start offset past the CH-catchup worst case; a $0.01 notional
  floor on the TWAP chain; `usage_daily` retained 12 months; restore
  `asset_supply_history` compression where 0030 left it disabled;
  `price_source_contributions` gains its aggregation window; account
  directory overrides record who and why (`override_reason`/`override_by`);
  widen remaining hypertables' chunk intervals; pin `materialized_only`
  on every served CAGG but the two real-time volume counters; correct
  stored comments on `divergence_observations.status`, `oracle_updates`
  and `ledger_ingest_log`; log a before-image of every `usd-volume-restamp`
  rewrite; record when the cached SEP-1 payload was fetched; one webhook
  per (account, url); delete sandwich/oracle-sandwich accusations whose
  stored evidence doesn't prove opposite-direction brackets.

## [v0.91.0] — 2026-09-18

### Added

- **ops:** `verify-served-values` now diffs `supply.sdf_reserve_accounts`
  against the reserve list SDF publishes — the `accounts` table plus the
  network-upgrade reserve in stellar/dashboard's `common/lumens.js`, the
  source of `dashboard.stellar.org`'s `circulatingSupply` (its API exposes
  program sums only, never the accounts) — on the same daily timer as the
  2% value cross-check, and emits
  `stellarindex_sdf_reserve_list_drift{kind="missing"|"extra"}`.
  `stellarindex_sdf_reserve_list_drift` (ticket, 26 h) alerts on any
  verified difference in both rule trees; a dark or reshaped source is a
  skip (`served_value_skipped{check="sdf_reserve_list"}`), never a verdict.
  Closes tail-triage C4-069: one account SDF adds or retires moves
  circulating supply by well under the value check's 2% tolerance, so a
  stale list was undetectable. New `-config` flag (default
  `/etc/stellarindex.toml`; empty skips the check), passed by the systemd
  unit. Runbook section in `served-value-drift.md`; catalogue row.

- **explorer:** the sponsor / creator cohort pages' flows panel now
  values each month's movement at THAT month's USD price beside the
  live-price figure — "USD then" beside "USD today", as a toggle on the
  value-moved chart. `GET /v1/accounts/{g}/graph/cohort` gains
  `flows.points[].by_asset[].{inflow_usd_then,outflow_usd_then,price_usd_then}`
  and the month point's `inflow_usd_then` / `outflow_usd_then` (the priced
  assets' exact sum, rounded once); every one is omitted — never zero —
  where no USD-quoted market priced the asset that month. "Then" is the
  month's volume-weighted USD price on this index's own markets:
  `timescale.Store.MonthlyUSDVWAPs` folds `prices_1mo`'s USD-proxy-quoted
  rows onto ONE row per (canonical asset, month) across every alias
  spelling (XLM's three forms land under `native`, a registered SAC under
  its classic id) with exact arithmetic; `ch-cohort-rollup` loads it each
  cycle into the new `stellar.asset_month_usd_prices` (+ `_staging`,
  swapped with the cohort tables — `deploy/clickhouse/tier1_schema.sql`
  and the operator mirror `account_cohort_rollup.sql`), and the flows read
  LEFT JOINs it on (asset, month). XLM's own monthly USD row is in the
  table so an asset quoted only in XLM can be priced through it later.
  Operators on an existing deployment re-run step 1 of the
  `account_cohort_rollup.sql` runbook (idempotent DDL) before the next
  cycle; the cycle now installs the `[supply].sac_wrappers` alias
  registry so SAC-quoted months fold onto the classic id the flows carry.

### Changed

- **docs:** `docs/methodology/rwa-definition.md` R4 names all three of
  its bases — it said "one of two" while the code had carried
  `sep1_isin_declaration` since 2026-09-15 — and describes the ISIN arm
  as the code applies it (form-checked `anchor_asset`, admits without a
  class, runs last, sits below R2 and R3). It also states the pre-filter
  contract `rwa.CouldQualify` is under (every asset-side input of every
  R4 arm, never a decision, pinned by a test) and points the
  sibling-recognition assumption at the R3 section that carries it. The
  `curated-rwa-sync.service.j2` header now describes the sync as it is
  — two public Dune query results read by GET, no SQL execution,
  billed by datapoint (`stellarindex_curated_rwa_sync_datapoints_read`),
  stamping `executed_at_unix`, writing `curated_rwa_published_series` —
  in place of the execute-and-poll design it replaced; directives are
  untouched. The alerts catalog and the `curated-rwa-sync-stale`
  runbook were checked for the same leftovers and carried none.
- **explorer/api:** the three follow-ups the #515 verifier listed. The
  creator and sponsor league tables under `/insights` read their board
  shapes from one `src/api/relationTypes.ts`, derived from the generated
  `operations['getAccountCreators']` / `['getAccountSponsors']` types
  alongside the standing panel that already did — the two hand-written
  copies are gone, and deriving them exposed that the spec never listed
  `totals.*`, `coverage.*` or a row's `first_/last_ledger` and
  timestamps as required although the handler emits every one
  unconditionally; the spec now says so. The explorer's `Coin` is the
  generated `Asset` schema outright — every field the hand-typed
  intersection carried has been spec'd since board #33, and the copy had
  drifted the other way, declaring `slug`, `first_seen_ledger`,
  `last_seen_ledger` and `observation_count` required while the handler
  serves each `omitempty`; the consumers that dereferenced `slug` now go
  through `coinSlug()` (falls back to `asset_id`, which `/assets/{id}`
  resolves), the home table draws an absent `observation_count` as a
  dash rather than `0`, and the sparklines take the wire's own nullable
  shape. `sep1ImagesReader` (the SEP-1 logo overlay) and
  `Sep1BoundCurrencyReader` (RWA classic membership) carry the
  compile-time `*timescale.Store` assertion the other optional seams
  have, so a reader that stops satisfying either is a build failure
  rather than a silent opt-out.

### Fixed

- **timescale:** `BatchInsertTrades` lifts TimescaleDB's per-transaction
  decompression cap (`SET LOCAL … = 0`, scoped to the batch's own
  transaction) before its upsert. It is the fallback writer for every
  range whose `trades` chunks are compressed, and an upsert into a
  compressed chunk decompresses whole segments per conflict: on
  2026-09-18 every 5,000-row sub-batch of a Soroban-era SDEX re-derive
  failed with `SQLSTATE 53400 tuple decompression limit exceeded` and the
  writer dropped to one INSERT per row again. The restamp and COPY writers
  already lifted the cap the same way.

- **api/aggregator:** token decimals are resolved from ONE source of truth,
  and a market cap is never computed across two scales (C1-050). The
  aggregator normalised VWAP through `nonstandard_decimals_assets` while
  `GET /v1/assets/{asset_id}` scaled supply by the lake's on-chain
  `decimals()`, and `market_cap_usd` / `fdv_usd` were computed regardless
  of whether the two agreed — a lake read that failed or timed out fell
  back to 7 while the price stayed normalised on the projection's value,
  so an 18-dp token's cap came out 10^11× too large on every lake blip.
  The lake is now the source of truth and the projection its materialised
  view: the aggregator's decimals-guard re-reads every persisted row
  against the lake on each sweep tick and repairs drift
  (`decimalsguard.Guard.Reconcile` — upsert the lake's value, or delete
  the row when the lake confirms 7 dp; a row the lake cannot read is left
  alone). The detail endpoint refuses the cap with a new
  `market_cap_decimals_mismatch: true` reason flag when the two disagree
  at request time, and falls back to the projection's value when the lake
  is unreadable so supply and price stay on one scale. New counter
  `stellarindex_nonstandard_decimals_lockstep_mismatch_total{site,asset}`
  is a third arm of `stellarindex_nonstandard_decimals_correction_failing`.
  Measured on r1 before the change: 9 projection rows, the 6 with an asset
  row all equal to the lake, 0 disagreeing — the defect was structural, not
  a live divergence. Runbook: `docs/operations/runbooks/dex-nonstandard-decimals.md`
  ("Decimals lockstep").

## [v0.90.0] — 2026-09-18

### Added

- **ops:** `stellarindex_sink_undrained_rows_total{sink,kind}` counts the
  rows the Postgres pipeline sink's bounded shutdown drain abandoned
  unwritten, by row and by kind (`trade` / `event`), and
  `stellarindex_ingestion_sink_undrained_rows` (ticket, fires at once)
  alerts on any increase in both rule trees. The indexer upserts the
  ledger cursor per ledger before the sink writes, so a row lost at
  shutdown was a served-tier gap that surfaced only as an ERROR log line
  nothing alerted on — the ClickHouse live-sink half of the same class
  already had its `dropped` counter and rules. Runbook:
  `docs/operations/runbooks/sink-undrained-rows.md`.

### Changed

- **ci/docker:** the six `docker/stellarindex-*.Dockerfile`s pin both
  base images by immutable digest — `golang:1.27-alpine@sha256:cf6fca66…`
  and `gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872…`,
  the multi-platform index digests resolved on 2026-09-18 with
  `docker buildx imagetools inspect`. The tag stays in the reference
  for readability but the digest is what the build pulls, so a
  re-tagged or tampered upstream tag can no longer change what ships
  without a diff here. Closes the `TODO(supply-chain, DEP-low)` left
  by #14, which could not resolve digests offline. Dependabot's docker
  ecosystem (already watching `/docker`) keeps the digests current.
- **explorer:** `GET /v1/accounts/{g}/graph/cohort` prices only the 50
  largest holdings by balance (plus the flow assets) and says so:
  `valuation.price_cap` and `valuation.unpriced_over_cap` count what the
  cap left unpriced. A large cohort carries up to 400 holdings and every
  price is a live read of 40–350 ms, so pricing them all serially spent
  the whole 8 s request budget on prices alone.
- **explorer/api:** the repo's own drift guards applied in two spots the
  2026-09-14→17 commits missed (#515). `Sep1FetchStateReader` — the
  optional seam behind the `not_fetched` / `unreachable` split on
  `/v1/assets/{id}` — now carries the compile-time `*timescale.Store`
  assertion `preciseSupplyReader` already has, so a reader that stops
  satisfying it is a build failure rather than a silent revert of every
  attempted-and-failed issuer to `not_fetched`. The explorer's `Coin`
  takes `listing_reference` / `listing_valuation` from the generated
  `Asset` schema instead of a hand-written copy, and the account
  board-standing panel derives the creator / sponsor bodies from the
  generated `operations` types and reads the rows without `as` casts.

- **ops:** `curated-rwa-sync` reads the totals the curator PUBLISHES
  instead of the per-asset tables it cannot read. The two CSV uploads
  behind the "RWAs on Stellar" dashboard are private to the uploading
  team — Dune refuses a SQL execution over them to every outside account
  ("Uploaded table … does not exist or it is private") — so the arm's
  execute-and-page design could never load a row. The run now GETs the
  latest result of the dashboard's public queries (6961845 "RWA Mcap by
  Month", 6961847 "Mcap by Month by Asset Subclass"), pages to the
  declared row count with strict per-row decoding, and replaces the
  curator's rows per series in one transaction into the new
  `curated_rwa_published_series` (migration 0162). A read bills by
  datapoint and never executes a query, so the textfile's
  `execution_cost_credits` gauge — which would have graphed a cost that
  cannot occur — is replaced by `datapoints_read`, and
  `executed_at_unix` records when the curator's query last ran; the
  `priced` gauge, which counted per-asset prices the run cannot see, is
  gone. Alert rules, their promtool fixture and the runbook follow.
- **api:** `/v1/rwa/assets` `curated` carries a `published` block — the
  curator's latest monthly total (`total_usd`, `as_of`, `executed_at`),
  its split by the curator's subclass labels, the full monthly series,
  the public queries it came from, and `gap_vs_verified_usd` (published
  minus this index's verified reference total, signed). Status
  `published_totals` names the state where the totals answered and no
  per-asset row is readable, and the `basis` prose now says the
  curator's list and prices are private and only its published totals
  are read. Existing fields keep their meaning; the Go SDK, the spec and
  the derived artifacts follow.
- **explorer:** the RWA page's curated panel shows the curator's
  published total, the month it is for, when the curator last computed
  it, the signed gap to the verified figure and the subclass split, in
  place of an empty comparison.

### Fixed

- **api:** the `supply_basis` enums on `Asset` and `RWAAsset` in
  `openapi/stellar-index.v1.yaml` carry `sep41_total_only`, the value
  `/v1/assets/{asset_id}` has served since v0.21.0 for a SEP-41 token
  whose admin balance came back zero — the DEFAULT reading for an
  unconfigured token, not an edge case. The constant was added to the Go
  vocabulary without touching the spec, so the generated docs mirror,
  Postman collection and explorer `types.ts` all published a closed
  union the API did not honour. A spec test now pins both enums to the
  `internal/supply` const block in declaration order, the way the
  `/v1/ohlc` interval enum is pinned to its route table.
- **sources/chainlink:** both Chainlink readers verify each feed's
  scale against the AggregatorV3 proxy's on-chain `decimals()` instead
  of trusting the configured (or built-in 8) value blind. The
  `[divergence.chainlink]` cross-check reference and the
  `[external.chainlink]` `oracle_updates` poller (and its backfill)
  read `decimals()` over the JSON-RPC path they already use, on first
  use and daily: an omitted `decimals` adopts the on-chain value; a set
  value that agrees flows; a set value that disagrees is logged at
  ERROR with both numbers, counted on the new
  `stellarindex_chainlink_feed_decimals_mismatch_total{consumer,pair}`
  and the feed is REFUSED (`price_unavailable` for the divergence
  worker, a per-feed error for the poller) until they agree — a
  cross-check that scales wrongly is a permanent false divergence, and
  a mis-scaled oracle row is worse than none. A failed `decimals()`
  read keeps the last known value with a WARN and retries after 5 min;
  a feed with neither a configured nor a read value is refused rather
  than guessed. r1 runs both readers enabled (EUR/GBP/JPY on the
  cross-check, the six built-in feeds on the poller), every one at 8,
  so no production reading changes; the guard is for the value that
  drifts. `BuildFeedSet` and the divergence constructor no longer
  substitute 8 for an omitted value (the poller's `project` refuses a
  literal 0 as `ErrDecimalsUnresolved`), and the reference takes a
  `Logger` so the verification lines land in the process log.
- **ci:** the weekly ansible-drift verdict's comment-only classifier
  picks the comment token per file type (#519). It stripped from the
  first `#`, `--` or `//` whatever the file, so every URL host
  (`https://…`) and every long flag (`--config-file …`) was discarded
  from both sides of a hunk before the compare, and a changed S3
  endpoint in `pgbackrest.conf`, a changed retention flag in
  `/etc/default/prometheus` or a re-pointed `ExecStart` in a systemd
  unit was reported under a ✅ as "comments only" — the exact hand edit
  on r1 the control exists to catch, across 69 of the role's templates.
  The classifier now uses ONE token chosen from the hunk's host path
  (`--` for `.sql`; `#` for shell, YAML, TOML, systemd units, `.conf`
  and the extensionless `/etc/default`, `logrotate.d`, `Caddyfile` and
  `sshd_config` files the role renders; `//` for Go/TS/JS), treats a
  type with no known convention (ClickHouse `.xml`) as substantive
  outright, and only honours a token that begins a word, so `https://`
  and a `#fragment` inside a URL never read as comments. The fixture
  suite gains the three drift rows above, a URL-fragment row, an
  unknown-type row, and the `.sql`/`.yml` comment-only rows; the five
  drift rows exit 0 against the shipped classifier.
- **ops:** `sla-proof-from-probe.sh` treats a headline cell it cannot
  evaluate — no row for that endpoint in that family, a non-numeric
  value, a NaN or an infinity — as NOT PROVEN, and the window cannot
  read PROVEN while any such cell stands. The refusal was per family
  (a series absent for every endpoint) but the verdict is per cell, so
  one endpoint missing from `p95_max` or lacking an availability
  denominator rendered `n/a` and counted as a pass: the week read PROVEN
  with part of one endpoint's SLA unmeasured, in the exact document the
  refusal exists to prevent. The report now names each unevaluable cell
  and an endpoint with a sample count but no bound stays in the table
  instead of vanishing from it. Not reachable on the real capture
  (every endpoint is in every family), but the first relabel or
  latency-only endpoint would have made it so silently (#513).
- **ops:** `curated-rwa-sync.service` describes itself. Its header, its
  `RUN_TIMEOUT` sizing note and its memory-ceiling note were the
  listing-sync unit's verbatim — CoinGecko, migration 0160,
  `COINGECKO_API_KEY`, a 3.7 MB catalogue — so `systemctl cat` told the
  operator the alert sent there the wrong upstream, key and migration.
  Directives unchanged. The key file is now ONE mechanism everywhere:
  the role renders `/etc/default/curated-rwa-sync` from
  `vault_dune_api_key` when the vault defines it (else installs the
  placeholder once), `root:root 0600` — what r1 has carried since the
  unit shipped and what the runbook and alert already prescribed, where
  the role said `0640 root:stellarindex`. The new vault value is in the
  preflight shell-metacharacter census like every other env-file
  secret, and the runbook names the `medium` tier the code asks for.
  (#518)
- **ops:** `postgresql.conf.j2` renders `min_wal_size = 512MB` again.
  The 2026-09-15 edit templated both WAL lines while sizing only
  `max_wal_size`, and its inline default moved `min_wal_size` 512MB →
  2GB unmentioned; the 09-16 revert restored `max_wal_size` alone, so
  every host rendered `min_wal_size = max_wal_size` and r1 runs 2GB
  today. The comment now states the intended pair (2GB / 512MB, the
  values the 2026-07-03 drift audit proved effective), that the next
  apply is an effective diff on r1 with the Postgres restart handler
  behind it, and how to land it by reload instead. The headroom guard
  the comment pointed at (`postgres_wal_headroom_assert`) does not
  exist; it names the real task now. Nothing applied to any host.
  (#512)
- **rwa:** the prospectus constant-NAV reference carries a review bound.
  Each `rwa.ConstantNAV` binding now derives a `ReviewBy` date from the
  date its issuer page was read plus a documented 90-day interval (a
  CNAV fund's NAV is 1.00 every day until the fund changes regime, and a
  regime change surfaces in the fund's quarterly reporting), and past
  that date `/v1/rwa/assets` serves the row with `stale: true` and a
  `source` saying the binding is due for re-verification. It was the
  one reference on the surface with no staleness mechanism — served
  `stale: false` forever, so a class converted, merged or wound down
  would have stayed at par until someone edited Go. The IB class
  (gBENJI, LU2900381208) binding also cited the AB class's page as its
  evidence; both bindings now cite their own share-class page as the
  issuer's product sitemap enumerates them.
- **api:** `/v1/ohlc?interval=2h|12h|3d|2w` answers 200 again. The four
  widths were routed to the store's re-bucketing read with fold
  literals (`2 hours`, `12 hours`, `3 days`, `2 weeks`) that its
  hand-kept allow-list never learnt, so every request at them failed
  with `outInterval not in allow-list` and a 500 on the public API from
  the day #213 shipped them (launch plan W8-17). The interval ladder is
  now one table, `timescale.OHLCRoutes`: the API's validation and 400
  body, the serving reader's choice of view and the fold allow-list all
  derive from it, so a routed interval cannot be one the store refuses
  (W8-20). Pinned by an executing test over every folded route,
  including 2w's Monday alignment against `prices_1w`, and by a test
  that holds the spec's enum to the table.
- **rwa:** the classic arm's scan pre-filter now reads the declared
  `anchor_asset` beside the code and the anchor type, so an entry that
  declares type `other` beside a well-formed ISIN reaches the
  definition. It read the type and the code only, which dropped
  Franklin's gBENJI, grBENJI and sgBENJI as `no_real_world_instrument_basis`
  before the ISIN arm, the domain-sibling recognition arm or the
  constant-NAV reference ever ran — both v0.89.2 arms shipped green and
  had zero live effect (29 assets, 18 issuers; expected 32 and 21). The
  pre-filter guard test now spans every asset-side input requirement 4
  reads, and a test drives the production filter through the production
  build.
- **rwa:** the classic arm resolves its listing-directory row by the
  same rule `/v1/assets` uses — the `CODE-GISSUER` id first, then the
  Stellar Asset Contract address derived from it — instead of the
  classic id alone. The directory publishes each asset under one form
  with no pattern, so a SAC-listed classic member was refused as
  `reference_not_bound` while the snapshot in hand named its address,
  and a SAC-listed CNAV share class took the prospectus rule over the
  live observation. One resolver now serves both surfaces (#514).
- **rwa:** `recognition` is documented as present on every served row
  — on classic rows `curated_account_directory` or
  `curated_account_directory_via_domain_sibling` — in the Go doc, the
  OpenAPI spec and the derived reference, Postman and explorer types;
  the served `definition.requirements` name the sibling and ISIN
  routes; and the one-entity-per-domain assumption the sibling route
  rests on is stated where the route is defined and in the methodology
  (#520).
- **explorer:** the cohort view labels its contracts BEFORE it prices
  holdings, and the contract → protocol index is built on its own 5 s
  deadline, detached from the request that triggered it. A cohort of 400
  holdings burned the request budget on price reads, labelled its
  contracts on a dead context, and — because the index cached whatever a
  cancelled build returned for ten minutes — served a statics-only map to
  every other root until the TTL lapsed: eight registered Aquarius pools
  read `protocol: null` on every request. An incomplete build now serves
  its partial map but retries within 30 s, logs one line with the entry
  count and the failed sources, and the API's 5-minute prewarm loop
  builds it so no request meets it cold.
- **explorer:** every served dollar on the cohort view is exact
  (ADR-0003, #516): `price_usd` is the price string the reader served,
  and `value_usd`, `total_usd`, `inflow_usd` and `outflow_usd` are
  `balance × price` as rationals rounded once to two places. Through
  `float64`, one unit at `0.015` rendered `0.01` and a `1.10` price came
  back as `1.1`.
- **ops:** the cap67 movements follow daemon's first run starts at the
  lake's first ledger, not below it. Floored at genesis (`-floor-ledger
  1`, the test-net setting) it resumed from ledger 1, which no lake holds
  (every net's lake begins at 2), and the contiguity gate read that as a
  boundary hole forever: both test nets' `account_movements` archives
  sat empty for months while the daemon re-ran a full-lake scan every
  second in silence. The first run now clamps up to `min(ledger_seq)`,
  a long idle run is named in the journal (`idle: start=… contiguous
  tip=… min_present=…`, every 30 idle ticks) and the tick backs off
  (doubling past 30 idle ticks, capped at 30 s, back to the base the
  moment a tick derives). The ansible default for the non-pubnet floor
  is 2 as well, so the config no longer asks for a ledger that does not
  exist.
- **clickhouse:** the creators rollup splits its two creation arms at the
  NETWORK's P23 boundary instead of pubnet's constant.
  `ch-creators-rollup` takes `-config PATH` (the unit passes
  `/etc/stellarindex.toml`) and reads `stellar.movements_floor_ledger`;
  without it the pubnet boundary
  applies. On a reset testnet/futurenet every ledger sits below
  58,762,517, so the classic arm owned all of them and looked for
  `create_account` movements a post-P23-only chain never writes —
  `account_creator_edges` stayed empty and every `created` cohort with
  it, however full the archive.
- **ops:** `curated-rwa-sync` asks Dune for the `medium` execution tier.
  It asked for `small`, which Dune does not name; every run with a key
  configured was refused before the SQL ran (`HTTP 400: This performance
  tier is not available with your subscription`), so the curated arm
  never loaded a row. Verified against the live API: `medium`, `large`
  and the default all execute on the current plan.

- **timescale:** `BatchInsertTrades` sends its rows in parameter-safe
  sub-batches (5,000 rows × 13 binds, under Postgres' 65,535-parameter
  ceiling) and tallies the outcome once across them. A 100,000-row batch —
  the bulk backfill's fallback size — failed with "extended protocol
  limited to 65535 parameters" and dropped to one INSERT per row, which
  is why a 40k-ledger SDEX re-derive chunk took five hours.
- **ops:** every `refresh_continuous_aggregate` CALL the backfill makes
  now runs under a per-CALL `statement_timeout` derived from the window
  it refreshes (5 min per hour of window, floor 10 min, ceiling 4 h),
  set on the one pooled connection that runs it and handed back with
  the connection's previous value. The ops pool is deliberately
  unbounded and the CALL ran on a context with no deadline, so one
  wedged refresh held every `-parallel` worker behind the refresh lock
  until SIGINT; a Go-side deadline alone would not have helped, since
  the driver closes the socket and leaves the backend materialising with
  the view's refresh lock held. When the bound fires the backend cancels
  the statement, the chunk fails with a typed error the log carries with
  the view, the window and the bound, the run continues, and the
  connection is returned usable (W8-19).

## [v0.89.3] — 2026-09-17

### Added

- **explorer:** the cohort's contracts table names a token contract the
  protocol roster does not claim — a Stellar Asset Contract by its
  classic asset's code, a SEP-41 token by its symbol — as `label`
  (`token USDC`) instead of leaving every such row unlabelled.

## [v0.89.2] — 2026-09-17 (tag only — the release workflow's asset upload failed; shipped as v0.89.3)

### Added

- **rwa:** an issuer account the curated directory never listed is now
  recognised when the SAME issuer-bound SEP-1, on the same domain, also
  binds an account the directory does list and does not flag — served as
  `recognition: curated_account_directory_via_domain_sibling`, apart
  from the direct arm. A scam flag on the account itself still refuses
  it, and the arm supplies no instrument claim: the class, oracle-code
  or ISIN arms still have to admit the asset. The case is Franklin
  Templeton's Luxembourg and Singapore share classes (gBENJI, grBENJI,
  sgBENJI — 82.2M tokens, ISIN-declared beside the listed BENJI issuer),
  which were refused for recognition while being named by the recognised
  entity itself.
- **rwa:** a share class whose fund rules fix its NAV — a CNAV money
  market fund — takes that NAV as its reference price when neither an
  oracle binding nor a listing price exists, served as
  `reference.provenance: prospectus_constant_nav` with the ISIN, the
  regime and the issuer's NAV page it was read from. Bound on the exact
  (code, issuer) in `rwa.ConstantNAV`: Franklin's Luxembourg gBENJI and
  grBENJI (EU MMFR public-debt CNAV, NAV $1.00, page read 2026-09-16);
  the accumulating Singapore class stays unpriced by design. This is the
  first reference price on this surface that is independent of both the
  oracle set and any curator.
