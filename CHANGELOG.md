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

- **`/v1/price` stamped a frozen held value as observed at read time
  (RLT-357):** every surface serving the aggregator's VWAP cache (the
  fallback chain, `?window=`, the frozen held value, the tip, and the
  asset headline's triangulated tier) set `observed_at` to the request
  time, while a freeze keeps the held value alive for its whole hold —
  up to ~35 minutes — without rewriting it. Both cache writers now store
  a `vwap:<base>:<quote>:<window>:observed_at` stamp (the closed bucket
  the value's window ends at, the same instant the SSE stream carries)
  in the value's MULTI/EXEC, the freeze keep-alive extends it with the
  value, and the API serves it. A cached value with no readable stamp is
  not served. Deploy the aggregator before, or with, the API: until it
  has written stamps, the API's cache-backed prices miss.
- **migrations — divergence row status no longer claims to be the flag:**
  the stored comment on `divergence_observations.status` said the API's
  divergence flag is "any reference firing" and that the threshold is
  per-(reference, pair). A row is one reference against the worker's
  single `threshold_pct`; `flags.divergence_warning` additionally needs
  `min_sources_for_warning` answering references, a median breach or
  zero agreeing references, and a 5-minute persistence debounce.
  Migration 0173 re-issues the comment; catalog-only, and its down
  restores 0019's string verbatim.
- **storage — `sep41_transfers` refuses negative or missing amounts on
  every path (T090):** the ch-rebuild COPY writer
  `CopyMergeSEP41Transfers` validated nothing, so a full-history
  re-derive could store a negative transfer amount the per-row writer
  refuses. Both writers now share `validateSEP41TransferRows`, and
  migration 0174 adds `sep41_transfers_amount_check` as the database
  backstop. The migration decompresses every chunk first: on
  timescaledb 2.26.4 the CHECK fails with a corrupted-plan error over
  two or more compressed chunks, which is what reverted the first
  attempt.

- **ops — `usd-volume-restamp` keeps a before-image of what it
  overwrites:** the restamp rewrote `trades.usd_volume` in place and left
  only its run generation behind, so undoing a bad run meant re-deriving
  the span. Migration 0175 adds `usd_volume_restamp_log`; every tier's
  write now copies each target row's prior `usd_volume` and
  `derive_generation` into it in the UPDATE's own REPEATABLE READ
  transaction and refuses to commit when the two row counts differ. The
  undo statement is in 0175's header; its down refuses while the log
  holds rows.

- **ops — per-source genesis ledgers locked in step (#898):** the
  reconciliation catalogue and the gap detector's
  `DefaultGapDetectorTargets` each restate every source's genesis ledger,
  and nothing failed when one was corrected without the other. A new test
  requires each catalogued source's genesis to equal the gap detector's
  earliest floor for that source, and both to equal the package's
  exported constant where one exists (blend, blend_backstop, sorocredit,
  sushiswap_v3, upshift). The gap table keeps literals because storage
  may not import `internal/sources`.

- **explorer / lake — fee-bump transactions and per-op failure reasons
  (#1063):** `stellar.transactions` gains `inner_tx_hash`, `fee_account`,
  `fee_bump_fee` and `inner_result_code`, and a second materialized view
  indexes a fee bump's inner hash in `stellar.tx_hash_index`.
  `GET /v1/tx/{hash}` now resolves the inner hash. Transaction summaries
  carry a `fee_bump` object (payer, inner hash, inner fee bid, inner
  result), and `max_fee` on a fee bump is the payer's bid, which bounds
  `fee_charged`. Each `op_inner` operation in the tx detail now carries
  `inner_result`, decoded from the stored result XDR (e.g.
  `payment_underfunded`). **Deploy ordering:** apply
  `deploy/clickhouse/transactions_fee_bump.sql` before the indexer and
  api binaries. Columns fill going forward only, so a historical fee bump
  has no `fee_bump` object until its range is re-derived.

- **clickhouse — `contract_instance_changes` keyed per transaction
  (T356/T377):** the instance timeline behind
  `/v1/contracts/{id}/code-history` and the wasm-hash lookup was
  `ORDER BY (contract_hash, ledger_seq, change_index)`, and `change_index`
  restarts on every transaction, so two transactions writing one
  contract's instance in the same ledger merged into one row — a
  same-ledger upgrade could vanish and the survivor was the last insert,
  not the ledger-final executable. The key is now
  `(contract_hash, ledger_seq, tx_hash, change_index)`; the MV and
  backfill carry `tx_hash` and `intra_ledger_seq`, and the indexed reads
  (and the legacy `ledger_entry_changes` scan) order a ledger's writes by
  `intra_ledger_seq`. Against a table still on the old key the reader
  probes the shape and keeps the old order instead of failing. **r1 needs
  the operator migration** `deploy/clickhouse/contract_instance_changes_tx_key.sql`
  (side table, `ch-instance-backfill -table contract_instance_changes_v2`
  to genesis, rename cut-over, API restart); `CREATE IF NOT EXISTS` does
  not re-key the existing table.

- **aggregator — the priceless-popular tripwire asks the substance gate
  whether a price is withheld (GH-906):** it re-derived "withheld" as
  trailing-24h volume below $1,000 per asset, one of the gate's three
  floors and without the alias union, so it paged on assets the gate
  correctly withheld and stayed silent on real gaps. It now asks
  `pricingguard.AssetSubstanceVerdict`, which the `/v1/assets` listing
  also uses, with the same policy and USD pegs; an unmeasured verdict
  still pages.

- **api — transitive price asks the scam gate on both legs (#847):**
  `transitivePriceFor` gated only substance, so a directory-flagged hop
  whose wash volume cleared the floor priced `/v1/assets/{id}` with
  `price_basis: transitive` while `/v1/price` refused the hop itself. It
  now refuses when the asset or the hop is scam-withheld, via the
  package's pair-aware `scamWithheld`.

- **api — explorer account and contract views: a failed directory read
  was wire-identical to "not listed" (GH-579):** `directoryFor` returned
  nil on a read error, so a `#malicious`/`#unsafe` label could go unseen
  with nothing saying the lookup did not run. Both views now carry
  `directory_unavailable: true` when the read fails (additive, omitted
  on a successful read).

- **api — `/v1/price/stream` spec and comments described a frame nobody
  emits (GH-751):** the example showed `as_of` 42 s after
  `observed_at` and a `flags` object on the 300 s series; the aggregator
  stamps each event with the closed bucket's end and the bridge emits it
  as both timestamps, without flags. The example, the description (the
  window each event covers, and that a 3600/86400 series advances once
  per minute as overlapping trailing windows) and the redispub comments
  now match the wire, and a test pins the example to the bridged frame.

- **api — `/v1/changes` served the open minute and ungated prices
  (GH-757):** the change-summary read admitted the in-progress
  `prices_1m` bucket (`bucket < now`), so a fat-finger print in the
  filling minute became `current_value` and, through the upsert's
  GREATEST/LEAST ratchet, a permanent ATH/ATL. It now reads closed
  buckets only (in SQL and again in the worker), refuses to upsert a
  newest point with no positive price, and the handler withholds the
  row with the `price-withheld` problem whenever `/v1/price` would
  withhold the market (flagged issuer, or below the substance floor).
  A new guard fails on any pair-bound `prices_1m` read without a
  closed-bucket predicate.

- **api — `AccountActivity.trades_total_since` and `AccountTrade.signer`
  documented (#573):** both fields were on the wire but absent from the
  spec, so the generated types and spec-driven clients could not see
  them — and `trades_total_since` changes `trades_total` from all-time
  to "since this date". Both are now in the spec (with that meaning
  stated on `trades_total`), and the handler-vs-spec field test covers
  the two explorer structs.

- **api — `Account.tier` enum no longer lists `anonymous` (#572):**
  `/v1/account/me` and `/v1/account/keys` 401 an anonymous caller before
  an Account is built, so the value was documented but unreachable. The
  enum is now `[apikey, sep10, operator]`, and the spec test derives it
  from the `auth.Tier` constants minus `TierAnonymous` while a second
  test holds the 401 premise that justifies the exclusion.

- **api — `bridge` source class accepted by `/v1/sources?class=` and
  listed in both spec enums (#571):** the registry serves `cctp` and
  `rozo` as `class: "bridge"` and `/v1/methodology` describes the class,
  but the `?class=` allow-list rejected it with a 400 whose message named
  four of the six values it did accept, and the `?class=` and
  `source_classes[].name` enums stopped at six. The allow-list now
  carries every `external.Class`, the 400 detail is rendered from it,
  and `TestSourceClassSurfacesAgree` pins the constants, the allow-list,
  the served glossary and all three spec enums together.

- **admin — account closure revokes keys and is terminal (GH-809):**
  `status: closed` on `PATCH /v1/admin/accounts/{id}` was the same
  code path as suspension, so a later edit back to `active` brought
  every credential back live. Closing an account now revokes every live
  Postgres-backed API key (reason `account closed`, evicted from the
  auth cache, counted in the audit row as `keys_revoked`), and any
  later non-`closed` status gets a 409 `account-closed`. Account
  erasure and data export (`DELETE /v1/account`,
  `GET /v1/account/data-export`) are still not built.

- **platform — api_keys reads bounded by the active set (GH-766):**
  revoked keys are kept forever, and `ListForAccount` read an
  account's whole key history on every mint quota check, every
  dashboard key list and every admin suspend or tier-clamp eviction.
  The mint check now runs `CountActiveForAccount`, the clamp and
  eviction paths read `ListActiveForAccount`, and
  `GET /v1/dashboard/keys` returns every active key plus the 100 most
  recently created revoked keys, with a new `revoked_truncated` flag
  when older ones were omitted. A package test fails on any
  `FROM api_keys` query without a LIMIT, aggregate, unique-key lookup
  or active-set filter.

- **api / price stream — clock-skew drops and silent pubsub retries are
  now visible (#753):** the Redis subscriber labels a future-dated event
  `future_observed_at` and a >24 h one `stale_observed_at` instead of
  `malformed`, seeds every outcome at construction, and the new
  `stellarindex_api_price_stream_not_delivering` ticket fires when the
  aggregator publishes but the API fans out nothing for 15 min. `Run`'s
  doc no longer claims a Redis failure ends it: go-redis v9 retries
  internally. Docs: `price-divergence` runbook states the real
  `flags.divergence_warning` rule (#826).

- **monitoring — every /sla target now has an alert at the published
  figure (#741):** added `stellarindex_sla_probe_p99_breach` (> 500 ms)
  and `stellarindex_sla_probe_availability_breach` (< 99.9 %) to both
  rule trees, and lowered the `/v1/price` freshness page from 180 s to
  the published 150 s bound. The multi-host rule file's timer pointer now
  names the real unit. `sla_figure_consistency_test.go` fails when a
  published target has no matching alert threshold or a probe family is
  selected by no rule.

- **auth / notify — one canonical spelling per inbox (#736):**
  `/v1/auth/login` and `/v1/auth/verify-code` now reduce the email
  through `notify.CanonicalRecipient` (the `mail.ParseAddress` addr-spec
  `/v1/signup` and `/v1/register` already used) instead of a hand-rolled
  `@`/`.` check. `"x" <a@b.com>` used to be stored, mailed and
  provisioned as a separate account from `a@b.com`, and
  `a@b.com,c@d.com` passed the gate. `notify` now refuses any
  `Message.To` element that is not already canonical. Signup and
  register additionally reject addresses over 254 bytes and dotless
  domains, as login always did.

- **notify — a client disconnect no longer aborts transactional mail
  (#735):** `ResendSender.Send` runs the provider POST on a context
  detached from the caller's cancellation, bounded by its own 10 s
  timeout. A login or signup request aborted after its token row or
  reservation was written used to cancel the send and count it as
  `result="failed"`.

- **api / price stream — one series per connection, one frame per
  bucket (#752):** the aggregator prices XLM as both `native` (SDEX)
  and `crypto:XLM` (CEX) and publishes each on its own topic, and
  `/v1/price/stream` subscribes to every alias spelling, so with a CEX
  connector enabled a client got two `price_update` frames per bucket
  from two independent series. Each connection now follows one series:
  the caller's own spelling first, falling back to an alias only once
  the preferred one has published nothing for `cachekeys.VWAPMaxAge`,
  the read order and horizon of `/v1/price?window=`. Closed-bucket
  events now carry a `producer_id`, and the API subscriber forwards
  one event per (topic, bucket end), dropping a second aggregator's
  copy or a late older bucket (counted as
  `stellarindex_api_stream_subscribe_total{outcome="duplicate"}` and
  logged with both producer ids).

- **api / price stream — closed-bucket events must name canonical
  assets (#754):** the Redis→Hub subscriber now parses `asset` and
  `quote` with `canonical.ParseAsset` and requires the canonical
  spelling, and rejects a self-pair. A forged event on the channel
  previously had any non-empty string echoed to SSE clients as
  `asset_id` and used as the Hub topic key; it is now counted as
  `malformed` and dropped.

- **alerting — a permanently dropped trade tickets at any rate (GH-612):**
  `stellarindex_ingestion_persist_drop` now includes
  `source_insert_errors_total{kind="trade"}`, so a low-rate trade drop on
  sdex or an external source (the dispatcher path, which never reaches the
  projector's `sink_permanent` alert) is no longer silent below the coarse
  0.1/s `insert_errors` line. A trade retry abandoned on shutdown or the
  projector's cycle timeout — cursor held, row re-derivable — now counts
  as `kind="trade_abandoned"` instead of sharing `trade`, so the label
  means "row gone" and the tripwire does not fire on an abandon.

- **projector — a newly enabled source starts at its genesis, not ledger 0
  (GH-567):** `projector.Source` gains `Genesis`, set for `upshift`,
  `sushiswap_v3`, `sorocredit` and `blend` from each package's verified
  first-event ledger. A source with no cursor row starts there (raised to
  the lake floor in CH mode) instead of crawling ~85 h of ledgers that
  cannot hold its events. `TestProjectedSourcesDeclareGenesis` makes every
  new projected source either declare one or be listed as a lake-floor
  crawler. `projector-replay` with a `-from` the cursor has not reached
  now says "nothing to rewind" and how far the cursor has to go, instead
  of "already at ledger N".

- **migrations — hypertable index and CAGG re-materialization lints
  (#856, #865):** `scripts/ci/lint-migrations.sh` now fails a
  `CREATE INDEX` on an existing hypertable that lacks `IF NOT EXISTS`
  or `SET LOCAL lock_timeout` (the 0150 shape), and a migration that
  recreates a continuous aggregate `WITH NO DATA` without naming a
  refresh for every recreated view. The downs of 0115 and 0147 were that
  second shape: their headers said "see the up", and 0115's up leaves the
  `prices_1m` back-fill that `twap_*` depends on to a prose "walk
  backwards" line. Both downs' headers
  now carry the full ordered refresh sequence (header-only edit, baseline
  refreshed). The 0150 register row records the operator step for its
  in-transaction `trades_signer_idx` build.

- **ops — ClickHouse maintenance and Phase-D backfill scripts stop
  reporting success on failure (#794):** `recompress-lec.sh` and
  `recompress-others.sh` now call ClickHouse with `--fail-with-body`,
  check every query, and exit non-zero without a `DONE`/`*_COMPLETE`
  line when an `OPTIMIZE` or a partition is skipped or fails.
  `recompress-others.sh` reads each table's own
  `max_bytes_to_merge_at_max_space_in_pool` before raising it to 500 GB
  and restores that value (or `RESET SETTING` when it was unset) on
  success, failure and TERM/INT/HUP, instead of pinning a hardcoded
  150 GiB. `phaseD-backfill.sh` and `phaseD-range.sh` stop non-zero after
  `PHASED_MAX_ATTEMPTS` (default 3) consecutive failures of one window
  instead of retrying it every 30 s forever. `lake-dedup-driver.sh`
  refuses any table argument outside the six lake tables it is for,
  since the name is spliced into SQL and the log path.
  `scripts/ops/ch-maintenance-fail-closed-test.sh` pins all of it and
  runs in `verify.sh` with the lake-dedup and D3 rebuild self-tests.

- **docs / D3 runbook — reproject from v1's floor, not 38,000,000
  (#793):** the launch plan's D3 step now computes the reproject start
  from `min(ledger_seq)` of the served `ledger_entries_current` and says
  that `cutover` refuses a v2 that does not cover v1.
- **api — listing valuation keeps the ADR-0011 observation (#531):**
  `listing_valuation` took the larger of the lake-flows total and the
  row's own `circulating_supply` whatever that reading was. When the row
  carried a supply observation, a lake over-count replaced it, for
  example BLND about +11.5% under `supply_basis: lake_flows`. The lake now
  replaces the row's reading only when that reading is a provable floor
  (trustline sum) or absent. An observation, or a reading with no basis,
  is multiplied as served.

- **supply — SEP-41 genesis seed rebuilds the fold it sits on (#596,
  #1086):** `seed-sep41-genesis -write` now re-derives the contract's
  rollup fold under the seeded floor in the same transaction, on every
  run. Before, it only zeroed the fold when the floor moved. A contract
  already seeded over a floor-0 fold kept double-counting its
  pre-boundary band, and re-running the seed could not repair it. A
  seed that did zero the fold put the serving read onto the unbounded
  per-contract aggregate until the aggregator re-folded. Every fold
  writer now runs under a 10 s `lock_timeout`, so an aggregator pass
  that meets a seed's held row yields instead of stalling every later
  contract in the pass. The rows already poisoned on r1 still need one
  re-run of the seed after deploy (docs/operations/v1-launch-plan.md
  §2.6).
- **completeness / `/v1/coverage` — vacuous verdicts and the denominator
  (#607):** a source whose reconcile targets all hold no served rows
  (expected ∅ = served ∅, or no targets at all) now publishes
  `projection_ok: false` with a `no evidence` detail. It no longer
  publishes a `true` that looks like a real proof. `/v1/coverage` gains
  `unverified_sources`, which lists each audited source with no verdict
  row. `total_sources` now counts those sources, so a first-pass failure
  shrinks the numerator instead of vanishing from both sides.

- **completeness — `contract_events` census on `-ch` (#806):** the
  substrate axis proved only `stellar.ledgers`, while recognition and
  projection read `stellar.contract_events`. A dropped or unrestored event
  partition therefore still published `lake_complete: true` and
  `recognition_ok: true`. Every run now compares each Soroban-era
  `contract_events` partition's active row count with the
  `soroban_event_count` recorded in `stellar.ledgers`. A short partition
  fails `substrate_ok` and `recognition_ok` for each event-reading source
  whose range it touches.
- **migrations — `usage_daily` retained 12 months (#1282):** migration
  0167 attaches a 12-month retention policy to the per-account usage
  rollups, which 0071 kept forever while their Redis source expires at
  35 days. Same horizon as `api_usage_events` (0027); armed on apply,
  and drops nothing yet because the table dates from 2026-07. The
  usage-rollup-backfill tests now name the package that runs them.

- **api — `/v1/account/usage` gains `billable` (#1278):** each row now
  carries the request units the monthly quota counts (ok + 4xx),
  derived identically on the per-endpoint rollup shape and the legacy
  per-day shape. `requests` includes 5xx on the rollup shape but not
  on the legacy one, so it never reconciled with a quota 429's
  `month_to_date`; the spec now says to sum `billable` for that.
  Additive field; `pkg/client.UsageRow` gains `Billable`.

- **api — monthly quota meters price lookups, not HTTP calls (#1275):**
  `/v1/price/batch` now advances the monthly-quota counter and the
  per-endpoint usage counters by one request unit per de-duplicated
  asset id, matching its per-minute rate-limit charge. A 1000-id POST
  used to cost one unit of the quota, so a 1M-unit key could resolve
  10⁹ prices. Handlers price a request through
  `middleware.ChargeUsage`; every other route still costs one unit.

- **ops — continuous-aggregate refresh policies (#561):** new config
  assertion `caggs_have_refresh_policy` enumerates every continuous
  aggregate and fails when one has no refresh job. The timescale-jobs
  probe enumerates the jobs, so a CAGG whose policy was dropped vanished
  from `stellarindex_timescale_cagg_stale` and was flagged only for the
  day `stellarindex_timescale_cagg_refresh_missing` remembers it (and
  never, for a CAGG created without one); it now raises
  `stellarindex_config_assertion_failed` for as long as it lasts.
- **rwa — curated published totals (#638):** the curator's split is a
  separate query execution from its total, and `/v1/rwa/assets` now
  serves its own `curated.published.by_subclass_executed_at` instead of
  implying the total's `executed_at` covers both; a split whose query
  has not run in 7 days is withheld beside a fresher total. New ticket
  alert `stellarindex_curated_rwa_published_stale` selects
  `stellarindex_curated_rwa_sync_executed_at_unix`, which no rule read:
  it fires when the curator's query has not re-executed in 72 h while
  the sync itself stays healthy.
- **divergence — below-quorum refresh:** a refresh that cannot
  reach `min_sources_for_warning` references no longer rewrites the
  pair's cached `warning_fired` to false, restarts its persistence
  streak or re-arms the `divergence.firing` webhook latch. It carries the
  last evaluated verdict forward (the API reports it with
  `divergence_checked=false`), so a transient reference error no longer
  produces a false all-clear followed by a duplicate webhook. The API's
  alias walk treats that carried-forward warning as standing, not as a
  verdict: a checked verdict under another spelling of the asset wins,
  and only when none exists is `divergence_warning=true,
  divergence_checked=false` served.
- **auth — operator-minted keys inherit the identifier's monthly
  ceiling (#1239):** only `POST /v1/account/keys` carried a monthly
  ceiling onto the key it minted; `POST /v1/admin/keys` and
  `stellarindex-ops mint-key` build their request without one, so a key
  an operator minted for a metered customer's identifier was persisted
  with `monthly_quota: 0` — unmetered, while billing the same
  per-account counter as the customer's capped keys. The inheritance now
  lives in the Redis key store's `Create`, which the admin, CLI,
  self-service and signup mint paths all call: a request without a
  ceiling takes the most generous ceiling the identifier's existing
  Redis credentials carry, so a mint can neither lift nor tighten the
  plan, and a new identifier (or one already holding a live unmetered
  key) still mints without one. A read failure fails the mint. The
  `key.mint` audit row and the CLI's audit output now record the
  ceiling issued. The rotation-resets-the-counter half of #1239 was
  already closed by per-account metering (RLT-404). Still open: the
  store sees only Redis records, so an account whose ceiling exists
  only in Postgres (no Redis mirror or child key) still gets an
  unmetered operator-minted key; and a self-service child of a
  Postgres-backed key is written to Redis with its parent's resolved
  ceiling and read back without the account override cascade, so a
  later change to that override, up or down, does not reach it.

- **dashboard auth — credential changes are audited (#765):** adding or
  removing a passkey, and minting or revoking a key from
  `/v1/dashboard/keys`, now append `passkey.register` /
  `passkey.delete` / `key.mint` / `key.revoke` rows to `audit_log` with
  the session, IP and user agent, as `/v1/account/keys` and
  `/v1/admin/keys` already did. A passkey sign-in refused on a
  clone warning or a ceremony replay appends a `passkey.clone_warning` /
  `passkey.login_replay` row and increments the new
  `stellarindex_passkey_login_refusals_total{reason}`; the new
  `stellarindex_passkey_clone_warning` alert tickets on a clone warning.
  Lost rows count on `stellarindex_admin_audit_write_failures_total`
  under four new `passkey_*` surfaces.

- **aggregator — source contributions keep their window (GH #763):**
  the contribution sink dropped `ContributionRecord.Window`, so the
  5m/1h/24h breakdowns of a pair landed in `price_source_contributions`
  indistinguishable and "latest row" returned whichever window ran
  last. Migration 0169 adds nullable `window_seconds` plus a
  window-aware unique key beside 0026's primary key (old-binary-safe),
  the sink writes it, and `InsertPriceSourceContributions` refuses a
  row without a whole-second window. 0169 also declares a 90-day
  retention policy, shipped disabled as 0156's is, and re-issues the table and `bucket`
  comments to describe the per-tick write they get. Dropping the old
  key and `SET NOT NULL` is a later release's migration.

- **directory — operator override records its reason (GH #858):** an
  operator override of a false-positive scam flag republishes a flagged
  issuer's price but recorded no justification. Migration 0170 adds
  `account_directory.override_reason` with a CHECK that an
  `operator-override` row carries a non-blank reason and an upstream row
  none; `stellarindex-ops directory-override -clear-scam-flag` now
  requires `-reason TEXT` and stores it. Pre-existing override rows are
  backfilled with a named placeholder.
- **explorer — directory label names an operator override (GH #858):** a
  label whose served `source` is `operator-override` was still attributed
  wholly to the StellarExpert directory. `DirectoryLabel` now badges it
  "flag lifted on review" and says the upstream scam-class flag was
  reviewed as a false positive and removed; the OpenAPI `DirectoryInfo.source`
  description names the value.
- **account / dashboard — plan surfaces show the enforced rate limit,
  not the tier ceiling (GH-1074):** `/v1/account/me` now serves
  `account.rate_limit_per_min` / `monthly_request_quota` — what auth
  enforces on a key minted without explicit limits, account overrides
  included — and the staff views serve the same as
  `effective_rate_limit_per_min` / `effective_monthly_quota`. The per-key
  cascade moved onto `platform.Account` so auth and every view resolve it
  from one place. A partner comped to 5,000/min now reads 5,000, not
  100,000; the tier number is labelled "Plan ceiling". Login no longer
  byte-truncates a User-Agent into invalid UTF-8 (GH-1303).
- **api — global price ladder freshness and honest authority labels:**
  `/v1/external/assets/{slug}`'s `vwap_native` tier now has a 10-minute
  freshness ceiling like `aggregator_avg`; an older VWAP yields to a
  fresh aggregator or triangulated price and is served (with its own
  `price_as_of`) only when neither exists. `price_authority` gains
  `reference_rate` (fiat FX rates), `identity` (USD) and
  `onchain_listing` (Stellar-only listing prices), which were all
  stamped `vwap_native`. The fiat price is the stored NUMERIC
  `inverse_usd` text instead of a float64 rendering, on the detail,
  listing market-cap and market-cap chart paths.

- **api — explorer stroop fees are strings (breaking wire change):**
  `base_fee` and `base_reserve` on ledger views and `fee_charged` and
  `max_fee` on transaction summaries (`/v1/ledgers*`, `/v1/tx/{hash}`,
  account transactions) are now decimal strings, like `total_coins`
  and `fee_pool` beside them (ADR-0003). A new test fails any
  `internal/api` response field with a monetary JSON name that
  marshals as a number.

- **sources — chainlink round dedup (RNC26):** the poller now marks a
  round as emitted only after its oracle update is built. A round whose
  projection failed (unresolved decimals, malformed answer) was
  previously marked anyway and never emitted until restart. Successful
  rounds are unchanged; sdex's `ErrUnknownClaimAtomType` godoc now
  matches the decoder (Q092, comment only).

- **docs / CHANGELOG — dangling `#948` citation in the "6 more detail
  surfaces" BreadcrumbList entry corrected (RSWP-055):** the entry
  credited the prior `/assets/{slug}` + `/markets/{pair}` BreadcrumbList
  work to `#948`, which was that work's issue number at the time; `#948`
  has since been reassigned to an unrelated, currently-open issue (an
  ECB fallback finding), so the citation no longer dangled — it resolved
  to the wrong thing. Removed the citation rather than guess a
  replacement number; `scripts/ci/lint-docs.sh`'s stale-reference check
  now guards against it reappearing.
  the existing JSON-LD on `/assets/{slug}` and `/markets/{pair}` —
  expands SEO coverage from 3 → 9 detail pages.

- **docs / CHANGELOG — dangling ref (RSWP-136) — CHANGELOG's default
  Chainlink feed map entry cited an internal PR number from before it
  existed as a real GitHub issue; that number is now a live, unrelated
  issue (`lint-doc-links` silently skipping undecodable markdown
  files), so the citation silently resolved to the wrong thing instead
  of 404ing. Dropped the citation; `scripts/ci/lint-docs.sh`'s
  stale-reference check now guards against it reappearing.

- **docs / CHANGELOG — dangling PR 845 citation in the rc.21
  `/sources` 24h-trade-count entry corrected (RSWP-050):** the entry
  credited its `?include=stats` opt-in to that PR number twice; the
  number has since been reused by an unrelated live issue (filed
  2026-09-18, about explorer `buildFetch` bypass), so the citation no
  longer pointed at a 404 — it pointed at real but unrelated content,
  which is worse. Removed both citations rather than guess a
  replacement number; `scripts/ci/lint-docs.sh`'s stale-reference
  check now guards against it reappearing.

- **docs / CHANGELOG — dangling `#888` citations in the navbar mobile
  menu, /signin, and /v1/currencies entries corrected (RSWP-054):**
  all three credited their preceding placeholder to "#888"; that
  number now resolves to an unrelated, currently-open issue (Timescale
  job-failure alert arithmetic), so the references pointed at real but
  unrelated content instead of just dangling. Removed the citations
  rather than guess a replacement number; `scripts/ci/lint-docs.sh`'s
  stale-reference check now guards against it reappearing.

- **docs / CHANGELOG — dangling issue 971 citation in the rc.23
  Massive.com forex-provider entry corrected (RSWP-057):** that number was
  cited as the originating issue; it has since been reused by an unrelated
  live issue (a `RedisAPIKeyValidator.statusCache` eviction finding), so
  the citation no longer 404s — it silently points a reader at unrelated
  content, which is worse than a dead link. Removed the citation rather
  than guess a replacement number; `scripts/ci/lint-docs.sh`'s
  stale-reference check now guards against it reappearing.

- **docs / CHANGELOG — dangling `#975` citation in the rc.25
  `/v1/currencies` entry corrected (RSWP-059):** the number was a stale
  issue reference that has since been reused by an unrelated live issue
  (a streaming-docs-vs-hub drift finding), so the citation no longer
  pointed at a 404 — it pointed at real but unrelated content, which is
  worse. Removed the citation rather than guess a replacement number;
  `scripts/ci/lint-docs.sh`'s stale-reference check now guards against it
  reappearing.

- **sources — decoder hardening (Q072, RLT-115):** sorocredit counts a
  settlement whose amounts leg cannot be parsed in the new
  `stellarindex_source_amount_degraded_total{source,field}`; decoded rows
  are unchanged. soroswap-router no longer turns a swap deadline at or
  above 2^63 into a bogus near-epoch timestamp; the deadline is left
  unset instead. Rows already ingested with such a deadline keep the bad
  value until the contract-call range is re-derived with
  `stellarindex-ops ch-rebuild -contract-calls`.

- **docs / CHANGELOG — dangling PR 1132 citation in the rc.5x
  `/v1/coins/{slug}` entry corrected (RSWP-093):** the entry credited its
  case-insensitive-XLM-intercept companion to that PR number; it didn't
  exist at the time and has since been assigned to an unrelated,
  currently-open issue, so the reference resolved to the wrong thing
  instead of just dangling. Removed the citation rather than guess a
  replacement number; `scripts/ci/lint-docs.sh`'s stale-reference check
  now guards against it reappearing.

- **docs — CHANGELOG's sac_wrappers entry misattributed to an unrelated
  live issue (RSWP-063):** the v0.5.0-rc.30 `[supply.sac_wrappers]` entry
  cited PR 1002 as one of its sources; that number didn't exist at the
  time and has since been assigned to an unrelated, currently-open issue,
  so the reference resolved to the wrong thing instead of just dangling.
  Removed the reference; `scripts/ci/lint-docs.sh` now guards against it
  reappearing.

- **docs / CHANGELOG — dangling `#1004` citation in the rc.30 `known_issuers`
  entry corrected (RSWP-065):** the number was a stale PR reference that
  has since been reused by an unrelated live issue, so the citation no
  longer pointed at a 404 — it pointed at real but unrelated content,
  which is worse. Removed the citation rather than guess a replacement
  number; `scripts/ci/lint-docs.sh`'s stale-reference check now guards
  against it reappearing.

- **docs — CHANGELOG's sac_wrappers entry misattributed to an unrelated
  live issue (RSWP-062):** the v0.5.0-rc.30 `[supply.sac_wrappers]` entry
  cited PR 1001 as one of its sources; that number didn't exist at the
  time and has since been assigned to an unrelated, currently-open issue,
  so the reference resolved to the wrong thing instead of just dangling.
  Removed the reference; `scripts/ci/lint-docs.sh` now guards against it
  reappearing.

- **ops / classic-movements-backfill — -resume could skip a widened -from
  range, and a same-window CAP-0038 claim could resolve to "unresolved"
  (Q216, T137):** `MaxAccountMovementLedger` reports the highest ledger
  anywhere in `[-from,-to]`, not a contiguous frontier from `-from`; a run
  that widened `-from` below a prior, narrower run's start still found that
  prior run's tip and jumped straight to it, silently never revisiting the
  newly-widened earlier range. `-resume` now also checks the new
  `MinAccountMovementLedger` and only trusts the jump when the range's data
  genuinely starts at `-from`. Separately, `classicMovementsHandleCAP0038Op`
  builds its claimable-balance-create movements via
  `classicmovements.DecodeCAP0038Revocation` directly (bypassing
  `dec.Decode`), so a claim against a balance CAP-0038 auto-liquidation
  created earlier in the SAME window always fell through to "unresolved" —
  the create was in that window's own batch but nothing consulted it before
  the ClickHouse fallback. `classicMovementsAttemptWindow` now decodes the
  entry-changes surface before resolving pending claims, and
  `classicMovementsResolvePendingClaimableBalances` checks a batch-local
  balance_id index as a free fallback.
  **T355 (deferred, needs coordination):** `cbLookupCreatesQuery`'s
  `idx_cb_balance_id` bloom-filter skip index is declared in both
  `internal/storage/clickhouse/account_movements.go`'s `accountMovementsDDL`
  and `deploy/clickhouse/tier1_schema.sql` (already applied to r1 via a
  one-off `ALTER TABLE`), and is disabled (`use_skip_indexes = 0`) by the
  only production query that touches its indexed expression, kept only for
  a documented-but-nonexistent future point-lookup caller. Removing it
  correctly needs both DDL copies changed in lockstep (to avoid schema
  drift between a fresh deploy and r1) plus a follow-up operator
  `ALTER TABLE ... DROP INDEX` against the live database — outside a single
  fixer's file scope and outside "commit code, never run a migration
  against a real database." Left as-is pending that coordination.

- **ci / agent-attribution guard now runs unconditionally in CI, not only
  an opt-in local hook (F167, F172):** a prior attempt at this wired the
  check into `ci.yml`'s `doc-checks` job, which never runs at all for a
  docs-only push — `ci.yml` skips the entire workflow via its top-level
  `paths-ignore` on `**.md`/`docs/**` before any job-level condition is
  evaluated, so exactly the case an attribution marker in a doc would land
  as went unchecked. New `scripts/ci/lint-attribution.sh` (self-tested by
  `scripts/ci/lint-attribution-test.sh`) instead runs from a new
  `self-attribution` job added to `.github/workflows/commit-identity.yml`,
  which already carries no path filtering and always fetches full history
  for exactly this reason. It scans every tracked doc for an agent/vendor
  self-attribution marker and scans this push's own commit range for the
  same marker in a commit message. Before this, the only thing that ever
  checked either case was `scripts/dev/install-hooks.sh`'s opt-in
  pre-commit hook — skippable with `--no-verify`, absent on a fresh clone,
  and blind to a web-UI commit either way.

- **api / price/at and price/changes — withheld prices reported as
  not-found, no per-request DB ceiling (RLT-454, RLT-455):** both
  handlers swallowed `ErrPriceWithheld` into the same generic
  `errors/price-not-found` 404 a pair with no data at all gets, even
  though `storePriceAtReader.PriceAt` (`cmd/stellarindex-api/main.go`)
  actively returns it once the substance/scam gate refuses to publish a
  pair — the same distinction `/v1/price`, `/v1/price/tip` and the
  SEP-40 surface already carry. Both handlers now track whether any
  alias/peg orientation hit the gate and report the distinct
  `errors/price-withheld` 404 once every orientation is exhausted;
  `/v1/price/changes`'s per-horizon `available:false` is left as-is
  (the gate is evaluated per horizon target time, so one horizon can be
  withheld while its siblings are not — the endpoint already treats a
  per-item miss the way the batch endpoint does). Neither handler also
  declared a `context.WithTimeout(r.Context(), …)` budget, so an
  unbounded alias/peg walk held its pool connection with nothing to
  catch it; both now cap at 8s, matching the sibling single-shot read
  endpoints.

- **api / cache-control — three SEP-40 oracle passthroughs stuck in the
  300s catalogue band (RLT-438):** `/v1/oracle/lastprice`,
  `/v1/oracle/prices` and `/v1/oracle/x_last_price` matched the
  `/v1/oracle/` prefix arm and served the 5-minute closed-bucket-catalogue
  directive, same as `/v1/oracle/latest` did before #344 carved it out.
  All three are "last observed price" or excludes-in-progress-bucket
  surfaces with no closed-bucket contract of their own; they now join
  `/v1/oracle/latest` in the 30s client / 5s CDN short band in
  `shortBandPolicy`.

- **ops / stellarindex-ops CLI — `-h`/`-help` on a subcommand no longer
  exits 1 (F070, K055):** every subcommand's `flag.FlagSet` uses
  `flag.ContinueOnError`, so `-h` makes `fs.Parse` print usage and return
  `flag.ErrHelp`. `realMain`'s dispatch had no case for it: the error fell
  through the generic branch, printed `"<subcommand>: flag: help
  requested"` on top of the usage line the flag package had just
  printed, and returned exit code 1 — indistinguishable from a real
  failure for a scripted caller. `realMain`'s error-to-exit-code mapping
  is now `dispatchExitCode`, a standalone, unit-tested function that
  treats `flag.ErrHelp` as exit 0 with no extra stderr line.

- **observability / job heartbeat — stale `.pidN.prom` siblings no longer
  wait on a contention that may never come (T597, T601):**
  `sweepStalePIDFiles` previously ran only when a second concurrent run of
  the same job contended for the lock, so a loser that died hard (SIGKILL,
  OOM) with no follow-up contention left its fallback textfile behind
  indefinitely, pinning its `pid` label in Prometheus forever. The primary
  heartbeat now also sweeps its own dead siblings on every heartbeat tick
  and unconditionally before releasing its lock at `Stop`, so a dead
  sibling is reaped within one heartbeat interval — or by the time the
  primary exits, whichever comes first — instead of only on the next
  contention.

- **scripts / lint-changed-test's own lint-count assertions weren't
  shellcheck-optional (RLT-055):** three fixture assertions hard-coded the
  "N lint(s)" totals `lint-changed.sh` reports assuming shellcheck is on
  PATH, unlike the one existing guard for it. `lint-changed.sh` counts only
  the steps it actually ran toward that total and defers shellcheck
  separately when the tool is absent, so on a checkout without shellcheck
  the real count is one lower and each assertion went red. All three now
  branch on a shared `HAVE_SHELLCHECK` probe, same as the existing guard.

- **indexer / hashdb live-append no longer overwrites a recorded hash on
  re-ingest (Q112, Q128, T133):** `recordHashdb` called `hashdb.Append`
  unconditionally on every live ledger, with no prior read — a restart or
  cursor rewind that re-ingested an already-recorded ledger with different
  bytes (the exact upstream-rewrite scenario ADR-0016's drift detector
  exists to catch) silently clobbered the original fingerprint instead of
  flagging it. `recordHashdb` now calls `hashdb.Verify` first and only
  falls back to `Append` on `ErrMissing`, per `Verify`'s own documented
  contract; a mismatch increments `HashdbDriftTotal` and logs instead of
  overwriting.

### Added

- **docs / engineering standards no longer claim unbuilt CI enforcement (NS30, NS31, NS32):**
  `docs/engineering-standards.md` §2.4, §2.6 and §2.7 asserted a
  `docs/reference/deprecations.md` table, a `internal/config/flags.go` flag
  registry with age-based build warnings/failures, and a CI removal-suggestion
  scan for `// workaround` comments as if each existed. None do. Section 6's
  "Enforcement mechanisms" table repeated the same claim for the first two via
  `scripts/ci/check-deprecations.sh` and `scripts/ci/check-flag-age.sh`, which
  also don't exist. All five spots now carry an explicit "Gap:" disclaimer.
  `TestEngineeringStandardsDoesNotClaimUnbuiltEnforcement` and its companion
  `TestEngineeringStandardsSection6DoesNotClaimUnbuiltEnforcement`
  (`test/controlwiring/engineering_standards_gap_claims_test.go`) pin both the
  prose and the table so the doc can't silently re-assert either mechanism
  without the artifact actually landing.
- **test / the withholding guard can now see the handler package (T669):**
  `TestV1VWAPCacheSeamsAreGated` scans `internal/api/v1` for every function
  that reads the aggregator's published VWAP cache and fails unless the
  withholding decision is reached before the read. Its sibling
  `TestPriceServingSeamsAreGated` parses `main.go` alone, so it could never
  see a handler — `/v1/price?window=300|3600|86400` published a
  directory-flagged issuer's aggregated price at 200 while the guard named
  "price serving seams are gated" passed, and it still passes when that fix
  is reverted. The new guard gives an HTTP handler no credit from its
  callers (`handlePrice` consults the decision and still dispatched to the
  windowed handler first), position-checks the consultation against the
  read, and derives its subject set from `TriangulatedPriceLooker`'s method
  list rather than a hand-written seam list. Three seams covered today:
  `handlePriceWindowed` (asks directly), `tryRedisVWAPFallback` and
  `resolveFrozenServe` (inherit it from every caller, one of which proves
  the exemption by discarding the price). Guard only — no behaviour change.
- **test / chainlink fan-out guard releases its slot (NS10):**
  `TestPollOnce_PanickingFeedReleasesItsSlotAndWaiter` pins the half of the
  K012 chainlink guard no test covered. `PollOnce` takes the concurrency
  semaphore in the CALLER's frame, so a recover that contains the panic
  without first running the `<-sem` and `wg.Done` defers turns a
  whole-process crash into a permanent hang of the poll tick — worse than
  the crash it replaces, and invisible to the existing single-feed test,
  which never re-acquires a slot. The new test runs four panicking feeds
  through one slot and asserts the tick returns, reports the failure, emits
  no updates, and moves `stellarindex_worker_panics_total` once per feed.
  Re-derivation of NS10 found both fan-out sites already guarded; this is
  the regression fence, not a behaviour change.
- **api / process-wide goroutine-guard (K012):** `TestK012_EveryGoroutineInTheAPIProcessRecovers`
  is now an unconditional test (the `k012evidence` build tag is gone) and guards
  the whole linked `stellarindex-api` process. #368 closed the unrecovered-panic
  hole in two places and guarded each with an AST walk — `cmd/*/main.go`, via
  each binary's `TestBackgroundWorkersRecover` (a walk over ONE file), and
- **test / verify-archive unit headers (RLT-265):**
  `TestVerifyArchiveUnitHeaders_WatchdogClaimMatchesDirectives` pins a
  mechanical rule — a unit whose comments claim it has no watchdog wiring must
  not set `WatchdogSec=`. `deploy/systemd/verify-archive-tier-a.service`
  breaks it: its header justifies keeping the 16h wall-clock cap by saying the
  reference copy "has no watchdog wiring", while the same file sets
  `Type=notify`, `NotifyAccess=main` and `WatchdogSec=1h` three dozen lines
  below. The header is the only place the trade-off is written down and the
  deploy/systemd copy is what an operator hand-installs, so the contradiction
  either tells them to add wiring the file already has or tells them a run has
  no liveness detection — in which case they keep a wall-clock cap whose
  mid-walk expiry leaves the high-water unadvanced and makes every subsequent
  run a full pass (the 2026-05-13 incident the same header documents). The
  test ships RED behind `//go:build k023evidence`, because correcting the
  header means editing a file outside this unit's set; it is the acceptance
  check for whoever owns it.
- **test / K023 verify-archive wiring evidence (F144):** the build-tagged
  `TestK023_VerifyArchiveCheckpointUnitsFailOnMissed` now asserts on the argv
  that reaches the **binary** — everything after `stellarindex-ops
  verify-archive`, with the `run-heavy-job.sh` singleton wrapper's own prefix
  stripped — instead of scanning the unit file's text. The old matcher
  (`execStartHasFlag`) accepted the token on any non-comment line, so it would
  have certified a `-fail-on-missed` sitting among the wrapper's leading
  arguments, where the wrapper consumes it and the binary never parses it.
  `TestVerifyArchiveBinaryArgs_WrapperPrefixIsNotTheBinary` pins that on
  synthetic `ExecStart` lines (wrapper prefix, header mention, `Environment=`
  line, backslash continuation) so the property holds without a unit file. The
  leg itself stays RED and stays behind `//go:build k023evidence`: landing
  `-fail-on-missed` on the Tier-B units belongs with the files that own them,
  not here.
- **api / goroutine-guard evidence (K012):** a build-tagged acceptance test,
  `TestK012_EveryGoroutineInTheAPIProcessRecovers`, pins the leg of #368 that is
  still open. An unrecovered panic in ANY goroutine terminates the whole
  process, and #368 closed that hole in two places — `cmd/*/main.go`, via each
  binary's `TestBackgroundWorkersRecover` (a walk over ONE file), and
  `internal/api/v1` and its subpackages, via `TestAPIDetachedGoroutinesRecover`
  (a walk rooted at its own tree). Both were green, and neither covered the rest
  of the code linked into the binary, so a PASS over a narrow slice read
  identical to a PASS over everything. This walk takes its package set from the
  linker's answer (`go list -deps .`) instead of a chosen root, so a package
  newly linked into the API is covered the day it lands; it subsumes the two
  narrower walks without replacing them. The HTTP listener stays the one
  exemption, checked BY CONTENT on main.go's own crash-by-design argument.
- **test / shell-fallback route coverage now derives from the filesystem (K022):**
  `web/explorer/functions/shell-fallback.test.js` hand-maintained an 8-entry
  `[name, handler, path]` array; `functions/assets/[[path]].js` had already
  shipped the same shell-fallback pattern as the other six routes with no
  matching entry, so its 200/503 propagation, header-stripping, and
  own-shell-path behaviour ran with zero test coverage. The suite now walks
  `functions/` with `fs.readdirSync`, includes any `[[path]].js` whose
  source references a `/shell/` sub-fetch, and derives each handler's
  expected shell path from its directory position rather than grepping it
  back out of the handler's own source (which would just echo a
  copy-pasted wrong path). `og/[[path]].js` is excluded automatically — a
  different contract, covered by its own `og.test.js`. Test-only; no
  handler behaviour changed.
- **test / VWAP methodology docs pinned against source (HO-359):**
  `TestVWAPDocStablecoinProxyMapMatchesSource` and
  `TestProtocolsReadmeBridgeContractCountsMatchSource`
  (`internal/aggregate/vwap_methodology_doc_test.go`) check
  `docs/methodology/vwap-aggregation.md`'s stablecoin-proxy table and
  `docs/protocols/README.md`'s CCTP/Rozo pinned-contract counts against
  `aggregate.FiatBackers`, `cctp.MainnetContracts()` and
  `rozo.MainnetPaymentContracts` respectively, so the numbers a protocol
  team is asked to verify can't silently drift from what the code ships.
  A prior truth-audit sweep found the two pages' claims could not be
  confirmed by reading alone; re-checked at HEAD, both pages' load-bearing
  facts (source-class table, stablecoin proxy map, freeze/router/TWAP
  file references, ADR citations, bridge contract counts) were accurate —
  no content fix was needed, only this regression guard.

### Changed

- **ansible / `ch-live-catchup` on r1 — the filed claim is false, the real gap
  is elsewhere (Q240):** Q240 said the ClickHouse lake's only self-healer "is
  installed only behind `run_clickhouse`, which is false by design on the
  production host", i.e. that r1's lake has no self-healer. The ansible half is
  true; the conclusion is not. **Measured on r1 2026-09-19:
  `ch-live-catchup.timer` is enabled *and* active, `clickhouse-server` is
  active, and the last service run exited 0 at 10:34.** r1's healer was
  hand-installed out-of-band on 2026-06-12, which is exactly the posture
  `run_clickhouse: false` exists to preserve — no fix was warranted and none was
  made. The measurement is now recorded beside `ch_live_catchup_enabled` in the
  role defaults so the next audit does not re-derive it. What IS open is a
  different and better-evidenced defect: **no inventory in this repo installs
  the healer anywhere** (`r1.example.yml` sets no `run_clickhouse`;
  `testnet.yml` / `futurenet.yml` set `run_clickhouse: true` but
  `ch_live_catchup_enabled: false`), and the consequence is visible on r1 —
  its `/usr/local/bin/ch-live-catchup.sh` is the 3421-byte copy from
  2026-06-12, while the repo's is 6298 bytes and has since #371 F10 REFUSED to
  run without `stellarindex_ch_live_era_from`, which r1's
  `/etc/default/stellarindex-ops` does not carry. Three months of fixes to that
  script have never reached the host running it. Closing that needs
  `configs/ansible/inventory/*.yml` and/or `tasks/08-clickhouse.yml`, both
  outside this unit's file set, and whether r1 should let the ClickHouse install
  tasks run at all is a deploy-topology decision for the maintainer.

### Fixed

- **`/v1/accounts/{g}` served no reserve inputs and mislabelled pool
  shares (#1064):** the account view never decoded `AccountEntry`
  ext.v1/ext.v2, so neither minimum balance nor spendable XLM was
  derivable from its fields. It now serves `num_sponsoring`,
  `num_sponsored`, native `buying_liabilities`/`selling_liabilities`,
  per-trustline liabilities, a trustline `kind` (`asset` | `pool_share`),
  and `num_subentries`/`flags` including zero. Both the account view and
  the `/v1/accounts` wealth ranking now carry a `coverage_note` naming the
  holding domains they exclude (claimable balances, Soroban/SAC contract
  balances); the ranking also sets `lower_bound: true`. Serving those
  excluded domains remains open.
- **api / pricingguard:** the point-in-time serving guard behind
  `/v1/price/at` and every `/v1/price/changes` horizon now judges a
  historical 1m bucket against the buckets immediately before it
  (`timescale.Store.ClosedVWAP1mCombinedBefore`). It fetched the newest
  40 buckets instead, kept only those older than the candidate, and so
  had no baseline — and passed the candidate unjudged — for any instant
  older than about 40 minutes on an active pair, which covers the 1h and
  24h change references it was wired to protect. A bucket with no prior
  bucket at all (the pair's first) is now withheld on these routes rather
  than served as validated: a point-in-time answer has no stale flag to
  carry the doubt `/v1/price` reports through. (#1149)
- **api:** a point-in-time bucket the serving-sanity guard refuses is now
  reported as withheld, not as missing data. The reader returned
  `ErrPriceAtUnavailable`, so `/v1/price/at` answered `price-not-found`
  with "no closed bucket within 24h" and a `/v1/price/changes` horizon
  read as a young pair. A new `ErrPriceAtGuarded` (an `ErrPriceWithheld`
  carrying the new `manipulation_guard` reason) makes `/v1/price/at` a
  `price-withheld` 404 worded for the guard, and each
  `/v1/price/changes` horizon gains a `withheld` boolean, true when the
  reference bucket exists and any serving gate refused it. Both
  `price-withheld` 404s now use the wording of the gate that fired rather
  than always the thin-market sentence. (#1151)
- **api / pricingguard:** the last two raw `prices_1m` readers now pass
  the serving-sanity guard: the SEP-40 `prices(asset, records)` series
  drops a bucket the band rejects (or one with no prior bucket) instead of
  publishing it as an oracle record, and the 24h-ago anchor behind
  `/v1/assets` `change_24h_pct` is judged by the point-in-time guard, a
  refused anchor reading as no anchor. pricingguard's list of wired call
  sites is now enforced by `TestRawPrices1mReadersPassTheGuard`, which
  fails when a function under `cmd/` calls a raw `prices_1m` store read
  without a guard entry point or is missing from the list. (#1150)
- **ops / `ch-rebuild-projected.sh` left every trades aggregate on the
  pre-repair rows (#782):** the script rewrites `trades` over `[50M, 62.894M]`
  and refreshed none of the twelve continuous aggregates built on it, whose
  refresh policies look back at most three months. New `stellarindex-ops
  trades-cagg-refresh -from N -to N` refreshes all of `timescale.TradesCAGGs`
  in order over the time span of the trades now in the ledger range, padded
  by half each view's minimum window so the buckets holding the range's first
  and last rows are refreshed too (Timescale refreshes only whole buckets).
  The script runs it after every window whose re-derive touched trades, and
  records the obligation in `$STALE` before the DELETE so a failed refresh or
  re-derive is retried first by the next run. `prices_1m` is refreshed with
  `force => true` over a window containing every `twap_*` window and the twaps
  are forced after it, since migration 0156's retention drops minute rows
  without an invalidation; while that policy is armed the twaps are refused.
  The usd_volume restamp's printed follow-up now emits the same forced,
  widened calls. Pinned by
  `ch_rebuild_projected_script_caggs_test.go` (executes the script),
  `trades_cagg_refresh_test.go`, and on TimescaleDB by
  `TestTradesCAGGRefresh_RematerialisesARewrittenLedgerRange` and
  `TestTradesCAGGRefresh_RebuildsDroppedMinuteRowsBeforeTheTwaps`.
- **docs / ADR index had no completeness check (T543):**
  `docs/adr/README.md`'s Index table topped out at ADR-0050 though
  ADR-0051 (USD-anchored fiat derivation, landed 2026-08-31) already
  existed on disk; `scripts/ci/lint-docs.sh`'s ADR integrity check (§8)
  only validated each ADR file's own frontmatter and never cross-checked
  the index, so the gap went undetected. Added ADR-0051's row and a new
  §8 check: every `docs/adr/NNNN-*.md` file must have a matching Index
  row, or the lint fails naming the offender.

- **docs / ADR-0040's index summary still claimed comet's automatic
  WASM-hash sweep shipped (T544):** ADR-0040 was amended 2026-07-24 to
  record that what shipped for comet is the curated one-pool allowlist,
  not the aspirational automatic WASM-hash sweep — the ADR's own body
  says so, but `docs/adr/README.md`'s one-line index summary still read
  "comet WASM-hash gate". Reworded the summary to match the ADR's own
  amendment, and added a pointer beside the ADR's "the WASM-hash sweep is
  the registered upkeep loop" line so a reader hitting that sentence in
  the body, not just the top-of-doc amendment, sees the correction.

- **sources / sorocredit — event body capture was lossy despite a
  "nothing is dropped" promise (Q069, RLT-111):** `decodeSettlement`,
  `decodeSupportedAssetAdded` and `decodeConfigBody` stored
  `Attributes["body"]` by running the raw event payload through
  `scval.DisplayB64`, which truncates at 120 runes, caps recursion depth
  at 3, and degrades exotic types to their type name — `scval.Display`
  documents itself as lossy by design. The decoders' own godocs promised
  the opposite ("captured verbatim", "nothing is dropped"). `body` now
  stores the raw base64 XDR payload directly; `scval.Parse` decodes it
  back on demand. `scval.Display`/`DisplayB64` are unchanged and remain
  correct for their actual purpose (compact explorer rendering).

- **platform / account store — admin PATCH race, unbounded reaper sweeps
  and a fabricated client IP (Q148, Q142, Q188):** `PATCH
  /v1/admin/accounts/{id}` now loads, mutates and writes the account under
  `SELECT ... FOR UPDATE` in one transaction, so two racing operator
  PATCHes can no longer silently discard one another (including a
  suspend). The magic-link, login-lockout and suspended-orphan reaper
  sweeps delete in bounded batches instead of one unbounded `DELETE`.
  `CreateSession` and `CreateMagicLinkToken` now fail on a nil client IP
  instead of writing a `0.0.0.0` placeholder into the forensics columns;
  `TouchSession` still leaves `ip_last_seen` unchanged on a nil IP.

- **explorer-web / status page and price poll — unbounded client fetches
  (T275):** the `/v1/price` poll (`usePricePoll` in `lib/live/hooks.ts`)
  and two status-page polls (`/v1/diagnostics/ingestion`,
  `/v1/status/notices`) issued `fetch()` with no `AbortSignal`, so a hung
  connection left the poll waiting past its own interval with no way to
  recover. All three now pass `timeoutSignal()`, the same bounded-signal
  helper `apiGet` already uses.

- **explorer-web / status page endpoint matrix — probes trusted `res.ok`
  alone (RLT-385):** `probeEndpoint` in `StatusPageClient.tsx` reported a
  200 response as `'fast'`/`'slow'` without ever reading the body, so a
  WAF challenge page, maintenance interstitial or misrouted edge response
  could be reported as a healthy API. Both the warm-up and timed fetch now
  parse the body and require the v1 envelope shape (`{"data": ...}`,
  per `internal/api/v1/envelope.go`) before counting the response as a
  real answer.

- **sources / cctp, rozo, sorocredit, phoenix, scale — doc/comment drift
  corrected against the shipped decoder and registry state (Q020, Q023,
  Q076, Q086, Q088, T086, T114, T687, T063):** nine documentation and
  comment claims had fallen out of sync with code that moved past them
  without a doc update. `sorocredit/README.md` still described 7 topic
  symbols and `BackfillSafe: false` against an 8-symbol `EventSymbols()`
  and a `BackfillSafe: true` registry entry, and cited the pre-move
  `cmd/stellarindex-ops/reconciliation_catalogue.go` path. `rozo/events.go`
  cited a nonexistent `AmountDecimals` field on the rozo registry entry
  for its 7-decimals claim. `cctp/README.md` and
  `docs/operations/wasm-audits/cctp.md` still described the 2026-05-26
  WASM-history audit as pending with `BackfillSafe: false`, though the
  audit approved and flipped the flag the same day; the audit doc's
  replay SQL and coverage claim also still reflected the original
  4-symbol transfer-flow scope instead of the 26 symbols the 2026-07-08/09
  governance-event audits added. `cctp/decode.go` claimed contract-ID
  filtering happens downstream of the package, contradicted by
  `dispatcher_adapter.go`'s own in-package `IsCCTPContract` calls.
  `docs/operations/wasm-audits/rozo.md`'s replay SQL still listed the
  original 3 contracts and the never-observed-live `payment`/`flush`
  short-form symbols instead of the 4th contract and the on-wire
  `payment_event`/`flush_event` forms the 2026-07-09 addendum itself
  documents. `docs/operations/wasm-audits/phoenix.md` and the registry's
  phoenix caption asserted binary string presence as if it were runtime
  uniformity, omitting that the pre-upgrade pool WASM's runtime stream
  emitted only 7 of 8 swap fields (the gap `phoenix.RawSwap.Decodable()`
  exists to recover from). `external/scale/scale.go` said "three FX
  venues" while naming only two. All are documentation/comment
  corrections against already-shipped, already-tested code — no decoder
  or registry behavior changed.
- **sources / blend — three more doc/comment claims corrected against the
  shipped decoder (Q022, Q082):** `blend/auction_data.go`'s
  `decodeAuctionData` doc said the asset-key parse "stays generous" to
  account addresses; `decodeAssetAmountMap` actually routes every key
  through `canonical.NewSorobanAsset`, which rejects any non-C-strkey
  (G-addresses included). `blend_backstop/events.go` described a
  "10-event vocabulary" against a 12-constant `Event*` block.
  `blend/dispatcher_adapter.go`'s `NewDecoder` doc said the event surface
  "is currently covered by a single contract version (V2)" while
  `MainnetPoolFactories` trusts both V1 and V2 and `decodeByKind` already
  dispatches the three V1-only event kinds. All three are documentation
  corrections against already-shipped, already-tested code — no decoder
  or registry behavior changed.
- **storage / MEV detection: dropped notional, and a doubled-leg single-venue
  false positive (T416, RLT-275, T001):** `InsertMEVEvent` wrote a literal SQL
  `NULL` for `profit_usd` regardless of the detected candidate's
  `NotionalUSD`, silently discarding it on every insert; it now binds through
  the existing `nullString` helper. Separately, `buildArbCandidate`'s
  single-venue guard only fired for 2-node cycles, so a 3+ node payment that
  padded a real cycle with a redundant extra leg on a pair it already covers
  (more edges than nodes — never true of a genuine minimal cycle) still
  cleared the cycle test and skipped the venue check entirely on one venue;
  the guard now also fires whenever edges exceed nodes.
- **storage / the assets listing `q` filter can find a Soroban contract id
  (RLT-023):** a Soroban-native row has NULL code/slug/issuer by nature (a
  contract asset has no SEP-1 code or issuer account), so the search
  predicate's `COALESCE(ca.slug, ca.code)` was NULL for every such row and
  `type=soroban` combined with any `q` matched nothing. It now falls back to
  `ca.asset_id`, mirroring the listing's own "slug" output column.
- **sorobanevents / the raw-event sink isolates a poison row, never orphans a
  row at shutdown, and its loss counter is finally observed (RLT-134, Q061,
  Q121, Q130):** a permanent data fault (pq class 22/23) on a multi-row
  INSERT abandoned the whole batch — up to 999 good rows lost for one bad
  one; `flushBatch` now bisects the batch and abandons only the row(s) that
  still fault alone. A `PushEvent` racing `Stop()` could win the send against
  an already-closed stopping signal after the drain's last poll, leaving the
  row in a channel nobody reads again, counted neither written nor dropped;
  producers now check stopping first and `Stop()` waits for every in-flight
  producer behind a barrier before the drain takes its final poll, so
  written+dropped+lost always equals pushed. `Start()` is now the no-op its
  doc promised on a second call instead of launching a second worker that
  panicked the process on `close(done)`. `LostCount()` had no production
  caller: the indexer bridges it onto
  `stellarindex_source_insert_errors_total{source="soroban-events",kind="dropped"}`
  every 15 s and on shutdown (the existing per-source dropped-rows alert
  covers it), and both the indexer's and the ops backfill's drain log lines
  carry `lost` with an ERROR naming the CH-lake re-derive path.
- **storage / the fx_quotes insert bumps `source_entry_counts` inline (F110, T437):**
  `InsertFXQuoteBatch` never touched the per-source entries tally its own
  contract said it did, so the active fiat-FX feed reported zero entries
  until an operator re-seeded. The upsert now bumps by the rows whose
  `xmax = 0`, the same replay-safe shape as the trades and oracle paths;
  `TestSourceEntryCounts_FXQuotesBumpInlineAndReconcile` proves bump,
  replay, correction and seed all agree.
- **storage / the entries seed folds every bumped per-source hypertable (T341):**
  sixteen tables the sink bumps (the seven Aquarius non-swap streams,
  soroswap_liquidity, phoenix_initialize, phoenix_admin_events,
  blend_emitter_events, the four sorocredit tables, upshift_vault_events)
  were missing from `SeedSourceEntryCounts`, so a re-seed collapsed
  aquarius to its swap count and zeroed blend_emitter / sorocredit /
  upshift. `TestSeedSourceEntryCountsFoldsEveryPerSourceHypertable` holds
  the seed SQL in lockstep with `DefaultGapDetectorTargets`.
- **storage / `fx_quotes.inverse_usd` is derived in NUMERIC (RLT-114):** the
  worker's float64 `1.0 / rate` was bound straight into the NUMERIC
  column; the insert now computes `1::numeric / rate_usd` itself, so the
  stored reciprocal is exact and agrees with the Rat-space resolver.
- **storage / one trades-rooted CAGG list (RLT-255, leg 1):**
  `timescale.TradesCAGGs` replaces the seven-entry refresh allow-list and
  the twelve-entry restamp copy; the five volume/TWAP aggregates the Go
  refresh used to reject are now refreshable, and
  `TestTradesCAGGsMatchCatalog` holds the list against the migrated
  schema in both directions.
- **forex / a cold start with the primary's names endpoint down still
  installs a snapshot (T052):** rates in hand are labelled by ticker and
  served instead of the refresh returning before `cache.Set`.
- **forex / a healed baseline stays healable until the current feed agrees
  (T054):** the history-majority heal no longer collapses the ticker to
  "confirmed", so an inverted heal (broken history, healthy current feed)
  is re-pointed once the history endpoint is corrected instead of wedging
  the ticker permanently.
- **api / the listings withhold a directory-flagged issuer's last_price
  (T663, RLT-314):** `/v1/markets`, `/v1/pools` and `/v1/pairs` now reach the
  same scam-gate decision `/v1/price`, `/v1/vwap` and `/v1/twap` do before a
  row's `last_price` goes to the wire. The market still lists — trade count
  and volume are activity, not a price — but the price is null. One
  chokepoint (`adjustListingPrice`) serves all three surfaces so they cannot
  drift apart again; pinned by `TestListingsWithholdScamFlaggedLastPrice`.
- **ansible / the archival-node WAL-headroom guard stops crediting a walked-up
  ancestor as WAL (RWC-529, #529):** the guard refuses a `max_wal_size` that
  does not fit the filesystem `pg_wal` is really on — the substitution that
  took r1 down on 2026-09-16. It resolved the symlink for `df`, then walked up
  to the nearest EXISTING ancestor so `df` still named a real volume on a fresh
  host, and `du -sm` ran on that ancestor. Its result reached the assert as
  "MB already occupied by WAL", which is ADDED to free space. On a `pg_wal`
  symlink whose target is not there the walk can reach `/`, so `avail + used`
  approaches the volume's total SIZE and the assert cleared exactly the
  configuration it exists to refuse: measured against a stub tree, a dangling
  `pg_wal` admitted `max_wal_size=16GB` on a volume reporting 100000MB free by
  crediting 12MB of unrelated ancestor data, and the absent-`pg_wal` shape
  credited the cluster directory's own contents. The probe now captures the
  `readlink -f` result BEFORE the walk and runs `du -sm` only when that
  original path is itself a directory, emitting 0 otherwise; `df` keeps its
  ancestor fallback. It also emits an explicit `dir` / `absent` / `dangling`
  state, and a new assert refuses a `pg_wal` symlink that resolves to nothing —
  fail-closed, because no ancestor's `df` describes the capacity WAL will get
  once the missing mount appears. Probed read-only against r1 the same day:
  `pg_wal -> /pgwal/15-main/pg_wal` on `/dev/md1` (the 49G root) resolves to a
  real directory today, and the old and new probes agree there (14086MB free,
  2049MB used) — the change is a no-op on the healthy live shape and only bites
  the broken one. Operator note: this is an **ansible-only** change. A binary
  deploy does not carry it; it reaches r1 only on the next
  `archival-node.yml` apply.
- **pricing / the priceless-popular tripwire's wash exclusion now sees AMM
  volume (T016):** the coverage sweep measured its concentration NUMERATOR
  over rows with `maker IS NOT NULL AND taker IS NOT NULL` while the 7d
  volume DENOMINATOR took every row — two different populations. Only the
  SDEX decoder records both sides of a fill; on every Soroban AMM
  (aquarius, soroswap, phoenix, comet, sushiswap_v3) the resting side is
  the pool, so the row carries a taker and a NULL maker — 100% of the
  27.8k AMM rows on r1 in 24h. The top-account-pair share of an AMM-only
  asset was therefore 0 BY CONSTRUCTION, the wash exclusion could never
  fire for it, and a farm painting volume on an AMM self-selected straight
  into the coverage alert the tripwire exists to keep honest — two such
  assets were live on r1 the same day, above the $10k popularity floor
  with 0.95 and 0.9999 of their volume swapped by ONE account, both
  reporting a share of 0. The counterparty key now DEGENERATES to the one
  known account when a side is unknown (the AMM ping-pong signature: one
  wallet round-tripping through a pool), so numerator and denominator
  speak about the same market. Order-book keying is unchanged. Volume from
  a venue that records no account at all (the external CEX feeds) still
  cannot enter the numerator, so it only dilutes the share downward — an
  unmeasurable market pages an operator rather than being silently
  suppressed — and the new `attributed_vol_share` signal, carried into the
  alert log line, says how much of the volume the share was measured over.

- **ops / lake-dedup-driver.sh stops reporting success on a failed query
  (T429):** `PARTS=$($CH -q …)` never checked clickhouse-client's exit
  code, so an enumeration failure (auth, OOM, server down) left `PARTS`
  empty, `total=0`, the per-partition loop never ran, and the driver's
  last line was still `=== done: 0 partitions processed ===` — exit 0,
  indistinguishable from a genuinely quiet run. The per-partition
  scratch guard had the same shape from the other direction: an
  unparseable `df` line made `[ "$free_bytes" -lt … ]` error (exit 2),
  which `if` reads as false, skipping the ABORT and falling straight
  through to an unguarded `OPTIMIZE … FINAL`, and `OPTIMIZE`'s own exit
  code was captured but never acted on. The driver now checks every
  `$CH` call's exit status and aborts loudly on failure, validates
  `rows_before`/`bytes_before`/`free_bytes`/`rows_after` as plain digits
  before any arithmetic or comparison uses them, and aborts on a
  nonzero `OPTIMIZE` rc instead of logging it inline as just another
  partition. A legitimate zero-candidate run still exits 0 and logs the
  zero, now distinguishable in the log from a failed enumeration because
  the failure path never reaches the `done` line at all.
- **monitoring / the projector's permanent-drop arm gets a rule, and ADR-0003's
  i128 SEV-1 gets an implementation (RLT-131):** the skip arm sheds a row the
  sink permanently rejected and advances the cursor past it — the served tier
  loses the row until it is re-driven — exactly like the quarantine arm, which
  has had a rule since COR-11. This arm had none; its only signal was an ERROR
  log. `stellarindex_projector_row_dropped_permanent` (ticket) now watches
  `outcome="sink_permanent"` in both rule trees, with the Q261
  born-inside-the-window arm the quarantine rule uses, because no projector
  series in production has ever carried an outcome other than `ok`, so the
  child is born by the very drop the rule is for and `increase()` alone reads
  0. Separately, `canonical.ErrI128Overflow` was folded into that same count,
  which made ADR-0003's "any observed overflow fires a SEV-1" unimplementable:
  an overflow is not a verdict about chain data but proof that an `int64` has
  been introduced on one of our own amount paths, so every amount that path
  touched is suspect. It now gets its own `outcome="sink_i128_overflow"` (the
  outcomes still partition — no row is counted twice) and
  `stellarindex_projector_i128_overflow` pages on it. The metrics reference
  enumerates the new outcome and says what `sink_permanent` actually counts
  (the sink's rejection, re-counted every cycle a held row is re-read — not
  the moment the cursor passes it).

- **projector / a permanent sink verdict now needs the same sink-health proof
  a quarantine does (RLT-131):** `permanentSkipCandidate` takes
  `madeProgress` exactly as `quarantineCandidate` already did. A SQLSTATE
  class-22/23 rejection is only ROW-LOCAL while the sink is otherwise
  accepting writes; the identical SQLSTATE arrives GLOBALLY when a migration
  adds a NOT NULL or a CHECK the live rows violate, and from inside the skip
  arm the two are indistinguishable. The per-cycle shed cap bounded how FAST
  such a fault drained the backlog but still let the first row leave the
  served tier on cycle one — about an hour before the lag and `sink_retry`
  signals it produces can reach anyone. Without a durably-committed event in
  the same cycle the verdict now waits out `QuarantineAfterCyclesNoProgress`
  (~1 h) and the cursor holds, so a bad migration is a visible stall rather
  than a silent, unbounded drop; a scattered poison row sits beside rows that
  commit, so it still costs exactly one cycle.

- **explorer / the five `/v1/price/batch` consumers stop erasing the price
  envelope (RLT-384):** the converter, its shared rate hook, the home currency
  strip, the account positions panel and the asset-page swap widget each typed
  the batch response as `{data: Array<{asset_id, price}>}`, so `price_type`,
  `observed_at` and `flags` were thrown away at every one of them. Three
  user-visible falsehoods over money followed. (1) `/convert/[from]/[to]` is a
  static export — `initialRate` is baked at BUILD time — and the widget stamped
  freshness from React Query's `dataUpdatedAt`, the instant the FETCH resolved,
  so any number on screen read "Updated 3s ago". Measured against the live API
  on 2026-09-19, `fiat:EUR`/`fiat:USD` answered `observed_at:
  2026-09-18T00:00:00Z` with `flags.stale: true` — 36h old. (2) The batch OMITS
  a pair it will not price (withheld, or never observed); that resolves as a
  success with no row, the live rate came back `null`, and the baked rate
  silently took its place dressed as the current one. (3) A `price_type: peg`
  row — the operator's standing 1:1 declaration, not an observation — rendered
  identically to an observed VWAP, including inside a positions panel captioned
  "valued at the live VWAP". All five now read the generated
  `PriceBatchEnvelope` type. `useConvertRate` returns a discriminated read
  rather than a nullable number, since "priced" and "omitted" are different
  money facts that a bare `null` collapsed; freshness comes from the row's own
  `observed_at` (a row with no stamp claims none), the declared basis is named,
  `flags.stale` is repeated, and an omitted row renders "rate unavailable —
  showing the last published rate" instead of passing the baked figure off as
  current. The strip and the positions panel stamp themselves with the OLDEST
  `observed_at` they are showing (compared as instants: RFC 3339 stamps carry
  variable fractional precision and lexicographic order gets `…00Z` vs `…00.5Z`
  backwards), mark a declared peg where one values a holding, and the strip's
  24h chip — which had no producer at all — now comes from the row's
  `change_24h_pct`. Twelve tests, seven of them proven red against the unfixed
  consumers.

- **ops / the D3 reproject keeps progress per window instead of one shared file (RLT-399):**
  `scripts/ops/d3-lecur-v2-rebuild.sh reproject <from> <to>` resumed from a
  single `$D3_STATE/reproject-progress` path that named no window, and adopted
  whatever number it held whenever that number was above the requested `from`.
  Running a LOWER range after a higher one is the normal order in this backfill —
  `scripts/ops/phaseD-backfill.sh` walks `[54000000,63050000]` and only then
  `[2,38000000]` — so `reproject 2 38000000` after a `[54000000,63050000)` run
  read the mark 63050000, entered `for ((CLO=63050000; CLO<38000000; …))`, made
  zero iterations, inserted nothing and logged `reproject [63050000,38000000)
  complete`. The early era would then have been missing from v2 with the phase
  reporting success. Progress is now keyed by the window's `from`
  (`reproject-progress.from-<from>`), which is the key the mark's own meaning
  implies — "[from,mark) is inserted" — and leaves the runbook's
  `reproject 38000000 <tip>` resumable across a moving tip, which a `(from,to)`
  key would not. A legacy unkeyed file is ignored with a logged notice rather
  than guessed at: re-inserting a covered chunk is idempotent under the
  ReplacingMergeTree, so the cost is time, not correctness. The phase also now
  refuses a non-numeric bound, an empty window, and a mark that sits below its
  own file's window start, and its completion line names the window that was
  requested rather than the one it resumed into.
- **ops / the D3 cutover refuses to swap an incomplete v2 over a complete v1 (RLT-399):**
  `scripts/ops/d3-lecur-v2-rebuild.sh cutover` went straight from the
  pre-cutover-tip read to `DROP` both MVs and `RENAME` `ledger_entries_current_v2`
  into the served name, without ever asking what was in v2. The RENAME is the
  point where `rollback-precutover` stops applying, and the reproject window is
  an operator argument: the launch plan's `reproject 38000000 <tip>` against a v1
  whose `min(ledger_seq)` reaches lower silently RAISES the current-state coverage
  floor — the one thing Step 2 of
  `deploy/clickhouse/ledger_entries_current_intra_ledger_seq.sql` says the window
  choice must preserve — and the phase reported `cutover DONE`. The phase now
  reads `count(), min(ledger_seq), max(ledger_seq)` from both tables BEFORE any
  DDL and refuses on an empty v2, on a v2 holding fewer rows than v1, on a raised
  floor, or on a v2 whose max lags v1's (a v2 MV that stopped capturing live
  ingest); every violation is printed with both numbers. `D3_FORCE_CUTOVER=yes` is
  the single explicit acknowledgement that proceeds anyway — the same idiom
  `finalize` (`D3_FORCE_DROP_OLD`) and `rollback-precutover` (`D3_FORCE_DROP_V2`)
  already require, and it exists because two ReplacingMergeTrees can differ in raw
  count on merge state alone. Non-numeric aggregates (a failed query returns text,
  or nothing) fail closed rather than being compared or interpolated.
  `scripts/ops/d3-lecur-v2-rebuild-test.sh` drives the phase against a recording
  clickhouse-client stub and asserts the refusals issue no `DROP`, no `RENAME` and
  no `INSERT`, that a covering v2 still cuts over, and that both coverage reads
  precede the first DDL — including the case r1 is in today, where the
  2026-07-29 cutover completed and there is no `ledger_entries_current_v2` at
  all: unfixed, a re-run of the phase DROPped the live
  `ledger_entries_current_mv` before failing on the RENAME.
- **webhooks / the account kill switch reaches the outbound path (SEC-06, RLT-420):**
  suspending or closing an account stopped its API keys authenticating (C3-010,
  `internal/auth`) but nothing in the customer-webhook path read account status,
  so a suspended or closed customer kept accruing queued deliveries AND kept
  receiving our data at the endpoints they had registered. The suspension was
  inbound-only. All four writers/readers are now gated on `accounts.status =
  'active'`: the resolver (`ListWebhooksSubscribedTo`), both enqueue writers
  (`EnqueueDelivery`, `AppendDelivery` — the shared choke point, so the
  price-alert producer is covered too) and the claim query
  (`ListPendingDeliveries`), plus a re-check in the delivery worker immediately
  before it signs and POSTs, for the batch claimed just before a suspension
  landed. Fail-closed throughout: an unreadable status withholds the delivery
  rather than sending it, and an enqueue refusal is reported as
  `PublishResult.Suppressed` — deliberately NOT as a lost event, so the kill
  switch working does not page an operator. A suspended account's queued
  backlog is PARKED, not destroyed (suspension is reversible via
  `AccountStore.Unsuspend`); a closed account's is terminally failed.
- **explorer / the status page's build-time incident parser now agrees with the
  Go corpus loader it mirrors (RLT-464):** `web/explorer/src/lib/incidents.ts`
  hand-rolls a small YAML-frontmatter reader for `/status` and
  `/status/incident/[slug]` (the runtime `internal/incidents` package is a
  typed, tested `go:embed` parser the TS side re-reads at build time so the
  static export can pre-render without a client fetch). The TS reader never
  implemented YAML comment semantics and cast `severity`/`status` straight
  through with `as`. `internal/incidents/data/_template.md` ships
  `resolved_at:  # leave empty until resolved` and
  `affected_components:  # one or more…` as documentation for a human editing
  the frontmatter — legal YAML the Go side (via `yaml.v3`) already parses
  correctly — but a real incident file authored by filling in only the
  required fields left the TS reader treating the dangling comment text as
  the *value*: `resolved_at` came back truthy with the literal comment
  string, and `affected_components` came back a non-array string that
  `Array.isArray` sent to `[]`. `status/page.tsx`'s
  `resolved_at.slice(0,10) + ' ' + resolved_at.slice(11,16) + ' UTC'` then
  rendered `# leave em ty un UTC` as the "Resolved:" timestamp of a
  currently-open incident — an ongoing SEV-1 could publish looking resolved.
  `parseFrontmatter` now strips a trailing `# comment` from unquoted scalars
  (never from inside a quoted value) before it decides whether a value is
  empty, so an empty-with-comment key still falls into the existing
  bullet-list/blank handling. The per-file build step (`parseIncidentFile`,
  now exported for testing) validates `severity`/`status` against the same
  enums the Go `Severity.valid()`/`Status.valid()` use and rejects — with a
  `console.warn`, never a silent default — a file whose severity/status is
  missing or out of enum, matching `internal/incidents/incidents.go`'s
  "logged + skipped, never panics the binary" policy instead of the old
  `?? 'SEV-3'` / `?? 'resolved'` fallbacks the Go package's own doc comment
  already named as unsafe. An unparseable non-empty `resolved_at` is likewise
  rejected rather than silently dropped. New
  `web/explorer/src/lib/incidents.test.ts` seeds a fixture that reproduces
  `_template.md`'s exact frontmatter shape and pins `resolved_at === null`
  and the bullet list still parsing under a commented key.
- **ci / a `--` comment could fake a lake-dedup collapse, and the gate's own
  aggregate classifier had no floor (RLT-050):** `lint-lake-dedup.sh`'s
  `sql_line()` only stripped a `--` comment whose LINE started with it, so an
  inline trailing comment on an unterminated `.sql` statement was appended to
  the statement buffer verbatim — a comment that happened to name `GROUP BY`
  over the table's own identity read as a real clause and greened an
  uncollapsed `count()`. `go_line()` had no `--` handling at all inside a Go
  raw string, so the identical bypass exists on every raw-string SQL literal
  via a full-line `--` documentation comment of the kind `explorer_reader.go`
  and `protocol_reader.go` already write. Both extractors now cut at an
  actual comment MARKER (`--` followed by whitespace or end of line) rather
  than at the first `--` anywhere in the text — a bare `index()` strip was
  tried and rejected, because it also truncates a `--` that is content (a
  CLI flag glued into a raw string, a quoted `'--'` literal), erasing the
  table name and the aggregate along with it and silently swallowing a real
  violation. Separately, `examined == 0` already proved the FROM/JOIN
  extraction was alive, but nothing proved the AGGREGATE half — a
  `mult_agg()` regression that stopped every real read from registering as
  aggregating reported a clean "0 aggregating" run, indistinguishable from a
  tree that legitimately aggregates nothing. Every run now feeds one
  synthetic, guaranteed-uncollapsed `count()` over `stellar.transactions`
  through the same analyser and requires it come back flagged, with its own
  contribution subtracted back out of the reported counts.
  `lint-lake-dedup-test.sh` gained six cases: both comment-bypass shapes
  (`.sql` and Go raw-string), their already-safe `;`-terminated sibling, two
  regression guards proving a marker-unaware strip would erase a real
  violation, and a blinded-classifier case that dies on its own canary
  instead of passing a tree that aggregates nothing.
- **ci / the RLT-050 comment bypass survived on a third path — the header-recipe
  COMMENT RUN, not just the code line (RLT-050 follow-up):** the marker-aware
  cut above landed on `go_line()` and on `sql_line()`'s CODE-line branch, but
  `sql_line()`'s COMMENT-run branch — the one every `.sql` header recipe is
  analysed through, deliberately, so operator verification recipes are in
  scope — still appended its comment body verbatim. A header recipe whose
  own prose carries a second `--` marker documenting a clause it is NOT
  using (`-- GROUP BY ledger_seq, tx_index`, `-- uniqExact(ledger_seq,
  tx_index)`) read that prose as a real clause and greened the uncollapsed
  `count()` above it — the same defect this gate exists to close, now on the
  shape that has no `)` or run boundary to end the read before the prose.
  The comment-run branch now applies the identical marker cut to its own
  body, after the existing run-boundary/indentation checks, before deciding
  whether there is anything left to add. `lint-lake-dedup-test.sh` gained
  three more cases: the `GROUP BY` and `uniqExact` header-recipe bypasses,
  proven RED against the prior fix, and their code-line sibling as a
  lockstep regression guard. The real tree is unchanged: 58 lake-table
  read(s) across 60 file(s), 1 of them aggregating.

- **explorer / the production publish path now runs the static-export guards (F085, T325):**
  the `__next.*` segment prune, `scripts/ci/explorer-file-budget.sh` and
  `scripts/ci/explorer-seo-lint.sh` existed only as steps of
  `.github/workflows/explorer-deploy.yml`, which is `workflow_dispatch`-only and
  documents itself as the hotfix/break-glass path. Production publishes through
  Cloudflare Pages' repository integration, whose build command is
  `cd web/explorer && pnpm install --frozen-lockfile && pnpm build` — so the path
  that actually deploys pruned nothing, measured nothing against Cloudflare's
  20,000-file-per-deployment cap and lint-checked no page metadata. That is the
  exact shape of the Next 15 -> 16 segment-file explosion that silently failed
  every deploy for nine days and froze the site on a June-24 build. All three
  guards moved into `web/explorer/package.json`'s `postbuild` chain, which pnpm
  runs after any `pnpm build`, so every invoker shares them: the Pages build, the
  dispatch workflow, the `web-explorer` CI job and a laptop. The prune keeps
  `__next._tree.txt` — the only segment file the client router prefetches, whose
  deletion produced the 2026-08-27 console-error report — and runs BEFORE the
  budget count, since the count is of what ships. Each guard fails the build
  loudly on a missing `out/` rather than passing over nothing. The three steps
  are gone from `explorer-deploy.yml` — its Build step is `pnpm build`, so it
  gets them from the shared chain and a second copy could only drift — and the
  rationale each step's comment carried (why the prune exists, why the tree
  files survive it, what the budget defends) now lives under **Build-time
  guards** in `docs/operations/explorer-deployment.md`, next to the build
  command an operator reads.

- **api / the lake watermark no longer serialises requests behind one
  ClickHouse read (RLT-095):** `lakeWatermark` held the process-global
  `lakeWMMu` across `LakeWatermarkReader.LakeWatermark(ctx)` — a ClickHouse
  round-trip whose only bound is the connection's 30s
  `max_execution_time`/`ReadTimeout` — so once a TTL window lapsed, one slow
  read put every concurrent request on every lake-backed route
  (`/v1/pools/reserves`, the three `/v1/assets/*/supply` arms, both
  `/v1/liquidity-pools` paths, `/v1/lending/reserves` and the three explorer
  account-state reads) onto a non-context-aware mutex, where they burned
  their own deadlines waiting for a freshness annotation. The failure arm
  made it worse: it did not stamp the fetch time, so a wedged lake was
  re-read once per request, back to back. Now it is the same stale-serve
  posture as the native liquidity-pool listing (#332 F4) — an existing entry
  is returned immediately and a lapsed one kicks ONE detached, 10s-bounded
  read coalesced through `singleflight`; only a process that has never read
  a watermark waits, and then only for the shorter of its own deadline and
  the 2s cold-wait cap. A failed read stamps a 5s retry gap, so a wedged
  lake is retried at most once per gap instead of once per request, and the
  last-good watermark keeps being served (its ageing close time still drives
  `flags.stale` correctly). No response shape changes.
- **api / a half-empty RWA membership set was cached and self-certified fresh (RLT-096):**
  the membership rebuild cached whenever EITHER arm answered, so a failed
  SEP-1 attestation scan (or a failed classic directory lookup) beside a
  healthy contract scan wrote a set with NO classic members over the last
  good one, re-dated it to now and cleared the failure stamp — publishing
  `stale: false` and no `rebuild_failed_at` over half a set for the full
  ten-minute lifetime, on the surface that decides what this index CALLS a
  real-world asset. The handler's only availability test needs BOTH arms
  down, so nothing anywhere went red. The rule is now that every WIRED arm
  must have answered: an arm that is not wired is still no obstacle (a
  one-reader deployment must not rebuild on every request), but a wired arm
  that did not answer makes the rebuild a partial measurement, which is
  refused at the cache. The last good FULL set keeps being served with
  `stale: true` and the real `rebuild_failed_at` beside it, and a cold cache
  with an arm down states the absence rather than publishing a half set —
  the posture the unavailable basis already describes. `available` could not
  express this on its own: it is false both for an unwired reader and for a
  read that failed, and only the second may refuse a cache write.
- **api / the RWA funnel basis never said whether the CLASSIC arm was measured (RLT-096):**
  it was built from the contract and listing arms' availability only, so a
  deployment with no attestation reader wired published the classic
  narrowing as stages of zeros under a sentence describing a measurement —
  asserting that no issuer on this network attests to a real-world asset,
  out of a scan that never ran. The basis now carries the same NOT MEASURED
  correction the other two arms have had since they were added.

- **projector / a global class-22/23 fault no longer sheds the whole backlog on
  cycle one (RLT-131):** `classifySinkFault` returns `dispositionSkip` for ANY
  SQLSTATE class 22/23, and those classes are not always row-local — a
  migration that adds a NOT NULL or a CHECK the live rows violate rejects every
  row of the window at once. The skip arm dropped each of them inline, on the
  first cycle, with no cap and nothing holding the cursor, so one bad deploy
  advanced a source's cursor past its entire backlog in a single pass; the raw
  events survive in the lake, but the only counter it bumped
  (`events_decoded{outcome="sink_permanent"}`) has no alert rule and does not
  say how much was in flight. The arm now takes the rail its sibling
  `quarantineCandidate` already had: at most `PermanentSkipPerCycle` (1) poison
  row shed per cycle, lowest ledger first, with every row it did not shed
  HOLDING the cursor and the cycle reported `runs_total{outcome="sink_retry"}`
  rather than `ok`. A scattered poison row still costs one cycle (COR-11 is
  unchanged: the source drains and never wedges); a global fault becomes a
  visible stall that bleeds one loudly-logged row per cycle while the lag alert
  climbs. A row carrying both a poison output and a held fault is retried
  whole, so it is not a shed candidate and its quarantine budget still
  accumulates.
- **ops / wasm-history exits non-zero on a short walk (RLT-282):** the
  companion to closing its ranges at the last ledger observed. The stdout JSON
  is copied into `docs/operations/wasm-audits/*` and is what a `BackfillSafe`
  determination rests on, and the runbook redirects stdout to a file — so a
  stderr warning is easy to miss. The JSON is still written (its ranges are
  honest about what was observed, and a partial audit is worth having) and the
  command then returns `wasmWalkCoverage`, which names delivered, requested and
  the bucket. An unbounded walk (`-to 0`, the live tail) has no requested count
  and stays exempt.

- **ledgerstream / a both-tier miss was reported as an ordinary miss (RLT-282):**
  `TieredDataStore.coldGetFile` incremented the `both_missing` counter — the
  data-integrity page condition, "neither tier has the object" — and then
  returned the cold store's error verbatim, the one unwrapped error path in the
  file. What the operator saw was `file does not exist`, byte-identical to the
  routine hot miss the cold tier exists to absorb, naming only the tier
  consulted second. Upstream that error becomes the SDK's "ledger object
  containing sequence N is missing" and then, on any bounded ops walk, a clean
  walk-complete via `TolerateTrailingMissing` — so the tier context had to
  travel with the error or nothing survived but a counter. It now reads
  `tiered: "<path>" missing in BOTH tiers (hot, then cold): …`, wrapped with
  `%w` so `errors.Is(err, os.ErrNotExist)` — which `IsNotFound` and the SDK's
  own retry/abort branch key off — is unchanged.

- **ops / verify-decoders had no `-bucket` and never checked what it read
  (RLT-282):** the bucket was hardcoded to `cfg.Storage.S3BucketLive`, which is
  TRIMMED, so pointing the command at a historic range read a prefix of it or
  none of it — and because `TolerateTrailingMissing` turns that into a clean
  walk-complete, the per-source table then reported every decoder as silent and
  the command exited 0. That is the loudest possible false positive from a tool
  whose single claim is "decoder X fired / did not fire over this range". It now
  takes `-bucket` and resolves through `opsutil.ResolveStreamBucket`, names the
  bucket in its banner, and fails closed through `verifyWalkCoverage` when
  delivered != requested, after printing the table.

- **ops / sdex-claim-audit counted no ledgers and defaulted to the trimmed
  bucket (RLT-282):** the tool exists to be differenced against an external
  anchor's trade count for the same range, so a walk that covered less of the
  range than the anchor did turns straight into a phantom decoder gap of
  exactly the ledgers nobody read — and it never counted the ledgers it
  walked, so it could not tell. `-bucket` defaulted to `cfg.Storage.S3BucketLive`,
  which is trimmed and cannot hold a historic range; a SIGINT was also treated
  as a clean exit. The bucket now resolves through `opsutil.ResolveStreamBucket`
  (the same seam policy as ch-backfill and census-backfill rather than a fourth
  local copy), the walk counts delivered ledgers, the report prints walked over
  requested, and the command exits non-zero through the shared `walkCoverage`
  rule after printing the full diagnosis.

- **ops / ch-gate measured the walk against itself (RLT-282):** the ADR-0034
  Phase-2 gate printed both the requested ledger count and the walked one, then
  gated on `ch.LedgerRows != walked` — so when a wrong bucket or a hole
  tolerated by `TolerateTrailingMissing` shortened the walk, both sides shrank
  together, the subset agreed with itself and the gate printed PASSED over a
  range it had only partly opened. The existing zero-ledger refusal caught only
  the loudest shape of that, and it named `*bucket` (empty unless `-bucket` was
  passed) rather than the bucket actually read. Coverage is now asserted as
  DELIVERED == REQUESTED by one shared rule, `walkCoverageErr`, checked before
  any other verdict because every mismatch below it is downstream; the CH-row
  comparison moved to the requested count; and the error names the delivered
  and requested counts and the resolved bucket.

- **ops / wasm-history published the requested bound as observed coverage (RLT-282):**
  `workerResult.upperEnd` is documented as "last ledger the worker actually
  saw" and is what `mergeWasmHistories` closes every open WASM range at — so
  it becomes the `to_ledger` of the coverage range in the tool's stdout JSON.
  It was assigned from the requested chunk bound BEFORE the walk started and
  never re-assigned, so a walk that stopped early asserted a contract was
  observed on a WASM hash through a ledger it never opened. Stopping early is
  routine, not exotic: every ops subcommand builds its ledgerstream config
  through `opsutil.NewBoundedLedgerStreamConfig`, which always sets
  `TolerateTrailingMissing`, and that converts a missing partition inside a
  65,536-ledger window into a clean walk-complete. `upperEnd` is now assigned
  from the ledger the walk callback actually receives, and the command prints
  a loud SHORT WALK line naming delivered-of-requested and the bucket. It
  warns rather than fails because `-to` overshooting the live tip is this
  subcommand's documented normal use.

- **movements / the cap67 watermark could advance past ledgers nobody derived
  (RLT-295):** the contiguity gate lived INSIDE `Cap67Range`'s `if last == 0`
  branch, so `ch-cap67-movements -to N` — the invocation an operator reaches
  for after an incident — trusted the operator's upper bound verbatim, derived
  straight over a near-tip lake hole and stamped
  `stellar.cap67_movements_watermark` above it at every window top. The
  watermark is read back as `max(thru_ledger)`, the derive resumes at
  watermark+1 with no trailing re-derive, and `/movements` floors its Postgres
  arm at the same value, so the skipped ledgers' classic/native movements were
  served by neither arm, permanently and with nothing to see: the raw lake
  heals itself via `ch-live-catchup`, `account_movements` never revisits.
  The clamp is now unconditional — a non-zero `-to` is min()'d against the
  contiguous tip (and the run says so on stderr) — and the advance itself is
  fail-closed: `SetCap67MovementsWatermark` takes the window's lower bound and
  refuses to record it unless `stellar.ledgers` holds every ledger in it
  (`ErrCap67MovementsHole`) and the window continues the derived prefix rather
  than skipping over it (`ErrCap67MovementsSkippedPrefix`, the same loss
  reached through `-from` above watermark+1). A refusal is delay, not loss: the
  watermark holds and the next `-follow` tick retries once the hole heals.
  Both shapes are proven through the real subcommand against a seeded lake
  hole in `test/integration/clickhouse_cap67_to_clamp_test.go`, the write-side
  guard in `clickhouse_cap67_advance_guard_test.go`. Still open, outside this
  change's files: the watermark has no staleness alert.
- **observability / latency burn alerts re-armed against real traffic (RLT-329):**
  the min-traffic guard on all three `stellarindex_slo_latency_burn_*` alerts
  compared TOTAL request rate against a 5 req/s floor sized off a ~2.4 req/s
  synthetic-monitoring baseline. Once the smoke/probe/prewarm User-Agent
  filter excluded that traffic from `http_request_duration_seconds`
  entirely, the floor read real traffic alone — which never clears 5 req/s
  pre-launch — so the guard silently suppressed every latency burn alert
  regardless of how badly the SLO was burning. Replaced the rate floor with
  `stellarindex:api_slow_request_count:1h`, an absolute count of bad
  (slow-or-error) requests: a significance guard that scales with the
  quantity that actually matters (bad requests) instead of total traffic
  volume. Both `configs/prometheus/rules.r1/slo.yml` and
  `deploy/monitoring/rules/slo.yml` change together so r1 does not drift
  from the deploy mirror. The promtool cases in
  `deploy/monitoring/rule-tests/slo_test.yml` run one request a minute —
  0.0167 req/s, two orders of magnitude under the old floor — and differ
  only in how many of those requests were slow: three stays silent, twelve
  pages. Both are proven load-bearing: the firing case stays silent under
  the old rate-based guard (reproducing the #739 disarm) and the silent
  case fires if the guard is neutered. Every assertion is a `> bool`
  comparison yielding exactly 1 or 0, because `increase()`/`rate()`
  extrapolation and range-selector boundaries differ between the
  Prometheus 2.x r1 and the verify container run and a 3.x promtool on a
  developer's PATH — a pinned decimal is green on one and red on the
  other. Also corrected the four runbooks (`api-latency.md`,
  `slo-latency-burn-{fast,medium,slow}.md`) that documented the deleted
  rate guard and asserted the alert "deliberately CANNOT fire" — that
  triage note is now false and is dropped.
- **ops / the archival-node role still hard-failed one import earlier (F128):**
  the ClickHouse-config gate landed on `20-clickhouse-serving-profile.yml`,
  `21-clickhouse-drop-guard.yml` and `22-clickhouse-exporter.yml`, but
  `tasks/15-log-discipline.yml` is imported BEFORE all three (main.yml 227
  against 235) and carried ClickHouse-config tasks of its own, so a host with
  no ClickHouse aborted there instead — on `/var/lib/clickhouse/logs`, chowned
  to a `clickhouse` user no task in this role creates, with every later task in
  the role (including the three now-gated files) never running. The file itself
  cannot be gated: it also carries the rsyslog suppression, the journald cap,
  the logrotate caps and the Redis memory cap, which every host needs. Each of
  its ClickHouse tasks now carries the same
  `clickhouse_config_tasks_enabled | default(false) | bool` the three files use
  — the fact was already resolved right after preflight for exactly this, so
  nothing moved to make room for it.
  **There are EIGHT of them, derived from the file:** the
  `/var/lib/clickhouse/logs` directory, `zzz-logpath.xml`, `zz-ratesengine.xml`,
  the `/etc/clickhouse-client` port drop-in, `zz-merge-memory-guard.xml`,
  `zz-max-query-size.xml`, `zz-system-log-ttl.xml` — and the bare
  `clickhouse-client -q "SELECT 1"` assert, which the earlier counts of six and
  seven missed because it carries `failed_when`, not `when`. So that the count
  can never be re-litigated, `scripts/ci/ansible-clickhouse-host-gate-test.sh`
  now RE-DERIVES the set from the file (a task that writes a clickhouse path,
  chowns to the clickhouse user, or runs clickhouse-client) and requires the
  fact on every member — a ninth cannot be added ungated. Two behavioural arms
  back it, both running the role's own `tasks/main.yml` under `--check` so the
  pre-fix replay cannot touch `/var/lib/clickhouse` either: a host with no
  ClickHouse must report `skipping` for all eight, and r1's shape (config dir
  present, `run_clickhouse` false) must still run them. r1 is unchanged — it has
  the directory, so every one of these tasks still applies there.
  **Operator note: this is an ANSIBLE-only change — it does not reach r1
  through a binary deploy.** It lands on the next `ansible-playbook …
  archival-node.yml` run, and changes nothing on r1 when it does.

- **ci / two ansible regression gates ran in no gate (F128):**
  `scripts/ci/ansible-clickhouse-host-gate-test.sh` and
  `scripts/ci/zfs-snapshot-coverage-test.sh` were wired into neither
  `scripts/dev/verify.sh` nor `.github/workflows/ci.yml`, so the evidence they
  carry — that the role survives a host without ClickHouse, and that the
  rolling snapshots cover the Galexie LCM archive and not just the two tiers
  derived from it — only ran when someone edited the scripts themselves. Both
  now run in `verify.sh` (so in `make prepush`) beside their peers, and in
  CI's `ansible-check` job. That job rather than `import-checks` because both
  need a python with jinja2 + PyYAML, which only `ansible-check` installs;
  the consequence is that CI triggers them on the `ansible` change class, while
  a change to `scripts/ops/zfs-snapshot.sh` alone is caught by the prepush
  gate. Nothing else about either gate changed: `check-verify-parity` still
  passes over all 87 CI gate scripts, and the change-class, prepush-integration
  and integration-shard self-tests are unaffected.
- **ops / cross-anchor checkpoint verification (F144):** a checkpoint with no file in
  `/srv/history-archive` is now attributed before it is judged, and the tier-B units
  enforce ADR-0017 contract 3 with an exit code. The anchor walk runs to the galexie
  bucket's live tip while the mirror is filled by its own daily job, so the newest
  checkpoints it reaches have no mirror file yet and never did — and every one of them
  was counted as missing archive data. **Measured on r1 2026-09-19:** the nightly run
  logged `checkpoints matched=325 missed=23`, then `checkpoint anchor OK`, then
  `Result=success, ExecMainStatus=0` (the fire before it, `matched=324 missed=24`). All
  23 sat in a contiguous block ABOVE the mirror's high-water 64,499,647, and the mirror
  holds **1,007,807 of the 1,007,807** checkpoint files between ledger 63 and that
  high-water — not one hole. The fill lands at ~02:2x UTC and the unit at 04:38 UTC, so
  the block is about two hours of ledgers and recurs on every run. The run then advanced
  the checkpoint tier's `last_verified_ledger` to 64,501,171, certifying 23 checkpoints
  the anchor had never been asked about, which `-from-last-verified` would have started
  the next run above. The mirror's coverage span is now measured from the mirror itself
  before the walk: an absence inside it stays `missed` (a hole — what contract 3
  forbids), an absence beyond it is `unmirrored` and is counted, logged and tolerated
  (`matched=… missed=… unmirrored=…`); an unreadable `-archive-root` measures no span and
  every absence stays `missed`. The checkpoint tier's high-water is clamped to the
  mirror's, so the trailing span waits for the run that can prove it — `updateTierState`
  only moves forward, so no persisted watermark rewinds. DAT-09 is restated for the new
  taxonomy: a run that matched NOTHING is inconclusive whether the absences were holes or
  merely unmirrored.

- **ops / tier-B `-fail-on-missed` (F144):** both tier-B units
  (`configs/ansible/…/verify-archive-tier-b.service.j2` and
  `deploy/systemd/verify-archive-tier-b.service`) now pass `-fail-on-missed`, as the last
  argument AFTER `stellarindex-ops verify-archive` — r1 runs the unit through the
  `run-heavy-job.sh` singleton wrapper, and a flag among the wrapper's leading arguments
  is eaten by the wrapper and never parsed by the binary. Wiring it against the previous
  build would have failed the unit on its next fire, for the trailing block above; with
  the attribution fix the flag reads 0 misses on the measured r1 state. The flag's **code
  default is unchanged** — the earlier deliberate decision ("flipping the code default is
  a separate decision, deliberately not taken here") is respected, and only the units
  change. **On failure the run returns before the state write**, so the checkpoint tier's
  high-water freezes at its last certified value and later runs re-walk from there;
  `node_exporter` raises `stellarindex_verify_archive_tier_b_unit_failed` (ticket).
  **Operator note:** the ansible template does NOT reach r1 through a binary deploy — the
  new flag arrives only when the archival-node role is applied, so until then r1 keeps
  running the unit as currently installed (the binary-side attribution and watermark clamp
  ship with the binary and take effect on the next release). The checkpoint counters still
  reach Prometheus only through the opt-in `-metrics-listen` endpoint that nothing scrapes
  — the textfile exporter writes the mismatch counter and the last-success gauge alone —
  so a miss is observable as a unit failure rather than as a number
  (`verify_archive_textfile.go`, untouched here).
- **test / the two ops subcommands that still default to WRITE (K015):**
  `TestBackfillDefaultsToWrite` and `TestCHBackfillDefaultsToWrite` pin the
  remaining half of the write-gate convention: `backfill` and `ch-backfill`
  declare `-dry-run false`, so they APPLY unless the operator remembers a flag.
  Both ship RED behind `//go:build k015evidence`, because the flip is not a
  code-only change — `scripts/ops/ch-live-catchup.sh` (a timer),
  `ch-full-backfill.sh`, `phaseD-backfill.sh`, `phaseD-range.sh`,
  `ordinal-rederive-chunks.sh` and `restore-drill.sh` all invoke ch-backfill
  with no mode flag, and flipping the default without them turns the production
  catch-up into a silent preview that still exits 0 and records its windows as
  done. Those files are outside this unit's set; the tests are the acceptance
  check for whoever lands the sweep
  (`go test -tags k015evidence ./internal/ops/{ingest,chops}/`).

- **ops / six mutating subcommands had no write gate at all (K015):**
  `census-backfill`, `backfill-router`, `tag-routed-via`, `tag-signer`,
  `seed-soroswap-pairs` and `seed-protocol-contracts` declared neither `-write`
  nor `-dry-run` and wrote unconditionally — an UPDATE over `trades`, a
  substrate row per ledger, a router-swap row per decoded op, a registry
  upsert per pair/child — so a mistyped range was applied on its first run with
  no preview to catch it. The shared fail-closed gate
  (`opsutil.RegisterWriteGate`, "preview by DEFAULT, `-write` applies") existed
  but was an opt-in helper, and a convention a subcommand may decline is not a
  safety property. All six now take `opsutil.NewMutatingFlagSet`, which builds
  the FlagSet and arms the gate in ONE call, print the mode banner before any
  slow work, and — in the default preview — write neither their rows nor their
  resume checkpoint (advancing a cursor over ledgers a run only LOOKED at is
  the C2-14 stride-past hole with the write never having happened at all).
  `TestMutatingSubcommandsRegisterTheSharedWriteGate` drives every mutating
  ingest subcommand through the real dispatch entry point with `-h` and reads
  the gate off its own usage, so the next one cannot be added without it, and
  `TestPreviewPathsNeverReachTheStore` hands each preview path a nil store so a
  branch that quietly still writes panics instead of passing. Two mutating
  subcommands are deliberately still ungated and named in that test's doc:
  `backfill` and `ch-backfill` default to WRITE with a `-dry-run` opt-out, and
  flipping them silently converts every unflagged caller
  (`scripts/ops/ch-live-catchup.sh`, `ch-full-backfill.sh`, `phaseD-*.sh`,
  `ordinal-rederive-chunks.sh`, `restore-drill.sh`) into a preview, so that
  flip lands with its callers.

- **ops / `ch-rebuild -sources` accepts a name nobody knows (K015):** the source
  filter was a bare `strings.Split` of the flag and `enabled()` a membership test
  against it, so `-sources sdx` (a typo for `sdex`) made `enabled()` false for
  EVERY real source: the run streamed its range, decoded nothing, printed its
  DRY-RUN banner and its count report, and exited 0 having re-derived nothing —
  the DO-NOTHING half of the trap the write gate's own doc names, reported as
  success. `-write` already refused an unregistered name through
  `checkCHRebuildBackfillSafe`; the default dry run, which is the mode an
  operator runs FIRST and reads the counts off, did not. `chRebuild` now
  validates `-sources` against the catalogue (plus the SEP-41 pair, which
  `buildReconciliationCatalogue` only promotes when a watched set is configured)
  as soon as the catalogue exists and before the gate warm-up, and refuses an
  unknown name listing the ones it knows. Only UNKNOWN names are refused, never
  known-but-inert ones: a source whose pass this invocation did not request
  (`-sdex` / `-sep41` / `-contract-calls`) still narrows legitimately, and
  `scripts/ops/ch-rebuild-projected.sh` depends on that narrowing — it asks
  `-preflight` for its whole source set and deletes only the subset the verdict
  names back.

- **ops / deploy rollback deleted binaries it never backed up (K078):** the
  whole-deploy rollback in `configs/ansible/tasks/deploy-one-binary.yml`
  re-derived its restore state while it ran instead of consuming what the
  forward path recorded. It `mv`'d **every** binary the run had already
  swapped aside to `.rolledback-<version>` and then `mv`'d a `.prev-<tag>`
  back with `ignore_errors: true` — but the forward path only creates a
  `.prev-<tag>` `when: current_stat.stat.exists`, so a binary this run
  installed for the **first** time (a new name in `binaries_csv`, or any host
  rebuilt from bare metal) had nothing to restore: the restore failed
  silently, the install path was left **empty**, and the play still reported
  that binary among the ones it "rolled back to keep the host on one
  consistent version set". A rollback runs when something is already wrong;
  deleting a healthy binary there turns a bad deploy into an outage.
  The forward path now records, before it touches anything, what a rollback
  of that binary may undo — its live path, the backup it will be able to put
  back (empty when there is none), and the version the sidecar held (empty
  for an untracked or first install). Every rescue task consumes that record:
  nothing is moved aside unless the exact recorded backup is on disk at that
  moment, restores are `removes:`/`creates:`-guarded so a repeated rollback
  is convergent rather than cumulative, and a binary that cannot be rolled
  back is left installed with its service stopped and **named** in the
  failure instead of deleted. The same record also stops a rolled-back
  first/untracked install from having the synthetic `untracked-<timestamp>`
  tag written into its `deployed-versions` sidecar as if it were a released
  version — the marker is removed, which is the pre-deploy truth.
  The per-binary rescue carried the same shape and is gated the same way: it
  parked the live binary at `.failed-<version>` unconditionally, so a deploy
  that failed *before* the atomic rename — a backup `mv` erroring on a
  hand-written sidecar, a full filesystem — moved the working build the run
  had not yet replaced out of the way and then had nothing to restore. Only
  a path this run actually wrote is treated as the bad binary now.
  `scripts/ci/deploy-rollback-test.sh` pins it by running the real task file
  against a throwaway container over a five-binary matrix (tracked prior
  install, first-ever install, untracked prior install, the failing binary,
  and a binary the deploy never names), plus a repeated run, a single-binary
  run whose record set is empty, and a run that fails before the swap.
- **dispatcher / Soroban state-archival eviction is now observed (Q119):**
  `ProcessLedger`'s ledger-entry walk gained a fourth phase that dispatches every
  key stellar-core EVICTED at ledger close as a `Removed` change. Archival is the
  one way an entry leaves the live state without a transaction touching it, so it
  reaches no transaction meta and the three-phase walk could never see it: the
  decoders handled `Restored` — the other half of the same lifecycle — while an
  archived SAC balance's last write stood as the holder's current balance
  FOREVER, holding the served supply component permanently above the truth with
  no path to self-correct (PHO read +157% against Horizon on 2026-07-27). The
  eviction phase is ledger-scoped (empty `tx_hash`, `op_index` -1) and runs last,
  so the eviction outranks any change to the same key earlier in the ledger and
  beats the ops seed's row on ledger; it APPENDS to the ledger's numbering and so
  does not renumber `intra_ledger_seq` or bump `EntryWalkVersion`. Keys no decoder
  watches (the paired TTL keys, contract code) fall out at `Matches` as any
  unmatched change does. `test/integration/sac_eviction_supply_test.go` is the
  served-money proof on real TimescaleDB — seed a dormant holder at
  `SeedIntraLedgerSeq`, evict, watch `SumSACBalancesAtOrBefore` fall to zero, then
  restore and watch it return. Still outstanding and NOT covered here: the lake
  walker (`clickhouse.extractEntryChanges`) has no eviction phase, so
  `stellar.ledger_entries_current` keeps an archived entry's last write as its
  current version and any reader of that table without a liveness filter of its
  own reads it as live. The lake readers that could reinstate a supply
  component already carry one (`ClassifyTTLLiveness`, v0.21.4 — the SAC seed and
  the pool-state readers), so this fix does not depend on the lake half landing;
  it narrows the remaining gap to the current-state projection and to history.

- **ansible / no default inventory (Q250):** `configs/ansible/ansible.cfg` set
  `inventory = ./inventory` — the whole DIRECTORY — and ansible merges a
  directory inventory. Every region file declares the same `archival_nodes`
  group, so a single forgotten `-i` resolved ONE group of five hosts:
  production r1 plus both test nets plus the `r*.example.yml` placeholders
  (measured: `ansible-playbook --list-hosts playbooks/deploy-binary.yml`,
  whose play is `hosts: all`, printed `hosts (5)`). The merge also rewrote
  per-region host vars by load order — si-futurenet came out with
  `stellar_network=testnet`, `region_id=testnet`,
  `galexie_start_ledger=4340000` and `postgres_replication_role=async-replica`
  pointed at `r1-01.stellarindex.io`. This is the sibling of the trap that
  already fired here: omitting `-e secrets_file=` applied r1's secrets to a
  test net and took it down. The default is now fail-closed rather than
  "some region": `inventory` names
  `inventory/no-default-inventory.yml`, a bare YAML scalar that ansible's
  `auto`, `yaml` and `ini` inventory plugins all refuse, so a run without `-i`
  resolves zero hosts (`hosts (0)`) behind four warnings naming that file,
  while `-i inventory/<region>.yml` is unchanged. Every ansible invocation in
  the tree — `ansible-drift.yml`, `deploy.yml`, ci.yml's three syntax-checks,
  `deploy-sync-test.sh`, `ansible-clickhouse-host-gate-test.sh` and the
  bring-up runbook — already passes an explicit inventory, so nothing moves
  with it. `[inventory] unparsed_is_failed = True` would turn the warnings
  into exit 1 and is deliberately NOT set: ansible-lint shells out to
  `ansible-playbook --syntax-check` with no `-i`, and ci.yml's `ansible syntax
  + lint` job goes red (exit 2) with it — that upgrade is a joint change with
  the workflow. Guarded by
  `test/controlwiring/ansible_default_inventory_test.go`, which resolves the
  real playbook through the real ansible.
- **ops / ch-schema-snapshot offsite-configured direction (RLT-029):**
  `ch-schema-snapshot.sh` blanked `SNAPSHOT_MC_TARGET` when the offsite push
  was refused (unknown mc alias, or an alias resolving to loopback), so the
  `stellarindex_ch_schema_snapshot_offsite_configured` gauge read back as `0`
  — indistinguishable from a host that never configured an offsite target at
  all. The alert's whole reason to split "never configured" from "configured
  but failing" is to send the on-call engineer to the right fix (ack the gap,
  vs. fix the alias/credentials); collapsing the two sent a typo'd-alias host
  to the wrong runbook step. The refusal now sets a separate
  `offsite_target_provable` flag that gates the push attempt below it, while
  `SNAPSHOT_MC_TARGET` itself is left untouched so the metrics block still
  reports `1`. `scripts/ops/ch-schema-snapshot-test.sh` was RED at HEAD
  (12/13 — "offsite_configured missing/wrong with a target set") and is now
  13/13.

- **clickhouse / op-stream successful-tx set-build (F111, T385):** `StreamSDEXOps`
  and `StreamClassicOps` still restricted to successful transactions with
  `AND o.tx_hash IN (SELECT tx_hash FROM stellar.transactions WHERE successful = 1
  AND ledger_seq BETWEEN ? AND ?)`. ClickHouse answers that with a
  `CreatingSetsTransform`, which materialises the WHOLE window's tx-hash set in
  memory before the join runs — the shape that blew the 10 GiB query budget on a
  dense 250k-ledger window (2026-07-11). The third sibling,
  `StreamContractCallOps`, was moved off it then and these two were not, so every
  wide re-derive that reaches them (`ch-rebuild`'s SDEX arm,
  `ch-reproject`, `classic-movements-backfill`, `compute-completeness`) carried
  the OOM shape. Both queries now use that sibling's grace_hash `INNER JOIN` over
  a derived table, with `GROUP BY tx_hash` supplying the set semantics `IN` gave
  for free — these readers take no `FINAL`, so an un-merged duplicate part in
  `stellar.transactions` would otherwise fan every op row of that tx out into
  two. The join is spelled BEFORE the outer `WHERE`, which is load-bearing and
  not cosmetic: hoisting the outer ledger window into a derived table instead
  removes the set-build but stops ClickHouse propagating that window through
  `o.ledger_seq = r.ledger_seq`, and `stellar.operation_results` — full chain
  history of wide `result_xdr` — then loses primary-key pruning entirely and
  full-scans as the join's spilled build side. `StreamSDEXOps`' doc comment also
  claimed `join_algorithm=full_sorting_merge` while the query has set
  `grace_hash` throughout; it now says what the code does.
  `test/integration/stream_ops_operation_results_pruning_test.go` is the live
  proof on the pinned ClickHouse line, over a 1M-ledger fixture read through a
  1,000-ledger window: it drives both production entry points, recovers the exact
  statement each executed from `system.query_log`, and asserts no `CreatingSet`
  step plus `EXPLAIN indexes = 1` pruning on `stellar.operation_results`
  (measured 1/62 granules, 29,576 read_rows, against 124/124 and 1,021,384 for
  the hoisted-window shape). All three rejected shapes — IN-subquery,
  hoisted-window, and join-without-`GROUP BY` — are frozen in that test as
  oracles and each is asserted to still exhibit its own pathology, so no
  assertion can pass vacuously.
- **ops / the Galexie archive was in neither backup net (NS03):** the rolling
  ZFS snapshot job covered `data/clickhouse` and `data/postgres` — and nothing
  else. Measured on r1 2026-09-19: `data/postgres` held 8 snapshots,
  `data/clickhouse` 4, and **every other dataset held zero, including
  `data/minio` at 2.64 TB** — the Galexie LCM archive, which is the CDP source
  of truth the ClickHouse lake and the served Postgres tier are *re-derived
  from*. Protecting the derivatives and leaving the thing they derive from with
  no snapshot is the wrong way round, and the off-site half does not cover it
  either: the only off-site credential on the box is pgBackRest's, scoped to its
  own repo2 bucket, so a mis-aimed `mc rm --recursive` had nothing at all to
  roll back to. `data/minio` is now the third entry in the role's
  `zfs_snapshot_datasets` and in `zfs-snapshot.sh`'s built-in
  `ZFS_SNAPSHOT_DATASETS` default (both, so a hand-run or a host with no
  `/etc/default/zfs-snapshot` gets the same set), at **7-day retention**: the
  archive is append-mostly, so a retained day pins only what that day deleted or
  overwrote — ~0 on the happy path — and the window exists to NOTICE a deletion,
  which is Postgres's week rather than the lake's three days. **Be accurate
  about the blast radius:** losing the archive is a *very long recovery*
  (re-ingest from the public Stellar history archives, days–weeks, no
  third-party SLA), **not an unrecoverable loss** — `ha-plan.md` §8 and
  `off-site-backup-plan.md` §1 now say so rather than calling it
  irreplaceable-forever, and both record that this is the LOCAL half only: there
  is still no off-host copy of the archive, and that remains a cost decision.
  The staleness alert is already per-dataset (`{{ $labels.dataset }}`), so
  `data/minio` is covered by it the first time the job runs. Pinned by
  `scripts/ci/zfs-snapshot-coverage-test.sh`, which renders the real
  `zfs-snapshot.env.j2` against the role defaults, sources `zfs-snapshot.sh` and
  asks its own `parse_datasets` what the shipped default resolves to, requires
  the dataset mounted at `minio_data_path` to be covered (derived from
  `zfs_datasets`, not hardcoded on the name) and fails if the two defaults ever
  diverge. **Follow-up outside this unit's file set:**
  `docs/operations/runbooks/zfs-snapshots.md` still enumerates only the two
  datasets and needs the third; `deploy/monitoring/rules/zfs-snapshots.yml`'s
  `absent_over_time` producer-down branch is pinned to `data/clickhouse` and
  could gain a `data/minio` twin.

- **ansible / the archival-node role hard-failed on any host without ClickHouse
  (F128):** `tasks/main.yml` imported three CH-CONFIG task files —
  `20-clickhouse-serving-profile.yml`, `21-clickhouse-drop-guard.yml`,
  `22-clickhouse-exporter.yml` — with no `when:`, and all three of their own
  enable flags default TRUE. Those files are not installers: they drop XML into
  `/etc/clickhouse-server/{config.d,users.d}` and shell out to
  `clickhouse-client --port 9300`. So on a host that has no ClickHouse — the DR
  bring-up, a fresh r2/r3, a no-lake box — the FIRST of them aborted the role on
  its vault-password assert, and because a failed task ends the play, every task
  after it never ran — which is `21-clickhouse-drop-guard.yml`,
  `22-clickhouse-exporter.yml` and `23-local-prometheus.yml`. Firewall,
  hardening, healthcheck, the stellarindex services, Caddy and log discipline
  import BEFORE it (main.yml 193/197/201/205/222/227 against 231) and did run.
  The imports were unconditional on purpose, and that
  purpose is why `run_clickhouse` is the WRONG gate: it is false on r1 by design
  (so `08-clickhouse.yml` never rebuilds r1's hand-tended lake) and r1 is exactly
  the host those three files were written for — gating on it would have silently
  stripped the destructive-DDL drop guard, the `api_serving` profile and the
  metrics endpoint from the one production lake. They are now gated on
  `clickhouse_config_tasks_enabled`, set from a `stat` of
  `clickhouse_server_config_dir` (`/etc/clickhouse-server`, created by the
  clickhouse-server package) OR'd with `run_clickhouse` so a fresh host
  installing ClickHouse on the same run is still configured. r1's behaviour is
  unchanged: the directory is there, with `si-drop-guard.xml`,
  `si-prometheus.xml` and `api-serving.xml` already in it. The serving-profile
  assert stays fail-closed and now names the other way out
  (`clickhouse_serving_profile_enabled: false`) for a bring-up that has not
  minted the credential yet. **Operator note: this is an ANSIBLE-only change —
  it does not reach r1 through a binary deploy.** It lands on the next
  `ansible-playbook … archival-node.yml` run, and changes nothing there when it
  does. Pinned by `scripts/ci/ansible-clickhouse-host-gate-test.sh`, which runs
  the role's own `tasks/main.yml` under `ansible-playbook -c local` against three
  host shapes (no ClickHouse → completes, all three files skipped; ClickHouse
  present with `run_clickhouse` false, i.e. r1's shape → they still run;
  `run_clickhouse` true with no package yet → they still run). The middle arm is
  the one that fails on a `run_clickhouse`-only "fix".
  **The remaining file is named rather than quietly left:**
  `tasks/15-log-discipline.yml` — imported EARLIER than these three, and
  outside this unit's file set — carries more unguarded ClickHouse-config
  tasks, so a real apply to a host with no ClickHouse still aborts there.
  The gate fact is therefore resolved right after preflight rather than beside
  the imports it guards, so closing that file is a one-file change. (Closed
  below in the same release.)

- **ops / the serving kill-switch never stopped the SSE streams (F137, Q239):**
  `sudo touch /etc/caddy/MAINTENANCE_MODE` — the documented way to stop serving
  during a live data-integrity incident — answered 503 on `/v1/price` while
  `/v1/price/stream`, `/v1/price/tip/stream`, `/v1/observations/stream` and
  `/v1/ledger/stream` kept pushing the very values the operator had just
  stopped serving, accepting new connections, with no signal the switch was
  partial. The cause is invisible in the file's source order: OUTSIDE a `route`
  block Caddy sorts a site's directives by its own fixed directive order, in
  which `handle` ranks **above** `respond`, so the stream proxy's
  `handle @sse { … }` block compiled ahead of the maintenance responder —
  `caddy adapt` on the pre-fix file yields
  `[vars+headers, encode, sse-handle, maintenance-503, /metrics, proxy]`, and
  caddy 2.11.4 in front of a stub SSE upstream served 200 + event data on all
  four stream routes with the file present. The kill-switch and the stream
  proxy now sit in one `route` block — the only construct that makes source
  order authoritative — with the 503 first and the proxy second as
  `reverse_proxy @sse`, keeping `flush_interval -1` so a stream still streams
  the moment the switch is disengaged (re-measured live: 503 on all four stream
  routes with the file present, event data within milliseconds once it is
  removed, and `/v1/healthz`, `/metrics` and the ordinary JSON routes byte-for-
  byte unchanged in both states). Both spellings moved together —
  `configs/caddy/Caddyfile.api` and the ansible template
  `configs/ansible/roles/archival-node/templates/Caddyfile.j2`.
  `internal/config/caddy_maintenance_mode_test.go` pins the ordering in both
  copies twice: structurally on the text (no `handle` block in the site; the
  maintenance `respond` inside the route and ahead of the stream proxy) and,
  where the binary is on PATH, on the config `caddy adapt` actually compiles.
  **Operator note:** a binary deploy does not carry this. The template only
  reaches r1 through the caddy role —
  `ansible-playbook … --tags caddy` (`19-caddy.yml` templates
  `/etc/caddy/Caddyfile`, `caddy validate`s it and reloads) — so until that runs
  r1 keeps the old ordering and the kill-switch stays partial there. Verify
  after the reload with the file engaged: every `/v1/*/stream` must answer 503.
- **docs / supply-cross-check-divergence runbook heavy-job command (F155):**
  the mandatory re-seed step's `run-heavy-job.sh` invocation passed
  `stellarindex-ops` as the wrapper's `<name>` job-label argument instead of
  a label, so `NAME=stellarindex-ops` and the wrapper tried to `exec`
  `supply` (the ops binary's first subcommand word) as if it were a binary —
  `run-heavy-job: exec: supply: not found` (exit 127) on every paste. Both
  commands now pass a `supply-seed-sac-balances` job label ahead of the
  `stellarindex-ops supply seed-sac-balances` payload, matching every
  sibling runbook's `run-heavy-job.sh <name> <command...>` usage.
  `scripts/ci/supply-cross-check-divergence-runbook-test.sh` extracts the
  runbook's own `sh` code block and the shipped wrapper from the ansible
  task and runs the block verbatim against a stubbed `stellarindex-ops`,
  so a regression back to the old argument order fails on the actual
  exec-not-found error rather than a hand-copied twin.
- **api, lake, pg / thirteen goroutines could kill the whole process (K012):**
  an unrecovered panic in ANY goroutine terminates the entire Go process, and
  thirteen detached goroutines linked into `stellarindex-api` recovered nothing.
  Every one now carries the shared `internal/worker` guard, so a recovered panic
  moves `stellarindex_worker_panics_total` and pages through
  `stellarindex_worker_panicked`. Containment alone was not the fix — on most of
  these sites it would have traded a crash for something worse — so each guard
  lands with the release its goroutine owns:
  - `internal/storage/clickhouse/ttl_liveness_cache.go` cleared `c.flight` and
    closed `fl.done` as trailing statements. Those move into a new `endFlight`
    called from a `defer`; without it a contained panic would leave `coldFill`'s
    waiters on a channel that never closes while `kickRefresh` handed out the
    same dead flight for the life of the process. Waiters now get
    `errTTLRefreshPanicked` rather than an authoritative empty verdict set,
    which would have re-opened the fail-open `TTLUnknown` path for the whole
    pair registry.
  - `account_state_cache.go`, `accounts_wealth_cache.go` (explorer SWR
    refreshers on attacker-chosen keys), `live_sink.go` and
    `internal/canonical/discovery/sink.go` and
    `internal/sources/sorobanevents/dispatcher_adapter.go` drain workers: the
    guard is registered so the existing `defer` release — the flight end, the
    gate slot, `close(done)` — still runs, so `Stop()` cannot block on a worker
    that is already gone.
  - `internal/sources/external/chainlink/poller.go`: the join goroutine closes
    `results` from a `defer` (a contained panic would otherwise leave the fan-in
    ranging forever), and a panicking feed is reported as that feed's FAILURE.
    Merely containing it made the feed vanish from the tick, which the poller
    classifies as a healthy skip — so the runner would have bumped
    `ExternalPollerLastSuccessUnix` and held the staleness gauge green over a
    poller that reported nothing.
  - `internal/divergence/compare.go` already recovered, but only into one
    `Result`'s `Failures` map; the panic now also goes through `worker.Report`
    so the page rule can see it. The operator-facing `panicked: …` label is
    unchanged.
  - `internal/storage/timescale/trades_bulk.go` (the SDEX bulk trade writer):
    both WaitGroup-joined pools were unguarded, and joined is not protected.
    Both communicate success by the ABSENCE of a value, so a contained panic
    would have been silent corruption: the `usd_volume` fan-out leaves
    `out[i]`'s zero value, i.e. a NULL volume, with `errAt[i]` still nil, so the
    backfill would COPY a silently under-valued batch and report it landed; and
    `copyTradePartitions` decides which ranges COMMITTED from `errs[i] == nil`,
    so a panicked partition would have credited `source_entry_counts` for rows
    that were never written. A panic now fails the call and marks the partition.
    Two hazards the guards introduce are closed in the same change: a dead
    resolver drains on its way out so the index feeder is not stranded on a send
    nobody takes, and `close(next)` stays the feeder's outermost defer. Every
    non-panicking path is byte-identical; the per-row resolve, batch cuts,
    written accounting and `filterStorableTrades` are untouched.
- **money / a catalogue row outlived its issuer's scam suppression (RLT-337 F3):**
  a catalogue-projected listing row (`type: "global"` — the row
  `suppressCatalogueTwins` serves INSTEAD of its classic twin) carries no
  issuer, so `fillIssuerDirectoryTags` skipped it and the scam-class
  suppression never reached it. That mattered because the row is priced
  independently and EARLIER: `fillCataloguePricesForPage` fills `price_usd`
  and `market_cap_usd` from the global tier before `fillCatalogueStatsForPage`
  runs, and `mergeTwinStats` fills only what is nil — so a directory-flagged
  issuer's verified currency published a price and a market cap on the
  catalogue phase of `/v1/assets`, on `/v1/assets?asset_class=…` and on
  `/v1/external/assets`, while the classic row thrown away in its favour and
  its own detail page both served null. `mergeTwinStats` now carries the
  twin's ISSUER verdict — the scam reason and the directory tags/domain/name —
  onto the catalogue row and re-applies `suppressScamIssuerPricing` there.
  The row's wire SHAPE is unchanged: it still carries no `issuer`, which is
  the documented discriminator for a catalogue row, so the answer is carried
  rather than the lookup key. `TestCataloguePricesArePaidBeforeTheTwinMerge`
  pins the order the carry depends on across every path that makes both calls.

- **money / a fiat-coded SEP anchor was reported as an impersonator (K033):**
  the verified-currency catalogue's nineteen sovereign-currency entries carry
  `networks: []`, so `indexTickerOnlyEntry` filed `USD`, `EUR`, `GBP`, `JPY`, …
  into `byStellarCode` — the impersonation index — and `StellarCollision`
  reported a collision for EVERY classic asset bearing one of those codes,
  whoever issued it. On Stellar the ISO code is exactly how SEP-1 tells an
  anchor to code a deposit token it denominates (`anchor_asset_type: fiat`,
  `anchor_asset: USD`), so a regulated dollar anchor lost its `market_cap_usd`
  on `/v1/assets/{id}` (`populateMarketCap` returned before the cap fill), was
  stamped `unverified_ticker_collision: true` on the listing, was refused a
  `listing_valuation`, and was served a warning saying its code "matches a
  well-known asset that has NO verified issuance on Stellar" — said of a dollar
  token, about the dollar. The contract is now decided and stated in one place:
  a fiat ticker is a DENOMINATION, not a Stellar asset identity, so `ClassFiat`
  entries with no Stellar issuance are indexed in `byFiatCode` and answered by
  the new `Catalogue.FiatDenomination`, while `StellarCollision` keeps every
  reference-only ticker (`USDT`, `XRP`, `BTC`, …) — which DO name an asset
  issued elsewhere — and every verified Stellar code exactly as before. A fiat
  entry that ever gains a verified Stellar issuance falls back under the
  collision rule with everything else, and a fiat-coded classic asset is now
  judged by the mechanisms that actually judge an anchor: the issuer directory,
  the scam tags and the substance gate. No wire field changed shape;
  `unverified_ticker_collision` simply stops firing on the compliant case.
  `TestStellarCollision_CoversEveryCatalogueTicker` asserts the new truth rather
  than a relaxed one — every ticker must still be answerable, by one index or
  the other.

- **docs(ops) / verify-archive state file (RLT-265):**
  `ChunkProgress.LastVerifiedHash`'s doc comment claimed it was "used for the
  cross-run chain-continuity proof". No code read it, and one terminal hash
  could not prove a boundary anyway — that needs the right-hand chunk's
  `FirstPrevHash` too, which is what the new `stitch` record carries. The
  comment now says what is true: a human-readable mirror, kept because it is
  already on disk and an operator reading the state file by hand uses it, not
  evidence. `TestChunkProgressLastVerifiedHash_IsNotTheBoundaryProof` pins it
  mechanically — poisoning the mirror changes no verdict, poisoning
  `stitch.last_hash` breaks the stitch.
- **ops(verify-archive):** a resumed walk skipped the cross-chunk chain
  proof (RLT-265). `stitchChunks` was handed only the chunks the run
  actually walked, so on a resume it compared chunks that are not
  adjacent in ledger space: a boundary next to a chunk skipped as `Done`
  was either **never checked** (skipped chunk at an end of the run set —
  a real chain break there was invisible in every run, since the prior
  run's stitch covers only its own results) or reported as a **gap that
  does not exist** (skipped chunk in the middle). The persisted
  `ChunkProgress.LastVerifiedHash`, documented as the "cross-run
  chain-continuity proof", was written and never read. The walk now
  persists each finished chunk's full boundary evidence
  (`first_seq`/`first_prev_hash`/`last_seq`/`last_hash`/`verified`, an
  additive optional `stitch` object) and stitches over the **whole
  plan**, supplying the skipped chunks' terms from that record; a
  `Done` chunk that carries no evidence — anything written by an
  earlier binary — is re-walked rather than assumed, which is
  self-healing. A corrupt or truncated persisted hash is an error, never
  a zero hash that would compare equal to another zero hash. The
  `-resume-from-hash` cross-run check is likewise indexed off the plan,
  so a resume that skipped chunk 0 no longer compares against the wrong
  ledger's hash.
- **ops(verify-archive):** an all-Done resume certified a run that
  verified zero ledgers (RLT-281). A chunk's `Done` marker records only
  that the chunk's own walk returned no error — the cross-chunk stitch,
  the checkpoint-anchor decision and the high-water advance all happen
  *after* the walk and are not recorded per chunk, so a prior run that
  marked every chunk `Done` and still left `in_progress` behind is by
  construction one that failed at or after those proofs (a real
  boundary chain break leaves exactly that state). The walk read
  `resumeChunks`' "all chunks already Done" verdict directly and
  returned `(0, "", nil)`; the textfile defer keys on `retErr == nil`,
  so `stellarindex_verify_archive_last_success_unix` advanced for a run
  that anchored nothing — holding the
  `stellarindex_verify_archive_run_stale` page green — while
  `updateTierState` cleared the `in_progress` record that was the only
  remaining trace of the failed run. The new `planResumedWalk` re-walks
  the full plan in that case instead, and an empty walk plan is now a
  hard error rather than a silent zero-ledger success. Partial resume
  is unchanged.
- **ops,storage / nothing re-read an issuer row once it was filled (RSEC-V1,
  RLT-470):** making `issuers.home_domain` overwritable was necessary and not
  sufficient. Both of the `issuer-flags` drain's queues can only ever see a row
  once — the primary one is `auth_required IS NULL`, so a row leaves it the
  moment it is filled, and the re-check queue covers only
  `last_known_before_removal` rows — and `issuer-enrich`, the job whose whole
  purpose is to sync the column, is a manual one-shot with no timer. So a
  FILLED, live-sourced row (r1 2026-09-03: **49,002** of them) was re-read by
  nothing on a schedule, and an anchor that moved domain with `SetOptions` and
  let the old name lapse kept the lapsed name until an operator ran a backfill
  by hand: the hourly SEP-1 refresh kept fetching it, and whoever registered it
  next could serve a `stellar.toml` listing the anchor's issuer account back,
  satisfy the bidirectional check and inherit its verified org identity. A
  third pass, `Store.IssuersNeedingChainRecheck` + `issuerFlagsChainRecheckPass`,
  now re-offers every filled non-merged row to the live reader each run and
  writes back only the rows the chain has moved past — so re-reading the whole
  set is ~98 bulk lake reads and, in the steady state, no Postgres writes at
  all, and `written` keeps meaning "rows the chain corrected". The queue
  EXCLUDES `last_known_before_removal` rows so the two partition the filled set
  rather than reading ~10k of them from the lake twice a night; it is ordered
  LAST and bounded by its own `-chain-recheck-limit` (default: every filled
  row) so widening it cannot take budget from the primary drain; a key the live
  reader does not answer for still changes nothing, because absence from the
  current-state projection is what a merged account and a coverage gap both
  look like; and an entry that declares NO domain is still not a retraction.
  The run's counters gain a third self-accounting line (`corrected` / `agreed`
  / `unread` of N filled rows), which is the line that shows a timeout leaving
  the primary-key-ordered tail unexamined.

- **api / the issuer read path is still inert for a drained row (RSEC-V1,
  RLT-470 — evidence, not yet fixed):** `enrichIssuerFromAccountState`'s cost
  guard (`source != last_known && AuthRequired != nil && HomeDomain != ""`)
  covers **44,247 of the 49,002** resolved r1 rows, which is exactly the
  population that can hold a LAPSED domain — so the live-beats-stored
  precedence the writers now implement never fires for the rows it exists for.
  The staleness is bounded to one drain cycle by the chain re-check above, and
  the guard is left standing on a measured cost:
  arming it with the seam as it stands costs **+0.47 s per cold issuer detail**
  (api.stellarindex.io 2026-09-19 — `/v1/issuers/{g}` 0.20 s, the same
  account's state read 0.67 s cold and 0.20 s warm behind the 30 s TTL), a
  3.4× regression on a long-tail page most views arrive cold at, and it buys
  nothing on its own: the identity surface the finding turns on (org name,
  logo, verified badge) comes from `sep1_payload`, which the hourly refresh
  fetches against the STORED column. The cost is an artefact of the seam, not
  of the read — `AccountStateCached` fans out to trustlines and offers, none of
  which this path wants, while the narrow reader the drain already uses
  (`BulkAccountAuthFlags`, a `key_xdr` point lookup measured at 0.028 s)
  returns exactly the four flags, the `home_domain` and the as-of ledger. The
  acceptance test for putting that reader on the `ExplorerReader` seam ships
  RED behind `//go:build rsecv1evidence`
  (`go test -tags rsecv1evidence ./internal/api/v1/ -run TestRSECV1 -v`),
  together with the over-reach guard it must not break. The follow-up needs
  `internal/api/v1/server.go` and
  `internal/api/v1/issuers_persisted_provenance_test.go`, neither of which is
  in this unit's file set.

- **docs / integration-trigger table drift (T424):** `docs/contributing/local-verification.md`'s
  path-filter table listed the `integration` change class as it stood before
  T424/F-1334/W6-tst-1 widened `scripts/ci/check-change-class.sh` to also
  cover `internal/ops/archive/**`, `cmd/stellarindex-ops/**`,
  `scripts/ops/**` and `test/harness/**` — so a contributor reading only the
  doc would not expect a change confined to one of those four directories to
  trigger the Docker `integration-test-shard` job, though it does. The table
  now names all ten directories (plus `go.mod`) the shell classifier does.
  `test/controlwiring/local_verification_doc_integration_class_test.go` pins
  the doc against the classifier's regex so the two cannot drift apart
  silently again.

- **docs / sep41 settled-bound helper comment (F159 follow-up, second copy):**
  `settleSEP41Cursor` in `test/integration/sep41_supply_settled_bound_test.go`
  claimed to seed the cursor "through the exact call `internal/projector`
  makes at the end of a clean cycle — `UpsertCursor(ctx, "projector",
  src.Name, commitTo)`". Since F159 the projector's only cursor write is
  `AdvanceCursorFrom`, a compare-and-swap; the same drift as the comment fixed
  one file over, in the file that names the helper. The helper's CALL is
  correct and unchanged — seeding a starting position has nothing to
  compare-and-swap against — so the comment now says what is actually pinned
  end-to-end (the `("projector", src.Name)` pair the storage layer hard-codes)
  and why the call deliberately differs.

- **docs / sep41 rollup cursor-write comment (F159 follow-up):** the
  `sep41SupplyCursorSource` / `sep41SupplyCursorSub` doc comment in
  `internal/storage/timescale/sep41_supply_events.go` still described the
  projector's cursor commit as `UpsertCursor(ctx, "projector", src.Name,
  commitTo)`. F159 (`7f2a32655`) moved that write to a compare-and-swap —
  `AdvanceCursorFrom` at `commitCursor` — so a reader following the old
  comment to `UpsertCursor` would be pointed at a call the projector no
  longer makes. The comment now names `AdvanceCursorFrom` and the finding
  that moved it.
- **explorer / account-activity probe lease (test coverage):** audit finding
  F120 ("`probeSchema` never re-validates a positive verdict") named the same
  latch-for-process-lifetime defect as F119, seen from the
  `accountActivityAvailable` caller side; the fix already landed under F119
  and covers every `requireRows` probe through the shared `schemaProbe`
  primitive, including this one. Added a regression test pinning that the
  account-activity watermark's probe is re-confirmed once its lease expires
  (`TestAccountActivityWatermark_PositiveLeaseRenewsAfterTruncate`) — no
  production code changed.
- **assets / a directory-flagged issuer can no longer publish a listing
  valuation (RLT-313, RLT-337):** `applyListingValuations` refuses a row whose
  issuer carries a scam-class directory tag, and reads that tag from a field
  only `fillIssuerDirectoryTags` writes — which ran three lines LATER on both
  unified listing phases. The refusal therefore saw an empty slice on every
  row, and a flagged issuer served `listing_reference.price_usd` and
  `listing_valuation.value_usd` underneath the `price_usd: null` the same tag
  had just produced. The catalogue-twin fan-out never tagged its twin at all,
  and `mergeTwinStats` carried the result onto the catalogue row.
  The directory call now runs BEFORE the valuation arm on both phases and on
  the twin fan-out (every price PRODUCER still runs above it, so the
  suppression cannot be undone), and `suppressScamIssuerPricing` — the single
  chokepoint — now clears the `listing_reference` / `listing_valuation` pair
  too, so the outcome holds whatever the order. A source-level guard in
  `rwa_pipeline_guard_test.go` holds every function in the package that makes
  both calls to that order, and fails if it finds fewer than three.
  Not closed: catalogue-phase rows carry no issuer at all, so
  `fillIssuerDirectoryTags` skips them and their own price is never
  suppressed — that needs a decision about whether a catalogue row should
  carry its twin's G-issuer.

- **issuers / a lapsed former domain can no longer hold an anchor's identity
  (RSEC-V1, RLT-470):** `issuers.home_domain` was write-once. Both of its
  writers refused a row that already held a value — the enrich job with
  `AND (home_domain IS NULL OR home_domain = <empty>)`, the auth-flags drain
  with a COALESCE that put the stored copy ahead of the one it had just
  decoded — and both justified it by citing a SEP-1 resolver as the
  better-sourced writer. That resolver never writes the column; it READS it to
  choose which domain to fetch. So the clause protected one snapshot of the
  AccountEntry from a newer snapshot of the same AccountEntry, and an anchor
  that moved domain with SetOptions and let the old name lapse could never
  take its identity back: the hourly SEP-1 refresh kept fetching the lapsed
  name, and whoever registered it next could serve a stellar.toml listing the
  anchor's issuer account back, satisfy the bidirectional check, and inherit
  the anchor's verified org name and logo. Re-running `issuer-enrich` is the
  documented on-chain remediation and was a no-op.
  Both writers now overwrite with the on-chain value (an EMPTY reading still
  never blanks a row — the lake only returns accounts that declare a domain,
  and a merged account's reading is deliberately persisted without one), and
  on the read path a live AccountEntry now OUTRANKS the stored copy, as the
  auth flags decoded from the same entry in the same function already did.
  Not closed by this change, and named so it is not assumed: nothing
  automatically re-checks a filled row's domain (the drain's queue is
  `auth_required IS NULL`), the issuer read path keeps its cost skip for rows
  that already carry flags AND a domain, and `sep1_status: verified` still
  does not record which domain it was verified against.

- **sep1 refresh / one hostile stellar.toml no longer wedges all ~76k issuers
  (RSEC-Z1, RLT-458):** the 1 MiB body cap bounded the INPUT, not the decoder's
  work on it — the pinned TOML decoder is roughly quadratic in inline-table
  nesting depth, so a 16 KB document (4,000 levels, publishable by any account
  with 1 XLM) allocated 1.81 GiB and a full-size body admits ~260,000 levels.
  Under the unit's `MemoryMax=2G` that is a cgroup SIGKILL, and because the
  attempt marker was written only AFTER the parse returned, the killed row
  stayed `sep1_resolved_at IS NULL` and came back as candidate #1 —
  `ORDER BY … NULLS FIRST` — on every hourly run, freezing org names, logos,
  `org_verified` and RWA admission for the whole issuer population.
  Three changes: the parser refuses a document nested past a structural-depth
  budget BEFORE decoding it (a string- and comment-aware scan, plus a
  context-free bound that cannot be fooled by a lexer disagreement); the
  refresh marks each issuer's attempt BEFORE the fetch, exactly once, so a
  worker killed mid-decode leaves the poison row deferred by the retry ladder
  (a success costs nothing for it — `SetIssuerSep1Payload` clears the ladder in
  the same statement that writes the payload); and each issuer now runs on its
  own 30s budget so no single domain can consume the run's deadline.
  OPERATOR NOTE: the `sep1-refresh.service` template changed only in its
  comments, which an ansible surface does not carry to r1 with a binary deploy
  — no ansible run is required for this fix, and `MemoryMax=2G` deliberately
  stays as the backstop.

- **docs / phoenix gating:** the tree's four "the factory's creation events
  predate the lake" claims are corrected — the events run from ledger
  51,572,026 — and the docs now describe the gate that shipped rather than the
  one that was planned. `docs/operations/wasm-audits/phoenix.md` records the
  upstream source review that cleared the blocker (allow-listed creators; the
  published address is the deployer's return value, not a parameter; one
  `("create", …)` publish), the trust it extends (the factory is
  admin-upgradeable, so admission trusts the Phoenix factory admin — identity
  trust, not price trust), and the residual it does NOT close (the installed
  WASM was not hashed, and two disassembled variants export a
  `create_liquidity_pool_v2` that exists in no upstream version read).
  `docs/protocols/phoenix.md` moves POOLS to ADR-0040 §1 mechanism 1 and is
  explicit that stake contracts stay curated because the factory never
  announces them. ADR-0040's taxonomy carries a dated correction rather than a
  quiet edit. `extract.go` documents at its source that `topic_0_sym` is
  Symbol-only and that filtering on a topic name must accept both encodings
  (F048).

- **clickhouse / topic[0] prefilter:** `StreamContractEventsFiltered` now matches
  a requested creation symbol in BOTH on-wire encodings. `extract.go` fills the
  lake's `topic_0_sym` convenience column from `Topics[0].GetSym()` — Symbol
  only — so it is EMPTY for every event whose topic[0] is an `ScvString`, and a
  prefilter of `topic_0_sym IN (…)` alone matched nothing for a String-topic
  protocol. Phoenix publishes `("create","liquidity_pool")` as two Strings, so
  the `-ch` re-derive's `gatedPrefilter` walk asked this lake for `"create"`,
  got zero rows over a lake holding those events since ledger 51,572,026, and
  reported a clean walk that had admitted nothing. (`seed-protocol-contracts`
  reads the PG landing zone, whose `topic_0_sym` is filled by
  `tryDecodeSymbolOrString` and does match Strings — it was inert for the
  decoder reason below, not this one.)
  The predicate now also matches the `ScvString` encoding in `topics_xdr[1]`.
  This only WIDENS a prefilter — it can never undercount, and the decoder's
  `Matches()` remains the attribution decision — and it leaves the meaning of
  `topic_0_sym` for the rows already written untouched, so no lake re-extract is
  implied. Phoenix's reconcile-catalogue entry now sets `factories` +
  `creationSym` so that walk actually runs, and three comments asserting the
  walk was inert because "the factory's creation events predate the lake" are
  corrected: the events are in the lake, and the walk was inert for this reason
  instead (F048).

- **phoenix / contract-identity gate:** the factory anchor can finally admit a
  pool. `pipeline.GatedMeta` declared phoenix's factory and a `"create"`
  creation symbol, and `seed-protocol-contracts` walked exactly those events —
  but the walk only decodes what the decoder `Matches()`, and `classifyAny` had
  no create action, so every `("create","liquidity_pool")` event was rejected
  and `Registry.Seed` was never called: the anchor, the walk and the live-upsert
  hook were all provably inert, and a pool the factory deployed stayed
  fail-closed until someone hand-edited `MainnetPools`. The decoder now
  classifies the announcement, gates it on `reg.IsFactory` (never `reg.Has` — a
  curated pool republishing the same topics must not be able to inject a
  child), decodes the single-`Address` body and seeds the pool it names. The
  announced address is safe to trust because upstream `create_liquidity_pool`
  requires the sender's auth AND membership of the factory's
  `whitelisted_accounts`, and publishes the address the factory itself
  deployed — the function takes no pool-address parameter — unlike defindex,
  whose caller-influenced announcement cost it self-registration on 2026-08-25.
  The trust this does extend is the phoenix factory ADMIN's: the factory is
  admin-upgradeable, so a malicious WASM upgrade could publish an arbitrary
  address. That is the same trust the curated seed extended by hand, now
  automatic, and it is contract-identity trust, not price trust (tokens are
  creator-chosen; the pricing guards are downstream). The curated seed and the
  `protocol_contracts` warm remain the operator override. STAKE contracts are
  not announced by the factory — the pool deploys them — so they stay seeded,
  not self-registered (F048).

- **ops / `ch-rebuild-projected.sh`:** a clean-slate window that is left
  EMPTIED can now be told to the completeness verdict, and a window whose
  local marker cannot be written is no longer deleted (F075). `$DIRTY` is a
  file on the rebuild host and the ADR-0033 verdict cannot see it, so between
  a failed re-derive and its recovery `/v1/coverage` kept carrying its prior
  clean claim over a range with no rows in it. Every state that leaves a
  window emptied — a failed re-derive, an ambiguous DELETE, a window still
  pending in `$DIRTY` at the start of a run — now prints a `TELL THE VERDICT`
  line with the command that files the range: the new
  `ch-rebuild -record-dirty-window`, which records one projection dirty window
  per deleted source under the catalogue names the reconcile keys on, so the
  next `compute-completeness` re-reconciles the range instead of carrying the
  claim. The script RUNS that command as well as printing it: the filing used
  to wait on an operator reading the log, so the hole stayed certified until
  someone did. A filing that itself fails (Postgres down, no binary) says
  `COULD NOT FILE` and leaves the printed command as the fallback. A window
  emptied by a kill -9 before the filing is filed by the next run, before it
  recovers anything. The obligation is discharged only by the verdict
  that covers it — nothing in the rebuild path retracts one. A SUCCESSFUL window still records
  nothing, on purpose (#408): one obligation per routine window would point
  the next nightly at ~12.9M ledgers across 8 un-prefiltered sources and time
  every source's verdict out. Separately, `mark_dirty`'s exit code is now
  checked and `$DIRTY` is probed for appendability up front: an unwritable or
  missing state directory used to lose the marker silently and DELETE the
  window anyway, leaving nothing that would ever rebuild it.
- **explorer / asset sidebar (`LiveAssetPrice`):** the 24h change pill beside
  the headline price was built once from the static export's build-time
  `change_24h_pct` and handed down as a fixed React node — the price next
  to it kept refreshing live (poll + tip stream), so a large intraday move
  could leave the pill's arrow pointing the wrong way for as long as the
  page stayed open. `LiveAssetPrice` now takes the raw baked percentage
  (`initialChangePct`) and re-derives the pill from the same live
  change-summary feed `ChangeSummaryStrip` already polls
  (`GET /v1/changes/coin/{id}`, one shared TanStack Query cache entry — no
  second, independently-computed 24h figure beside it), falling back to the
  baked value only until the worker has a row (F090).
- **explorer / account + contract pages:** `DirectoryLabel` now decides
  "warn the user" with the canonical scam vocabulary instead of a private
  copy of it. It held `{malicious, unsafe}` — two of the six
  `DIRECTORY_SCAM_FLAG_TAGS` — and matched the served strings
  case-sensitively, so an address tagged `#scam`, `#phishing`, `#fraud`,
  `#hack`, or `#Malicious` in any spelling the upstream directory ships
  rendered on `/accounts/{G…}` and `/contract/{C…}` as a neutral grey
  badge with no "treat with caution" line — while the same tags make the
  server withhold that issuer's price and rank its assets last. The
  component now calls `scamFlagTags`/`hasDirectoryScamFlag`, the one
  frontend list that `pricingguard.TestScamFlagTagSet_MatchesFrontend`
  pins equal to the Go `DirectoryScamFlagTags`, and its test enumerates
  the warning from that exported list so a private subset cannot come
  back (F091).
- **pricing / `/v1/price` fallback chain:** the scam-issuer gate is now
  consulted at the ENTRY to `priceFallback`, so the aggregator's cached VWAP
  can no longer re-serve a directory-flagged issuer's aggregated price
  through the side door. The reader's withholding chokepoint only runs on the
  arms of `LatestPrice` that produced a value; the synthetic-fiat fast path (a
  `fiat:`/`crypto:` quote never has a literal `prices_1m` row) and the
  zero-trades exit return `ErrPriceNotFound` before it, and that 404 is what
  routes the handler into the fallback chain — whose first layer, the Redis
  VWAP cache, had no gate reference at all. For a flagged issuer's
  triangulated pair, `/v1/price`, `/v1/price/batch` and the SEP-40
  `lastprice`/`x_last_price` passthroughs all answered 200 with the flagged
  market's own price. Both legs are asked, via the package's single
  `scamWithheld` spelling, and the verdict propagates as `withheld` so the
  response is `errors/price-withheld` rather than `errors/price-not-found`
  (RLT-350).
- **pricing / `/v1/price?window=`:** the windowed route now consults the same
  scam-issuer gate. It answers straight out of the aggregator's
  `vwap:<base>:<quote>:<window>` keys and is dispatched from `handlePrice`
  BEFORE the price reader — so neither the reader's withholding chokepoint nor
  the fallback-chain gate above could see it, and `?window=300` alone re-served
  a directory-flagged issuer's aggregated price at 200. Every route that can
  answer out of that cache (`/v1/price`, `/v1/price?window=`, `/v1/price/tip`,
  `/v1/price/batch`, both SEP-40 passthroughs) is now pinned on one market by
  `TestCachedVWAPSurfacesWithholdScamFlaggedMarket` (RLT-350).
- **pricing / GlobalAssetView headline:** the headline's triangulated tier
  (tier 3, the aggregator's VWAP cache) now consults the withholding
  chokepoint like tier 1 already did. Tier 3 is the tier a Stellar-only token
  reaches — its literal `<asset>/fiat:USD` pair has no `prices_1m` rows, so
  tier 1 misses by construction — which meant a directory-flagged issuer's
  asset page could still carry a price and a market cap, the exact outcome the
  scam gate was built for. Withheld degrades to "no data" and the caller falls
  through, as tier 1 does. The scam half only: the substance floor is measured
  on the pair's literal alias union, which is empty by construction on this
  tier (RLT-350).
- **ci / integration-suite trigger:** a change confined to `scripts/ops/**`,
  `cmd/stellarindex-ops/**`, `internal/ops/archive/**` or (in CI)
  `test/harness/**` now runs the Docker-backed integration suite. Those
  directories are in the Makefile's `INT_TEST_PKGS`, so the suite builds and
  runs their `//go:build integration` tests — but neither classifier that
  decides whether the suite runs at all named them: not ci.yml's preflight
  `integration` path filter (which gates the shard matrix' work steps), its
  offline mirror `scripts/ci/check-change-class.sh`, nor
  `scripts/ci/prepush-integration-required.sh`. So for such a diff the tests
  compiled in the unconditional compile gate and were executed by nothing —
  including `scripts/ops/fx-history-backfill/generation_test.go`, which pins
  the INV-3 money invariant that an operator `fx_quotes` correction is
  stamped with a positive derive generation (without it the next gen-0 worker
  refresh silently reverts the correction). All three classifiers were widened
  in lockstep and
  `test/controlwiring/integration_pkgs_trigger_evidence_test.go` — previously
  build-tagged red evidence — now runs untagged and fails if any
  `INT_TEST_PKGS` directory is missing from any of them, or if one is widened
  so far it fires for a docs-only diff (T424, T449).
- **projector / `projector-replay`:** a replay's cursor rewind can no longer
  be reverted by the live projector's in-flight cycle. A cycle reads its
  cursor, spends up to `PerSourceTimeout` scanning and sinking, then
  commits a position derived from that read; the commit was a
  never-regress upsert, so a rewind landing in between — a lower value —
  was simply overwritten by the forward write. The replay printed success,
  its dirty window stayed open, and nothing was re-projected. The
  projector now commits through `Store.AdvanceCursorFrom`, a
  compare-and-swap against the position the cycle read: if the cursor
  moved, the cycle abandons its advance (its idempotent sink writes stay),
  logs it, and the next cycle starts from the rewind point. The rewind
  wins whichever writer reaches the row first (F159, K013).
- **completeness / replay-rewind windows:** `compute-completeness` now
  stores a source's verdict and clears the replay-rewind dirty window that
  verdict discharged in one transaction
  (`Store.PublishCompletenessVerdict`), and the clear runs only if the
  verdict was actually stored. The two used to be independent statements
  and the verdict write could not report that the never-regress guard had
  rejected it: a re-verify run with a `-to` below the stored tip reconciled
  the window clean, had its verdict silently dropped, and then deleted the
  window anyway — leaving the stored pre-rewind clean claim standing over a
  rewritten range that no later run would be forced to re-reconcile. Such a
  run now leaves the window pending and its log line says `VERDICT NOT
  STORED`; a run at or above the stored tip clears it as before (F072,
  K013).
- **ops / `scripts/ops/ch-rebuild-projected.sh`:** the per-window DELETE was
  a hard-coded twelve-table batch that never read `SRC`, so
  `SRC=soroswap bash ch-rebuild-projected.sh` emptied every other source's
  tables for each window, re-derived only soroswap, and — done-state being
  keyed by window alone — marked the window done for all of them; a later
  full run skipped it. A re-derive that died after the DELETE left the window
  emptied with no record, and a narrowed re-run then certified it (F075).
  The DELETE is now built per source from the list `ch-rebuild -preflight`
  says the run will re-derive, and that same list is what `-write` is given,
  so a table is only ever emptied for a source the same window rewrites. A
  test pins each source's DELETE against the reconciliation catalogue's
  table ownership. Done-state is per source (`source lo hi`); a bare window
  start written by earlier runs still reads as done for every source. Before
  each DELETE the window is recorded in `$STATE.dirty`, and removed only
  after its re-derive succeeds: the next run — whatever `SRC`/`FROM`/`TO` it
  is given — rebuilds every recorded window first, for exactly the sources
  that were deleted, and stops if it cannot. `SRC`, `FROM`, `TO`, `WIN` and
  the preflight's list are validated before they reach SQL (`WIN=0` used to
  spin forever). **Operator-visible:** a source the script has no DELETE map
  for (anything outside its eight defaults, e.g. `reflector-dex`) is now
  refused instead of being upserted additively with nothing deleted — run
  `ch-rebuild` directly for those. **Not fixed here:** the ADR-0033
  completeness verdict still does not learn that a window is emptied between
  a failed re-derive and its recovery; `ch-rebuild` records no projection
  dirty window by design (#408), and changing that needs a bounded
  per-window re-reconcile in `compute-completeness` first.

- **ops / `ch-rebuild -preflight`, `scripts/ops/ch-rebuild-projected.sh`:**
  the script DELETEd a window and only then ran `ch-rebuild -write`, whose
  refusals — the `BackfillSafe` gate, the live-cursor one-writer guard, the
  2M-ledger buffered-range ceiling — all fire inside that second process. A
  guard doing its job therefore left the window's tables empty, in
  autocommit, with nothing to rewrite them (RLT-381). `ch-rebuild` gains
  `-preflight` (only valid with `-write`): it runs those three guards for the
  exact range and sources, prints one line
  (`ch-rebuild: preflight ok [from,to] rederive=a,b,c`) and exits before the
  first lake read, writing nothing. The script now asks first, per window,
  and treats anything short of that line as a refusal — a guard's "no", a
  deployed binary that predates the flag, or silence — deleting nothing. The
  DELETE batch is also one `BEGIN … COMMIT` transaction, so a failure on a
  later table no longer leaves the earlier ones emptied. **Operator-visible:**
  the script needs an ops binary that knows `-preflight`; against an older
  one it stops before the first DELETE rather than running unguarded.
  Runtime failures after the DELETE (a lake stream error, a failed write)
  are not something a preflight can see.

- **ops / `scripts/ops/ch-rebuild-projected.sh`:** the per-window trades
  DELETE named `sushiswap_v3`, which was never in the script's re-derive
  list — so every window it processed deleted that source's served trades,
  rewrote none of them, and was then marked done (RLT-380). Adding it to the
  re-derive is not available: `ch-rebuild -write` refuses a source that is
  not `BackfillSafe`, and `sushiswap_v3` is not. The script no longer deletes
  it. A test now EXECUTES the shipped script against stubs for `psql` and
  the ops binary and fails if a window can delete trades for a source the
  same window does not ask `ch-rebuild` to re-derive.
- **`/issuers/[g_strkey]` long-tail SEO (F086):** `generateMetadata` had no
  `shell` branch, so the runtime-fallback shell that
  `functions/issuers/[[path]].js` serves verbatim for every issuer beyond
  the pre-rendered top-100 baked `canonical`/`og:url` from the literal
  string `shell` and carried no `robots` directive — every long-tail
  issuer page declared itself as `https://stellarindex.io/issuers/shell`
  and was indexable, consolidating the entire long tail onto one URL.
  `generateMetadata` now short-circuits on `g_strkey === 'shell'` and
  returns generic, `noindex, follow` metadata with no canonical, matching
  the pattern already used by `/assets/[slug]` and `/markets/[pair]`.
- **price:** `/v1/price?window=N` takes `flags.frozen` from the freeze
  marker of the pair the value was read under, not from the spelling the
  client used (K037 class sweep; closes the open site of T004). The
  windowed path walks both legs' aliases to find the published
  `vwap:` key, then asked the marker about the requested literal — and the
  marker is keyed on the literal pair the aggregator prices. So
  `asset=native` answered from a frozen `crypto:XLM` market carried a held
  value with no `frozen` flag, and a marker on the requested literal flagged
  a healthy alias's value as frozen. The value served is unchanged: a frozen
  pair's windowed key already is what the freeze holds.
- **test / ci:** the regression test for an INV-3 money invariant was
  compiled and run by nothing (T424, T449).
  `scripts/ops/fx-history-backfill/generation_test.go` pins that the FX
  history backfill stamps a positive derive generation on its store — without
  it an operator's `fx_quotes` correction is written at generation 0 and the
  next daily worker refresh silently reverts it. The file is `//go:build
  integration`, so the unit job never saw it, and its package was missing from
  the Makefile's `INT_TEST_PKGS`, so neither `make test-integration`, the CI
  compile gate, nor any CI shard did either: the seam could regress, or the
  test stop compiling, with every gate green. `INT_TEST_PKGS` now lists
  `./scripts/ops/...`; the shard script derives its package list from that
  variable, so shard 0 runs the test and the four shards still partition the
  suite exactly. This was the third package found stranded this way, one at a
  time, so the class is now closed: an untagged test in `test/controlwiring`
  walks the tree for integration-gated test files and fails the default suite
  on any whose package `INT_TEST_PKGS` does not match. Still open, and
  recorded as build-tagged red evidence (`-tags t424evidence`) rather than
  claimed: a change confined to `scripts/ops/`, `cmd/stellarindex-ops/` or
  `internal/ops/archive/` does not *trigger* the Docker suite, in CI's
  path filter or in the local pre-push classifier, so for such a change the
  test compiles but executes only when something else in the diff does.

- **forex / `/v1/price` fiat paths:** the in-memory FX snapshot that
  `/v1/price` and `/v1/price/tip` read for fiat crosses now carries only
  rates the C2-030 sanity band accepted. The worker used to install the raw
  upstream snapshot and run the band afterwards, inside the `fx_quotes`
  write, so the band protected the table and not the served value: on
  2026-08-24 it kept Massive's UZS = 1820 (true ≈ 11,800) out of `fx_quotes`
  all day while every `quote=fiat:UZS` request was priced off 1820. The band
  now runs first — with or without a writer attached; a cache-only worker
  previously served every bar unbanded — and the snapshot is built from its
  verdicts. A ticker whose new rate is refused, or that the standby feed
  does not carry, keeps its last guarded rate with its original timestamp
  for at most 7 days (the `fx_quotes` lookback), then drops; a ticker whose
  baseline the history-majority heal overturns is dropped until a current
  rate is accepted. The fiat-vs-fiat path stamps `observed_at` with the
  older leg's own timestamp, so a held rate is not presented as today's.
  (F004, F026, K032)
- **forex (degraded refreshes):** the join between the FX rates map and the
  currency-names map is now case-insensitive. The primary emits lower-case
  codes, the ECB standby UPPER-case, and the reused-names path (names
  endpoint down) re-keys by UPPER-case ticker, so any refresh that mixed the
  two matched nothing and the in-memory snapshot collapsed to a single
  synthetic USD row — on exactly the refreshes the standby and the names
  reuse exist for. The trailing-7d history fetch had the same join and
  silently returned no bars under reused names. (F033)
- **rozo (payment memo):** a payer can no longer make a Rozo payment
  permanently un-ingestible by choosing its memo bytes. The memo is an
  `ScString` — arbitrary bytes, picked by whoever calls `pay()`, for one
  stroop — and the decoder bound it unchecked to the `rozo_events.memo`
  `text` column. Postgres refuses a NUL or an invalid UTF-8 sequence there
  (SQLSTATE 22021), the projector classes that as a permanent data error and
  skips the event, and the row is gone for good. The decoder now hands the
  memo over through the new `scval.AsText`: a memo that is valid NUL-free
  UTF-8 is stored verbatim exactly as before, and anything else is stored as
  `\x` + the hex of its bytes (Postgres's own bytea notation — recover the
  bytes with `decode(substr(memo, 3), 'hex')` or `scval.FromText`). The
  mapping loses nothing, is deterministic (a re-derive writes the same row)
  and is injective: a clean memo that itself begins with `\x` is hex-encoded
  too, so no literal memo can be mistaken for an encoded one. No schema
  change; `scval.AsString` is unchanged and now documents that its result is
  bytes, not text. New integration test drives NUL, invalid-UTF-8, overlong,
  surrogate and out-of-range memos through decoder → sink → real Postgres
  (audit 2026-09-02 F052).
- **ops (route-sweep.sh):** the deploy-time route sweep no longer exits 0
  when its OpenAPI spec parser fails or produces an empty/too-short route
  list. Previously `python3 … <<PY … PY > /tmp/route-sweep-paths.txt` had
  no exit-status check and the redirect always created the file, so a box
  without PyYAML (or any generator failure) left an empty file, the
  while-read loop over it ran zero iterations, and the script printed
  `ok=0 client_4xx=0 server_5xx=0 unreachable=0 skipped=0` and exited 0 —
  a tooling failure indistinguishable from "every route healthy",
  reproducing the invisibility of the 2026-07-27 outage (21 of 94 GETs
  503ing under an all-green board) one layer further down. The generator
  is now checked for a non-zero exit, the resulting list must clear a
  floor (`ROUTE_SWEEP_MIN_ROUTES`, default 50) and must contain the exact
  routes dark during that outage (`/ledgers`, `/contracts`,
  `/accounts/{g_strkey}`); any of those failing refuses with exit 2
  before a single curl is issued. New `scripts/ops/route-sweep-test.sh`
  pins the refusal on an unreadable spec, a spec parsing to zero routes,
  and a spec missing the known routes, and confirms the positive path
  still sweeps (audit 2026-09-02 F082).

- **ops (`ch-rebuild`, contract-gated sources):** `ch-rebuild` now re-derives
  on the gate the live indexer runs with — each gated source's in-code
  curated set UNION the children in `protocol_contracts` — instead of the
  bare in-code seed. The re-derive catalogue takes only a config, so it built
  all eight gated decoders (comet, blend_emitter, phoenix, blend, aquarius,
  sushiswap_v3, upshift, defindex) with no registry warm. A pool or vault an
  operator admitted through `protocol_contracts` (the documented no-redeploy
  seam) was decoded live and then invisible to the rebuild, so a truncate +
  `ch-rebuild -write` rebuilt its table WITHOUT those rows.
  `preseedFactoryChildren` never covered it: it walks creation events, which
  such a contract does not have, and it is a no-op for the five sources that
  declare no factory. The warm is read-only (no upsert hook), rebuilds the
  `-ch` prefilter throwaway with the same options, and widens the static
  `contractIDs` hard filter (upshift, blend_emitter) by the registry-only
  extras — with an empty registry the list is byte-identical to the in-code
  one. It fails closed if a gated source has no warmed options or if the
  catalogue's decoder type differs from the gated registry's. The other three
  catalogue consumers are closed by the bullet below (RLT-430).

- **ops (`compute-completeness`, `verify-reconciliation`, `ch-reproject`):**
  the remaining three consumers of the reconciliation catalogue now warm its
  gated decoders from `protocol_contracts` too, closing RLT-430 for the whole
  class. `compute-completeness` is the one that reached the public API: it is
  the EXPECTED side of `/v1/coverage`, so a contract admitted through the
  operator seam produced real served rows that the unwarmed re-derive could
  not account for — the source published as a projection mismatch and its
  rows read as phantoms. `verify-reconciliation` reported the same mismatch
  to an operator, and `ch-reproject` both dropped the contract from the
  static `contractIDs` prefilter and rejected its events in `Matches()` —
  neither writes, but `ch-reproject` is the report that answers "what would
  rebuilding Postgres from ClickHouse change?", so a CH-under-served delta
  that is not in the data is exactly what invites the destructive
  `ch-rebuild -write`. Each warm runs on the
  freshly built catalogue and BEFORE anything reads its decoders — ahead of
  `preseedFactoryChildren`, which seeds into the instances the warm rebuilds,
  and ahead of the recognition owner map, which reads the widened
  `contractIDs`. `compute-completeness` reuses the gated options it already
  loaded for the recognition scan (the call moved above the catalogue's first
  reader) rather than warming twice. Read-only throughout (no upsert hook).
  The previously build-tagged evidence test is now untagged and guards the
  class: any future consumer of `buildReconciliationCatalogue` that does not
  warm fails it, and the per-call-site ORDER is pinned for all four. On
  testnet / futurenet all eight gated sources are pubnet-anchored and already
  filtered out of the catalogue (#483), so the warm has nothing to rebuild
  there and cannot fail closed — pinned by a test.
- **completeness (recognition, rozo + blend_backstop):** an unhandled event
  topic on a Rozo payment contract or on the Blend backstop now fails THAT
  source's `recognition_ok`. Neither catalogue entry declared `contractIDs`,
  and neither source is a gated-registry source, so nothing ever named their
  contracts in the recognition owner map: such a gap fell into the
  system-wide unattributed bucket and the per-source axis was structurally
  unable to go false — the 2026-07-07 rozo blind-spot class, with the alert
  that used to cover it gone since #465. Both entries now pin the exact set
  their decoder already gates `Matches()` on (the four Rozo payment
  contracts; backstop V2 + V1), so the pin is counts-identical as a
  re-derive prefilter. Expect either source to turn red on the next
  `compute-completeness` pass if its contracts already carry a shape no arm
  handles — that is the axis reporting for the first time, not a regression.
  A lockstep test holds each pin against the decoder's own identity check
  (audit 2026-09-02 F071).
- **migrate (credential redaction):** `stellarindex-migrate` no longer echoes
  the text it was handed on any path that can carry the Postgres DSN, and the
  scrubber that used to be the only thing between that echo and the log is now
  the backstop rather than the control (audit 2026-09-02 F077, K057). The tool
  is given the production DSN — password inline — on every deploy, and its
  stderr is captured by the deploy job, journald and Loki, so a malformed
  value printed there is a compromised credential on the run where an operator
  is most likely to paste the output into a ticket. Two earlier attempts
  scrubbed the secret back out of the echo by pattern and then by value, and
  each was rejected on one more shape the scrubber read differently from the
  way the operator meant it: a password with an unescaped `@`, then one with a
  space or a quote, then `net/url`'s own reason quoting the password as a bad
  port, then the flag package cutting a rejected "flag name" at the first `=`
  of a base64 password, then a `?password=` value holding an `@` behind any
  `host:port`. A malformed DSN is by definition not parseable, so every rule
  for where the password ends in one has a counter-example — which is why the
  fix is to stop echoing rather than to scrub better. **The four sites that
  repeated operator text now do not.** The flag package's output goes to
  `io.Discard` and the tool composes its own message, naming the argument's
  POSITION and never its text; `unknown subcommand`, `down N`, `force V` and
  the leftover-positional error quote an argument only when it is a plain word
  (`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$` — a mistyped verb or a step count) and
  otherwise withhold it; and `redact.ParseFailure` classifies the parse failure
  by TYPE and reports it in this project's own words, rendering the string only
  when it PARSES, structurally, from the parse tree. **What replaces the
  value** is which of the two routes it arrived by (`-dsn` or
  `$STELLARINDEX_POSTGRES_DSN`), what kind of thing is wrong with it (`invalid
  URL escape`, `invalid port after the host`, …) and, for an argument, its
  position on the command line. A DSN that parses is untouched: it still
  reaches the dial and still reports the host it could not reach.
  **Operator-visible:** an UNPARSEABLE DSN no longer has its host echoed back —
  that is the trade, and the value is in the file the operator just edited.
  `redact.Known` stays armed behind all of this for the one class the binary
  does not write itself, a dependency's error embedding the URL it was handed,
  and two leaks in it are closed with it: the greedy userinfo reading of
  `postgres://host:5432/db?password=HEAD@TAIL` used to consume the `password=`
  that the query reading is recognised by and print `@TAIL`, so a cut may no
  longer end inside a second secret whose anchor it swallowed and a `:pw@` span
  that covers another anchor is not used at all; and a password beginning with
  the literal `<redacted>` used to be mistaken for an already-cut region and
  print its remainder. **Not closed, and listed on `redact.Known` rather than
  implied away:** a secret repeated with neither its anchor nor its span (the
  price of never rewriting the bare word `app`), a query password that itself
  contains `&<recognised parameter>=`, and a credential the process was never
  handed as a connection string (`PGPASSWORD`, a passfile). Tests build and run
  the real binary over every shape in the flag-name, subcommand, `down N`,
  `force V` and leftover slots and through both routes the DSN arrives by, and
  assert that neither half of a stand-in password NOR the harmless parts of the
  argument (the host, a `password=` key) reach the output — the harmless parts
  being the canary that the tool echoed at all. Red before the change: 26
  credential-leak assertions on the previous attempt's binary, 94 on the
  branch base.
- **supply (SEP-41 rollup):** a fold pass can no longer pair a fold reset it
  can see with a view of `sep41_supply_events` from before the rewrite that
  reset was issued for. The 2026-09-18 fix took the rollup row's lock
  _inside_ the folding statement; under READ COMMITTED that statement's
  snapshot is fixed when it starts, while `SELECT … FOR UPDATE` returns the
  latest committed row. A pass whose statement began just before
  `ch-rebuild -sep41 -write` (or `projector-replay`) committed its last row
  and its reset therefore read `last_ledger = 0`, summed the OLD event set
  "from zero", and moved the checkpoint back above the re-derived rows —
  stranding them below both the fold and the reader's live delta. On the
  regression fixture: 1,500,001 served for a true 1,750,001, unrepaired by
  the next cadence. The window is narrow (statement start to lock
  acquisition) but the loss was permanent and silent. The pass now takes the
  lock in a statement of its own, so the fold's snapshot postdates it. The
  in-code claim that the row-materialising `INSERT … ON CONFLICT DO NOTHING`
  "never contends" was also wrong (it waits out an in-progress reset) and is
  corrected. New two-connection integration test pins the interleave and
  fails if the pass parks anywhere but its locking statement (audit
  2026-09-02 F108).
- **explorer (tx lookup, lake indexes):** a `stellar.tx_hash_index` that is
  emptied while the API is running no longer turns every `/v1/tx/{hash}`
  into a 404. A miss against the index is served as authoritative absence,
  which is only safe while the index holds rows — and the reader checked
  that once, then kept the "holds rows" answer for the life of the process.
  Truncating the index, or dropping and recreating it ahead of a
  `ch-txindex-backfill`, left every already-running API process answering
  "no such transaction" for transactions that exist, until it was
  restarted. The answer is now a 30-second lease: the first lookup after it
  runs out re-asks (one `LIMIT 1` read per lease window per process, none
  while idle — not one per request), an emptied index is dropped to the scan
  path at once and picked back up once repopulated, and a re-check that
  lands between the DROP and the CREATE does not pin the scan path for the
  process lifetime. If the store will not answer the re-check, the last
  answer is honoured for at most two minutes. The same lease covers every
  other rows-required lake probe (contract active-ledgers, instance changes,
  census, accounts stats, creator/sponsor boards and edges, holders rollup,
  account-activity watermark); probes of a column's or table's existence
  keep their process-lifetime cache (audit F119).
- **ops (runbook):** `customer-webhook-delivery-failing.md` no longer tells
  the operator that a healthy-Postgres `_mark_errors` loop is the outcome
  write sharing the per-attempt HTTP deadline ("#368 M6, code half
  outstanding"). That stopped being true when `Worker.mark` moved every
  outcome write onto `context.WithoutCancel` bounded by its own 1s
  `markWriteTimeout`. The section now describes the shipped behaviour and
  points at what can still cause it: a single-row UPDATE that cannot land
  inside that second (CO-22 carry-over).
- **test-infra (integration):** the claimable-balance seed tests no longer
  time out when the machine is loaded (CO-22). Two Blend fixtures seed the
  process-shared ClickHouse lake at ledgers 3,999,900,000-4,000,000,000 and
  left the rows there; the seed readers walk the lake from `min(ledger_seq)`
  to `max(ledger_seq)` in 250k-ledger windows, so every later walk in the
  same process stepped ~16,000 empty windows — 53-58 s each unloaded, five
  of them in one file, and `TestClaimableSeed_NativeAndAssetScope` breached
  its 5-minute deadline under load (reproduced on unfixed source: FAIL at
  300.00 s, package 485 s). Both fixtures now delete their high-ledger rows
  in `t.Cleanup` with a synchronous, partition-scoped mutation that fails
  the test if any row survives. The claimable-seed helper checks the lake's
  window count before each walk and names the offending row instead of
  timing out, and a new regression test runs both fixtures and then asserts
  the walk is <= 2,000 windows and finishes inside 30 s. Same selection
  after the fix: 18 s, the two-walk test 4.2 s, one walk 285 windows in
  1.3 s. The SAC full-history seed tests walk the same bounds, so they
  shed the same windows (measured after the fix: ~1.3 s each).
- **api (dashboard sign-in), ops:** an empty Resend key no longer reports
  sign-in mail as sent. With `STELLARINDEX_RESEND_API_KEY` unset or empty the
  API wired a recording stub whose send returns success, so
  `POST /v1/auth/login` answered `200 {"status":"sent"}`, minted a live
  magic-link row nobody could receive, and counted
  `stellarindex_notify_sends_total{result="sent"}` — the
  `notify_send_failure_ratio_high` alert read 0 on a deployment where nobody
  could sign in by email. The signup path already guarded this state; login
  did not. Now an unset, empty or blank-only key wires a transport whose every
  send is an error (`notify: mail transport is not configured`), the login
  handler refuses up front with `503` — before the throttle and before any
  token row or login-intent cookie, identically for every address, so it is
  neither an enumeration nor a throttle oracle — and the refusal is counted
  as `result="failed"`, which is what the alert reads. Signup keeps reporting
  `email_verification_sent: false`. The API still boots (a missing mail key
  must not take price serving down with it); passkey sign-in and existing
  sessions are unaffected. A Resend sender built without a key fails before
  the wire instead of posting a blank bearer, and the key is whitespace-trimmed.
  Only the env var's name is ever logged, never the key or part of it
  (RLT-321, #734).
  **Operator note:** the behaviour above ships in the binary and is safe on
  its own. Separately, the ansible env template
  (`stellarindex.env.j2`) no longer falls back to an empty key on a
  production host that serves the dashboard — the render is refused instead.
  That template change does **not** reach a host through a binary deploy; it
  takes effect only on the next role apply, and until then a host's
  `/etc/default/stellarindex` is whatever was last rendered. Because the
  template task is `no_log`, ansible censors the refusal text: a censored
  failure of the `/etc/default/stellarindex` render means
  `vault_resend_api_key` is missing from that region's secrets vault. The
  test nets (`region_deployment` other than `production`) and hosts with
  `stellarindex_dashboard_base_url: ""` still render an empty key, and the
  API answers 503 on login there. To check a live host without reading the
  key: `journalctl -u stellarindex-api | grep "mail transport is NOT configured"`.
- **ops (compute-completeness, verify-reconciliation):** a soroswap pair
  seed that is configured and fails now stops the run instead of being
  printed and ignored. The seed is a live RPC sweep of the factory and was
  the one input read before the per-source loop that logged and continued
  while every sibling returned; it also returns part-way through, so a
  failure could leave the re-derive decoder holding some pairs and not
  others. Every event of a missing pair then failed the decoder's match,
  the projection re-derive expected zero against real served trades, and
  `projection_ok=false` was published over healthy data — after which the
  next `-pass` re-floored soroswap, the first catalogue source, at genesis.
  Both commands now return the error (no snapshot is written, the last real
  verdict stands, the timer exits non-zero). A seed that is *disabled* is
  not a failure and does not stop anything: with
  `oracle.soroswap.factory_contract` empty — the documented disable, the
  config default, and what testnet and futurenet run — the seed is skipped
  with a one-line notice and every source is still evaluated, the same
  split `verify-decoders` already makes. A factory that is set with no RPC
  endpoint to sweep it from is a failure. (RLT-416, #805)
- **alerts (customer webhooks):**
  `stellarindex_customer_webhook_delivery_exhausted` can now fire on the
  event it describes. It was `sum(rate(…{outcome="exhausted"}[1h])) > 0`
  with `for: 1h` — the `rate()` twin of the tripwire defect below. `rate()`
  is `increase()` divided by the window, so after one exhausted delivery the
  expression is true for exactly 1h and a 1h pending period could never
  complete; the ticket was raised only if deliveries kept exhausting through
  a second consecutive hour, while its text ("A delivery hit the 15-attempt
  retry budget … Customer hasn't received the event") is about one. It is
  now `for: 0m` in both rule trees. Nothing transient is left to filter — an
  exhausted delivery is already the end of ~8h of retries — and the
  expression is unchanged, so one ticket stays up while exhaustions continue
  and resolves 1h after the last. No born-inside-the-window arm is needed:
  every outcome child is pre-seeded at 0. New promtool cases in
  `tripwire-isolated-increment{,-r1}_test.yml` feed ONE exhaustion against
  each tree (red on the old rule: no alert at 5m or 60m) and pin that an old
  non-zero count and healthy `delivered` traffic stay silent; the
  alerts-catalog row is updated. **Operator note:** ticket severity; expect
  it to appear for customer endpoints that have been dead all along. Reaches
  r1 the same way as the rules below (audit Q261).
- **alerts (data-loss tripwires):** four "any nonzero increase" alerts can
  now fire on the event they exist for.
  `stellarindex_ingestion_persist_drop`,
  `stellarindex_ingestion_trade_buffer_drop`,
  `stellarindex_projector_row_quarantined` and
  `stellarindex_ledgerstream_tier_both_missing` (page) were all written as
  `increase(m[W]) > 0` with `for: W`. After a single increment that
  expression is true for W and the pending period needs it true for a
  further W, so one isolated drop could never fire them — they alerted only
  while increments kept arriving, which is a sustained-rate alert carrying a
  tripwire's description ("Sensitive — any nonzero increase"). They are now
  `for: 0m`; a `for:` shorter than the window would only have delayed the
  notification, since the expression cannot go false sooner than W after
  the event. The first three were mute a second way: they watch an
  unseeded counter child that is born at 1 by the very increment in
  question, and `increase()` over a series with no earlier sample reads 0 —
  the normal case after every deploy, because counters reset. They gain the
  `or (m > 0 unless m offset W)` arm the oracle-symbol rules already use.
  Both rule trees change together, and new promtool cases in
  `deploy/monitoring/rule-tests/tripwire-isolated-increment{,-r1}_test.yml`
  feed each rule ONE increment (existing series, and born-non-zero) against
  each tree; every earlier case for these rules fed a continuous ramp, the
  one shape that did fire. Left alone on purpose, and pinned as such:
  `discovery_drops`, `discovery_record_failures`, `ch_live_sink_drops`,
  `ch_live_sink_drops_sustained` and `stellar_archive_publish_fail` share
  the shape but describe themselves, and are documented in their runbooks,
  as sustained signals (archive-publish.md relies on `for: 1h` to filter a
  retried transient). Two things the rule comments now say plainly. The
  born-inside-the-window arm has a known false positive: "did not exist one
  window ago" is also true when Prometheus itself has no sample there, so
  after a Prometheus outage or scrape gap longer than the 5m lookback an
  already-non-zero child tickets for up to W with no new event — accepted
  at ticket severity, as the oracle-symbol rules accept it, used on no
  page, and pinned as a known limitation by a promtool case in each tree.
  And `tier_both_missing` is safe at `for: 0m` because no scraped process
  emits it benignly: tiering attaches only to archive-bucket reads, and the
  one scraped process that makes them is the indexer in its bounded archive
  phase, where a both-tiers miss is a real hole (the tip-racing reads that
  miss harmlessly belong to `stellarindex-ops` backfills, which Prometheus
  does not scrape). `docs/operations/alerts-catalog.md` (four rows) and
  `projector-row-quarantined.md`'s "Detected by" row quoted the old `for:`
  and are brought in line. **Operator note:**
  `stellarindex_ledgerstream_tier_both_missing` is a PAGE and is now
  `for: 0m` — one both-tiers miss pages at once. Before the rules land,
  read
  `increase(stellarindex_ledgerstream_tier_read_total{outcome="both_missing"}[30d])`
  on r1. The other alerts here go from never firing to firing at once, so
  expect tickets that were previously silent. Codified is not applied:
  `configs/prometheus/rules.r1/` is applied by `deploy.yml`'s `prom_rules`
  step, which runs unconditionally on every r1 deploy (it is
  `continue-on-error`, so read its result), and the multi-host
  `deploy/monitoring/rules/` tree is shipped by the ansible `prometheus`
  role (`tasks/03-prometheus-configure.yml`) (audit Q261).
- **projector (trades):** a trade the store permanently rejects is no longer
  reported as projected. `persistTrade` returned nil both when a trade
  landed and when it was dropped as a permanent data fault, and the
  projector counts every nil sink return as a durable commit — so a dropped
  trade was published under
  `stellarindex_projector_events_decoded_total{outcome="ok"}`, the one label
  that promises the row was written, and appeared in no projector loss
  counter. `persistTrade` (and `persistTradeRouted`'s external arm, which
  carried its own copy of the contract) now return a `*TradeDroppedError`
  that unwraps to the store's error: the projector classifies it as a
  permanent fault, counts it `outcome="sink_permanent"` and still advances
  the cursor, so a poison row cannot wedge a sole-writer source. Reporting
  the drop exposed a second defect, fixed with it: the projector stopped a
  lake row at its FIRST sink error, so once a drop was an error the row's
  remaining outputs were never offered to the sink while the cursor
  advanced past them — and one row really does decode to several outputs
  (soroswap emits one trade per completed swap+sync pair absorbed from a
  single event; phoenix emits rescued evicted trades plus the completed
  one). `processEventSafely` now continues past a permanently dropped
  output and stops only at a retryable or unclassified fault, which still
  holds the cursor; `outcome="sink_permanent"` is counted per dropped
  OUTPUT, like `outcome="ok"`, not once per row. This also applies to a
  permanently rejected non-trade output, whose later siblings were lost
  the same way before. The validation and constraints that decide which
  trades the store rejects are not touched, nor is the dispatcher batch
  path, which carries a row only on a context error (audit RLT-132).
- **ops (projected-rebuild):** a trade the store permanently rejects is
  counted and reported, and does not hold its window. Now that
  `pipeline.HandleEvent` reports that drop (above), counting every non-nil
  return as a window-holding insert error would have left a
  deterministically poison trade's window un-checkpointed on every resumed
  run, under a log line saying "re-run to retry it" — which can never
  succeed. `-write` recognises `*pipeline.TradeDroppedError`, counts it in
  the new `PermanentDrops` result, logs it with the fact that no re-run can
  land it, and checkpoints the window — the live projector's policy for the
  same fault, and what this tool already does for a decode error. The
  checkpoint behaviour for such a trade is what it was before the drop was
  reported; what is new is that the loss is visible. The summary now
  reports the two loss classes apart, because the operator's next step is
  opposite: held windows say re-run, permanent drops say fix the defect
  then re-run the range with `-resume=false`. Every other insert failure
  still holds its window (COR-09). The comments claiming only non-trade
  inserts can reach that seam are corrected (audit RLT-132).
- **ops (soroswap pair seed):** one transient RPC failure no longer fails
  the seed, and with it the nightly completeness pass. Failing closed on a
  seed error (above) made `compute-completeness` and
  `verify-reconciliation` exactly as reliable as the sweep, which is 1+3N
  sequential `simulateTransaction` calls (about 640 on pubnet) with no
  retry anywhere under it — so a single 429, 5xx or dropped connection
  stopped the run for every source. Each call is now retried on a
  *transient* failure only, up to 5 attempts with 1s/2s/4s/8s backoff that
  honours the context. Transient means: a transport error or a body cut
  short; an HTTP 408, 429 or 5xx **whatever its body** (empty, HTML, JSON
  that is not an envelope, or a JSON-RPC error envelope with any code — the
  status decides); on a status below 400, a JSON-RPC `-32603` internal
  error, a code in the implementation-defined server range
  `-32000..-32099` (where hosted providers put `rate limit exceeded`), or an
  HTTP-style 408/429/5xx code in the envelope; and a body that does not
  decode as an envelope (a proxy interstitial served with a 200, or nothing),
  which costs at most the bounded budget if it turns out to be permanent. A
  contract that rejects the call, any other 4xx (even over a retryable
  envelope code), `-32600`/`-32601`/`-32602`/`-32700` and every other
  JSON-RPC code, and a result of the wrong shape are deterministic and
  still fail at once; an endpoint that stays down for a whole budget still
  fails the
  run closed, and the error carries the attempt count and wraps the cause.
  Each retry is logged at WARN. The retry re-issues the one failed call; it
  never restarts the sweep. Worst-case added wall time is 15s of backoff
  per call plus up to four extra attempts; across a sweep it is bounded by
  the caller's existing 15-minute seed context. The inter-call throttle
  now honours the context too. `seed-soroswap-pairs` and `verify-decoders`
  share the sweep and get the same behaviour. The seed notices from the two
  completeness commands no longer claim to come from
  `verify-reconciliation` when `compute-completeness` printed them.
  **Operator note:** `seed_rpc_endpoint` on r1 stays the public
  `https://mainnet.sorobanrpc.com`. Pointing it at the host's own
  `127.0.0.1:8000` was considered and rejected: the archival-node role
  deploys no stellar-rpc (removed from r1 on 2026-04-23), so nothing
  listens there and the seed would fail closed every night. The ansible
  template change is a non-rendering comment recording that; no role apply
  is needed and the rendered `stellarindex.toml` is unchanged. (RLT-416,
  #805)
- **stellarrpc (client errors):** every response with an HTTP status of 400
  or above now returns a typed `*HTTPStatusError` carrying the status. The
  client kept the status only as message text, and only for a non-empty
  non-JSON body or a valid-JSON body with no `error` member: an empty body
  surfaced as `decode: unexpected end of JSON input`, an undecodable JSON
  body as a decode error, and a JSON-RPC error envelope as the bare
  `*JSONRPCError` — so no caller could tell a 429 from a malformed request,
  which is what left the soroswap pair-seed retry above blind to the usual
  shape of a hosted provider's rate limit. `*HTTPStatusError` unwraps to the
  `*JSONRPCError` when the body carried one, so `errors.As` for it is
  unaffected; its message now leads with `stellarrpc: <method>: HTTP <code>`.
  An undecodable body on a status below 400 is a typed
  `*ResponseDecodeError` (same `decode:` message as before). No other caller
  classifies these errors — `detect-gaps`, `rpc-probe`, `verify-decoders` and
  `seed-soroswap-pairs` only print them. (RLT-416, #805)
- **api (markets, pools):** `last_price` on `/v1/markets` and `/v1/pools` no
  longer grows by the decimals factor on every cache hit. Both handlers
  correct a non-7-decimals pair's raw price in place, and the in-process
  markets cache handed every request the cache entry's own row slice — so
  the first request wrote the corrected price into the cache and each later
  hit multiplied it again (a 9-decimals token's 41.32 served as 4132, then
  413200, then 41320000 for the life of the entry), while concurrent
  requests raced on the same array. The cache now returns a copy of its
  rows from every serving branch (cold leader, fresh hit, stale-serve and
  cold waiter), so a caller can only ever write its own page. Ordinary
  7-decimals pairs were never affected. None of the existing listing tests
  wired the handler to the cache the way production does; the new ones do,
  and run concurrent hits under the race detector (audit F014).
- **api (markets):** `/v1/markets` no longer serves `volume_history_24h` and
  `first_trade_at` to requests that did not ask for them. The
  `?include=sparkline,inception` enrichment was written onto the same shared
  cached rows as the price correction above, and `include` is not part of
  the cache key, so one opt-in request attached the enrichment to every
  later plain request for that page until the entry refreshed. Closed by the
  same change — the handler now enriches its own copy — and pinned by a
  test of its own, alongside tests that every serving branch of the cache
  (including stale-while-revalidate and callers parked on a cold fetch)
  hands out rows no other caller can see (audit K038).
- **sdex order book:** `/v1/sdex/orderbook` no longer serves an offer that
  was taken or cancelled in a ledger the lake's live sink dropped. The
  in-process book bounded its incremental read by the raw
  `max(ledger_seq)` of `ledger_entry_changes`, so it read across the hole
  and committed its cursor above it; when `ch-live-catchup` filled the hole
  minutes later those rows sat below the cursor and were never read — the
  removed offer stayed up as resting liquidity, and an offer created in
  that ledger never appeared, until the API restarted. Both the incremental
  read and the full load's starting cursor are now bounded by the lake's
  contiguous tip over `stellar.ledgers` (the same guard the projector and
  the cap67 derive use), read on the reader's own connection: the cursor
  holds just below a hole, logs that it is held, and resumes through it
  once it is filled. The full load looks back 100,000 ledgers for an open
  hole; a book loaded off an empty lake starts at the lake's first ledger
  instead of stalling on the never-exported ledger 1 (audit 2026-09-02
  F162).
- **sdex order book:** the book is now rebuilt from the lake once a day.
  `Load` was documented as the self-heal "if Advance ever falls
  persistently behind", but the API's maintainer loop called it once per
  process and never again, so anything that went wrong below the cursor
  stayed wrong until a restart. The loop's policy moved out of `main.go`
  into `SDEXOrderBookCache.MaintainTick`, where it is tested: initial load
  retried every tick until it lands, then advance plus the quarantine
  drain, plus a full re-load every 24 h. A failed re-load leaves the
  previous book serving and is retried after an hour, not every tick. A
  re-load keeps verification verdicts already earned — a version-tie
  suspect still served at the identical version is not re-quarantined — so
  the daily rebuild does not pull long-resting offers out of the served
  book while the probe backlog drains again. The duplicate advance/verify
  warnings `main.go` logged on top of the cache's own are gone (audit
  2026-09-02 F162).
- **ops (backfill):** a chunk that walks only PART of its range now fails
  instead of exiting 0. `stellarindex-ops backfill` errored only when it
  walked zero ledgers, while `ch-backfill` and `census-backfill` both
  fail any short walk. Every walk opts into the trailing-missing
  tolerance, and that window is measured against the walk's own `-to`
  (the chunk top), not the network tip — so for a chunk, or any request
  under 65,536 ledgers, a missing object anywhere ends the walk without
  an error. The chunk then logged `chunk complete`, refreshed the CAGGs
  over what it had and recorded itself done: `-from 62800000 -to
  62900000` against the hourly-mirrored archive walked 99,280 of 100,001
  ledgers and the 721-ledger trade hole's only evidence was a success.
  The coverage check now runs before the CAGG refresh and the completing
  checkpoint, charges a resumed chunk only for the ledgers above its
  prior cursor, and checkpoints the walked prefix so `-resume` continues
  from the truncation point. **Operator-visible:** a `-to` at or near the
  tip now exits non-zero until the archive mirror holds the range; clamp
  `-to` or re-run with `-resume`. The archive stays the default bucket —
  `opsutil.ResolveStreamBucket`'s no-seam default is the live bucket,
  which cannot hold a historic range. The `TolerateTrailingMissing`
  godoc no longer claims mid-range gaps always error (RLT-266, #695).
- **ops (projector-replay, ch-rebuild):** both re-derive commands now
  consult the `BackfillSafe` WASM-audit gate, which until now only
  `stellarindex-ops backfill` asked — while projector-replay is the
  documented catch-up procedure for every projected source and
  `ch-rebuild -write` re-decodes history with rows that win over the
  stored ones. `projector-replay -source X` refuses (dry-run included,
  before any database access) when X is not attested;
  `ch-rebuild -write` refuses when any source the run would decode is not
  attested — named in `-sources`, or selected by the default
  whole-catalogue run, which today means `sushiswap_v3` and `upshift`.
  The ch-rebuild dry-run stays ungated: it writes nothing and is how an
  unaudited decoder gets evaluated against history. **Operator-visible:**
  a `ch-rebuild -write` with no `-sources` now refuses until those two
  audits land; pass the audited list explicitly, as
  `scripts/ops/ch-rebuild-projected.sh` already does. A mistyped name in
  `-source`/`-sources` is refused too, instead of rewinding or rebuilding
  nothing with exit 0. The question is asked through the new
  `external.ReplayBackfillSafe`, which resolves the three projector
  source names that deliberately have no registry row: `blend_backstop`
  follows `blend`'s attestation (its audit covered the backstop
  contract), and `sep41_transfers` / `sep41_supply` decode a
  standard-fixed schema and keep their sanctioned recovery procedures.
  No override flag, matching `backfill`: the way through is the audit
  plus the registry flip. `projected-rebuild` is the third re-derive
  path and is NOT gated by this change (F050).
- **ops (projected-rebuild):** the third re-derive path now consults the
  `BackfillSafe` WASM-audit gate too. `projected-rebuild` builds the live
  projector's current decoder and runs it over a historical lake range
  with a winning `derive_generation` — and it is where the
  projector-replay runbook sends any rewind over ~1M ledgers — yet it
  never asked, so the bulk path could do exactly what the two gated
  commands refuse. `projected-rebuild -source X` now refuses when X is not
  attested or not a known source, before the config load and any database
  access, through the same `external.ReplayBackfillSafe` (so
  `blend_backstop`, `sep41_transfers` and `sep41_supply` keep working).
  **Operator-visible:** the refusal covers the default dry-run as well as
  `-write`, unlike ch-rebuild — this command's dry-run is a preview of the
  write run (its live-cursor guard already applies to both), and
  ch-rebuild's ungated dry-run remains the way to evaluate an unaudited
  decoder against history. No override flag. The `test/controlwiring` leg
  for this control is green on all three paths and has left the
  `k023evidence` build tag (`replay_backfillsafe_test.go`), now matching
  only real calls to the external gate rather than any identifier ending
  in `BackfillSafe(` (F050).
- **tests + docs (ch-rebuild):** the second leg of ch-rebuild's
  `BackfillSafe` gate — the one that covers a `-write` with no `-sources`,
  the widest run there is — is now pinned at its call site; deleting the
  call failed no test, because the existing coverage invoked the helpers
  directly. Two runbooks prescribed `-write` commands the gate refuses:
  `history-completeness-plan.md` §2.2 now passes `-sources sdex -sdex`
  (it also lacked the `-sdex` its `-sdex-gaps` pass needs), and
  `sep41-mint-recovery.md` §3 now passes
  `-sources sep41_supply,sep41_transfers` plus the `-config`/`-from`/`-to`
  the tool requires. A test drives both documented flag sets through both
  gate legs and checks the runbooks' command blocks (F050).
- **tests (storage):** `TestHistoryPointsDirectionUnion` no longer fails on
  every run between 00:00 and about 02:05 UTC. It seeds trades two hours
  back and asserted that the 1-day history series is empty because "today's
  bucket is still open"; just after midnight the seed lands in yesterday's
  bucket, which has closed and is correctly served. The expectation is now
  derived from the seeded timestamps — a day bucket is expected exactly when
  it has closed, and no open bucket may be served. The query was right; the
  test was pinned to the clock. Found when it failed a pre-push gate at
  00:16 UTC.
- **auth (api keys):** listing, revoking, re-budgeting and
  email-verifying a Redis-backed API key no longer walks the whole
  credential keyspace. Four store lookups answered "which records does
  this owner hold" / "which record has this KeyID" with a
  `SCAN apikey:*` plus one GET per credential in the deployment, and
  three of them sit behind `/v1/account/keys`, which any anonymously
  registered key can call: measured against 300 other customers' keys, a
  caller owning two keys cost 302 GETs per list and 128–217 per
  by-KeyID operation, and the admin tier clamp paid one full walk per
  key it lowered. Issuance (`Create`, and the `/v1/register` mirror
  `CreateWithSecret`) now writes the record and its entries in one
  lookup index — a single Redis hash, `apikey-index:v1` — as one atomic
  script, and the lookups read that: 3 commands for a list of two keys,
  no SCAN. Ownership is still decided from the record, never from the
  index. It is one hash on purpose: Redis runs `allkeys-lru`, and a
  per-owner key evicted on its own would hide live credentials from
  revoke; evicting the one hash takes its `ready` marker with it and the
  next lookup rebuilds. (F057, K051)

  **Operator note — this fix is not live on a lockdown host until the
  Redis ACL is applied.** `apikey-index:*` is a new key family and the
  `stellarindex` Redis user allow-lists key patterns;
  `redis-sentinel/templates/users.acl.j2` now grants it, but that is an
  ansible surface and a binary deploy does not carry it. Until the role
  is re-run the binary is safe and unchanged in behaviour: index access
  is `NOPERM`, keys are issued record-only, and every lookup keeps
  walking (so the cost defect is still open on that host). After the ACL
  is applied no restart and no manual backfill are needed — the first
  lookup builds the index for every existing key under a single-flight
  lock, then marks it ready. Verify with
  `redis-cli HGET apikey-index:v1 ready` → `1`. `DEL apikey-index:v1`
  is always safe and forces a clean rebuild; do that if the ACL pattern
  is ever removed and re-granted, or if keys were minted by a binary
  older than this release after the index went ready.

- **ci (api keys):** `scripts/ci/lint-apikey-scan.sh` bans any new walk
  of the `apikey:*` credential keyspace: no Redis scan-family call in
  `internal/auth`, nobody in `internal/`, `cmd/` or `pkg/` may build the
  `apikey:*` wildcard, exactly one site may carry an `apikey-scan-ok:`
  marker (the index build), and a marker that exempts nothing fails. On
  the code before the index it reports the four sites the finding
  names. The four walks that shipped were each written by copying the
  one before, and each passed its tests, because a test keyspace holds
  three keys. `internal/auth`'s `TestNoNewAPIKeyKeyspaceWalk` runs the
  script and its 13-case self-test, so the ban is enforced wherever
  `go test ./...` runs; the script is not yet named in
  `scripts/dev/verify.sh` or `.github/workflows/ci.yml`. (K051)
- **ci/ansible-drift:** `ci.yml`'s ansible syntax/lint job and
  `ansible-drift.yml`'s drift-check job (which can APPLY to production
  r1) now install the pinned collections from
  `configs/ansible/requirements.yml` after their `ansible` pipx bundle,
  the same file `deploy.yml` installs from at apply time (F146).
  Previously both CI-side jobs validated tasks against whatever
  community.general/ansible.posix/community.postgresql versions the
  bundle happened to resolve (community.general 11.x-class under
  `ansible==14.2.0`), while only the real deploy path pinned
  community.general to 9.5.0. A module argument present in the bundle
  but absent or changed in the pinned collection passed syntax-check,
  ansible-lint and drift's `--check --diff` clean and only broke — or
  silently diverged — on the real r1 apply; the drift workflow's
  `apply=true` input made this two-collection-sets-mutating-production
  risk, not just a CI-parity nit. Installing the pinned collections
  after the bundle relies on Ansible resolving the explicit collections
  path (`~/.ansible/collections`) ahead of the ones shipped inside the
  `ansible` package, so all three workflows now lint/dry-run/apply
  against the same collection versions.
- **price:** a frozen pair on `/v1/price` and `/v1/price/batch` serves
  the value the freeze is holding, not the bucket it refused (F013,
  MNY-22). `flags.frozen` promises the last-known-good (ADR-0019, and
  the flag's own OpenAPI wording), but the default path reads the raw
  `prices_1m` bucket — which the anomaly checker never gates — and
  stamped the flag on it from an independent marker read. On a frozen
  XLM/GBP a client got the newest Kraken minute, the very print the
  freeze rejected, labelled as the protected value. The handler now
  replaces that bucket with the aggregator's held value from the VWAP
  cache: the 5m window first, then 1h, then 24h, because the lifecycle
  is per (pair, window) while the marker is per pair, and the response
  carries the real `window_seconds` of whichever window held it. It is
  `stale: true` as well as `frozen: true` (it is below the closed-bucket
  baseline, and `observed_at` is the read time — the cache does not
  record when the held value was fresh), and it credits no sources,
  since the refused bucket's venues did not produce it. The freeze
  check follows the alias the bucket was actually read from, so
  `asset=native` answered from `crypto:XLM`'s bucket is governed by
  `crypto:XLM`'s freeze rather than slipping past it on a spelling. A
  frozen pair with NO held value readable (a first-bucket freeze, an
  expired key) is refused — `503 price-unavailable` on `/v1/price`, the
  row omitted on the batch — rather than published as something it is
  not; that is not the "503 instead of last-known-good" ADR-0019
  rejected, because there is no last-known-good to prefer. Unfrozen
  pairs are byte-identical. Two existing tests wired a freeze with no
  VWAP cache at all, a shape production cannot take, and so certified
  the defect (raw `0.07` under `frozen: true`); their fixtures now hold
  a value and they assert it is the one served. Not changed, and still
  reading the raw bucket with no freeze protection: `/v1/oracle/*`
  (SEP-40), the `/v1/assets` price columns, and the USD leg of the
  derived-fiat cross — none of them carries `flags.frozen`, so none of
  them makes the false claim, but none of them is protected either.
- **price:** a freeze marker on the requested spelling no longer
  discards a healthy bucket read from an unfrozen alias (F013 residual).
  The freeze check above looked at the served alias first and then at
  the requested literal, so `asset=native&quote=fiat:GBP` answered from
  an unfrozen `crypto:XLM/fiat:GBP` bucket (three venues) was still
  overridden by a marker that existed only on `native/fiat:GBP`: with
  nothing held for `native` it returned `503` — under a detail naming a
  "refused bucket" that did not exist — and the batch row vanished,
  while `asset=crypto:XLM` served `200` for the same market; with a 24h
  value held for `native`, that value replaced the healthy bucket. The
  marker that governs a response is now the marker of the pair whose
  bucket is served, and only that one: an unfrozen served pair is
  served exactly as read (`window_seconds: 60`, its sources, not stale)
  and is NOT flagged `frozen`, because the flag describes the value in
  the response and that value is not a held one. A healthy alias wins,
  the same rule the alias walk already applies to a withheld alias. The
  main fix is untouched: a frozen SERVED pair still serves its own held
  value or refuses, even when the literal holds a value of its own. The
  requested literal's marker is consulted only when no bucket was read
  at all (the fallback chain answered).
- **clickhouse ops:** `deploy/clickhouse/account_activity.sql`'s backfill
  runbook fails closed (F112). The per-account activity watermark is a
  HARD `ledger_seq <=` bound on the account-history readers, and its
  "exact upper bound by construction" invariant only holds once the
  windowed Step-2 backfill has covered every account — but the runbook
  could end looking complete without having done so. Step 2 now ends
  non-zero, and without its `COMPLETE` line, when a window's job fails,
  when the TIP probe fails or answers empty/0/garbage (the loop used to
  run no window and "succeed"), and when `run-heavy-job.sh` finds the
  per-job lock held — the wrapper exits 0 there without running the
  payload, so each payload writes a success marker and a job with no
  marker aborts the run; a closing count requires every job's marker.
  The loop is one subshell with the success line chained behind `&&`, so
  an abort neither kills the operator's login shell nor lets a later line
  of the same paste print success, and the windows are counted in bash
  rather than by `seq` (BSD `seq` prints `2e+06`). Step 3 is replaced: it
  sampled recently active accounts, a population the live MVs cover
  whether or not the backfill ran. It is now a per-window check in the
  same 2M-ledger grid — a hash sample (`AA_MOD`, default 1 in 64; 1 is
  exhaustive) of the accounts active in each window must have a
  watermark at or above their activity there, a missing row counting as
  too low — so a gap in any window is counted by that window's own
  check. It is a labelled heavy op: run under `run-heavy-job.sh`, every
  lake read scoped by ledger, `max_threads`/`max_memory_usage` capped; a
  failed or lock-skipped check aborts rather than reading as 0. Proven
  by `internal/storage/clickhouse/account_activity_runbook_test.go`
  (the runbook's own text through the shipped wrapper, both its root and
  non-root branches) and, against a live ClickHouse,
  `test/integration/account_activity_backfill_verify_test.go` (window 1
  backfilled, a later window skipped, an account active in both: the
  verify counts it, and returns 0 once the window is re-run).
- **`/v1/ohlc` multi-bar series:** an explicit `from`/`to` window wider
  than `limit` intervals now serves the NEWEST `limit` buckets instead
  of the OLDEST (RLT-453). `Store.OHLCSeries` / `Store.OHLCSeriesReBucketed`
  ended their query `ORDER BY bucket ASC` and then applied `LIMIT`, so a
  10,080-bucket window capped at the default 100 silently returned the
  first 100 minutes of a week-old window and never reached `to` — a
  stale slice for exactly the wide-window request `limit` exists to
  bound. Both readers now order `DESC` and reverse in Go before
  returning (mirrors `Store.TradesInRange`, F-1319), so the documented
  ascending-order contract is unchanged and only which end survives a
  cap is corrected. The fiat combine (`quote=fiat:USD`) was the third
  consumer of that ordering and is corrected with it: it passes `limit`
  to every constituent read, merges, and used to keep the EARLIEST
  `limit` of the merge — which, over newest-N constituent reads, is
  exactly the buckets a dense constituent was cut out of (through the
  handler, a 12h window at `limit=3` served 04:00 and 08:00 as `n=1`
  bars where the market printed 101). It now keeps the newest `limit`,
  which are inside every constituent's own newest `limit` and therefore
  complete, and for the same reason never a bucket the held-back SAC
  pass mistook for unanswered. `OHLCSeriesBar.truncated` — declared on
  the wire since F-0071 but never assigned (OpenAPI: "reserved for
  future row-cap signalling") — is now set on every bar of a response
  whose window held more closed buckets than `limit`, and only then:
  the handler reads one row past the cap to know, so a default request
  that simply fills its `limit`-interval window is not flagged. The
  envelope's `from` still echoes the requested bound; the OpenAPI prose
  says so and says how to page back.

- **assets listing:** the listing `market_cap_usd` of a confirmed
  non-7-decimals token divides supply by the token's confirmed decimals,
  not by the standard 7 (F017, the market-cap leg). This corrects the
  listing entry below, which claimed that normalising `price_usd` in the
  `asset_price_snapshot` writer needed no reader-side change: that holds
  for everything that READS the price and is wrong for the one place
  that MULTIPLIES it. The cap is supply in the token's smallest unit,
  divided by 10^decimals, times the price, so the divisor is a consumer
  of the price column's scale. While the column held the RAW ratio the
  two errors cancelled — supply / 10^7 times a price off by
  10^(7 − decimals) is the right number — and moving the price to true
  scale turned a correct cap into one wrong by 10^(decimals − 7): 1,000
  tokens of a 9-decimals contract at 2.50 USD published 250000.00
  instead of 2500.00, an 18-decimals token eleven orders of magnitude
  too much, a 5-decimals one a hundredth. `fillRowMarketCap` now takes
  the divisor from `nonstandard_decimals_assets`, the same table the
  writer joins, so price and divisor switch on one fact; it is the
  fallback the detail page already makes when it has no lake reading.
  The row's `decimals` is set to the same value, so the published
  divisor is the one the cap was computed with and the smallest-unit
  `circulating_supply` beside it scales correctly. One fix covers every
  caller of the shared fill — both `/v1/assets` listing variants, the
  catalogue-twin merge, the RWA classic listing, and the RWA contract
  listing, where this fill runs before the contract's decimals are read
  and its cap survives whenever the later contract-specific fill
  declines. A token with no confirmed row keeps 7 beside the raw ratio
  it is still served with, byte for byte.

- **asset catalogue:** the catalogue reads that serve a USD price as
  rounded text — the per-asset row's `price_usd` and the four
  price-history series (24h, 7d, and both batch forms) — no longer round
  a confirmed non-7-decimals token's RAW ratio to 10 places before the
  API can correct it (F017, the rounding leg). A flat `ROUND(raw, 10)`
  on an 18-decimals token, whose correction is 10^11, turned a 1 USD
  price (raw 1e-11) into zero and a 14 USD one into a raw 1e-10 that
  reads back as exactly 10 USD; the API could only withhold. These reads
  now round to 10 + k places where the correction is 10^k, which is the
  corrected price rounded to 10 places:
  `ROUND(raw, 10 + k) * 10^k == ROUND(raw * 10^k, 10)`. The values stay
  RAW and the multiply stays in the API, the one place that owns it —
  correcting here as well would apply the factor twice. k is floored at
  zero, so a token with fewer than 7 decimals (scaling down only shrinks
  the error) keeps the 10 places it had, and an asset with no confirmed
  row resolves to exactly `ROUND(…, 10)`: the same bytes. On the wire
  this reaches the listing sparkline, the on-chain price fallback, and —
  see the detail-page entry below — the `/v1/assets/{id}` row price and
  its 24h / 7d price histories, all of which read the rounding scale off
  the value.

- **asset detail:** `GET /v1/assets/{id}` now serves the row price and
  the `price_history_24h` / `price_history_7d` points of a confirmed
  non-7-decimals token that its flat precision floor used to withhold
  (F017, the rounding leg on the detail page). The entry above widened
  the SQL's rounding to 10 + k places, but the detail overlay's two call
  sites still treated every string as rounded to 10 on the raw scale, so
  the extra places bought nothing there: an 18-decimals token at 14 USD —
  raw 1.4e-10, 1.4 quanta at 10 places — stayed unpriced on its detail
  page while the listing beside it served 14.0000000000. Both sites now
  go through `normalizeCatalogueReadUSD`, the same function the listing
  sparkline uses, which keeps the fail-closed floor for a string that
  really was rounded on the raw scale (fewer than 10 + k places) and
  corrects a longer one exactly. The all-time-high path is unchanged: it
  was never rounded and never floored.

- **assets listing:** `?include=sparkline7d` now serves a
  decimals-normalised series for a confirmed non-7-decimals token (F017,
  the sparkline leg). The batch price-history reader returns RAW
  `prices_1m` ratios and the listing put them on the wire verbatim, so a
  9-decimals token listed at 2.50 USD over a chart that ran along 0.025.
  Each point now takes the same single factor the detail page's
  histories take, and a point that cannot be corrected becomes a
  null-priced bucket so the 7-day grid is unchanged. The same correction
  is applied to the one other raw catalogue read `assets.go` published:
  the per-asset price behind the global asset view's on-chain fallback
  (`onChainListingPriceUSD`), which read the raw per-asset row, not the
  writer-normalised listing rollup its name suggests. Whether the
  precision floor applies is read off the value itself:
  `ROUND(x, n)::text` carries exactly n fraction places, so a string
  with at least 10 + k places for a 10^k scale-up was effectively rounded
  after the correction and is corrected exactly, while a shorter one was
  rounded on the raw scale and keeps the fail-closed floor (an
  18-decimals token's `0.0000000001` is withheld, not served as exactly
  10 USD). Byte-identical, with no parse, for every asset with no
  confirmed row.

- **assets listing:** `price_usd` on `GET /v1/assets` is now
  decimals-normalised for a confirmed non-7-decimals token (F017, the
  listing leg). The column is read from `asset_price_snapshot`, whose
  writer stored the RAW `prices_1m` ratio, so a 9-decimals token listed
  at a hundredth of its price, a 5-decimals one at a hundred times it,
  and an 18-decimals token worth $14 as `0.0000000001` — the listing's
  `ROUND(…, 10)` ran on the raw ratio and left nothing to correct. The
  same column feeds `ContractCatalogueRows`, and through it the RWA
  contract listing, which multiplies that price by supply to publish a
  market cap. The correction is applied **in the rollup's writer**, one
  exact factor of 10^(decimals − 7) joined from
  `nonstandard_decimals_assets`, and not at read time like the rest of
  the class. `/v1/changes` had to normalise at read because its upsert
  ratchets ATH/ATL with GREATEST/LEAST and a write-side switch would pin
  an extreme from the old scale; this rollup has no such hazard — the
  upsert overwrites every column, the table is recomputed every
  2-minute pass and pruned — which a test proves by withdrawing and
  restoring a confirmation and reading the stored value flip with no
  residue. Writing it once corrects every READER of the column (none of
  them normalised) — but not the market cap, which multiplies the column
  by a supply carrying its own scale and needed its divisor moved with
  it; see the market-cap entry above. It keeps the multiply on unrounded
  NUMERIC so the wire
  rounding now happens after the correction, and leaves the 1h/24h/7d
  change columns alone — each is a ratio of two same-scale legs. An asset
  with no confirmed row stores and serves the byte-identical value it
  always did. Readers of the column must not normalise it again; a unit
  test fails if the listing SELECT ever joins the decimals table.
- **ops (projector-replay):** `stellarindex-ops projector-replay -source
  sep41_supply` now resets the `sep41_supply_rollup` fold checkpoint after
  rewinding the cursor (F024). `AdvanceSEP41SupplyRollup` only ever folds
  `ledger > last_ledger`, so a replay's whole point — re-driving a row the
  projector's own held-row retry gave up on (quarantined) or correcting a
  row already written — lands at-or-below that checkpoint and was
  permanently invisible to the fold and to `SEP41KindTotalsAtOrBefore`'s
  fast path, no matter how many times the replay ran. `ch-rebuild -sep41
  -write` already reset the fold for its own re-derive path
  (`sep41RollupResetPlan`); the projector's replay path had no reset at
  all. The reset is a FULL reset (every watched contract), matching the
  fact that a source-level replay re-walks every contract over the
  rewound range, not just the one row that triggered it; it is safe by
  construction — the reader falls back to the exact full-sum aggregate
  until the worker re-folds, so served supply stays correct throughout.

- **docs (timescale):** `Store.ResetSEP41SupplyRollupFold`'s docstring now
  names `stellarindex-ops projector-replay -source sep41_supply` alongside
  `ch-rebuild -sep41 -write` as a caller (F107, a duplicate audit finding
  of F024's already-fixed defect above). The doc previously described only
  the ch-rebuild recovery path, so an operator reading it for the
  behavioral contract had no way to tell that the projector's replay path
  depends on the identical reset.

- **api/auth:** `PATCH /v1/admin/accounts/{id}` no longer **permanently
  destroys the account's `/v1/register` API keys** under the default
  `auth_backend=redis` (F056, K050, Q145). The "key cache invalidator"
  was wired whenever Redis was configured, but `apikey:<hash>` is a
  rebuildable read-through cache only under `auth_backend=postgres`; on
  the redis backend that key IS the credential (the register mirror), the
  plaintext is shown once and Postgres keeps only its hash — so any
  override change (a quota or rate-limit RAISE included), any suspension
  and any tier lowering that clamped a key DELeted the customer's key
  with no recovery path, and a suspend-then-reinstate left the account
  active with nothing that could authenticate. The credential stores are
  now assembled by `v1.NewAPIKeyBudgetStores`, which takes
  `auth_backend` and wires the invalidator
  (`auth.NewKeyCacheInvalidatorForBackend`) for postgres only; on redis
  the tier clamp already rewrites the canonical record in place and the
  validator's account-status gate enforces suspension. Postgres-backend
  eviction is unchanged. Proven against real Redis + Postgres through the
  real register and PATCH handlers
  (`TestAdminAccountPatch_PreservesRegisterCredential_RedisBackend`), with
  an AST guard keeping `main.go` on the backend-aware constructor. Keys
  already deleted by this defect cannot be restored — affected accounts
  need a new key minted.
- **completeness (clickhouse-lake):** the substrate axis (Claim 1) now scans
  each source's own `[genesis,tip]` range instead of reusing one lake-wide
  scan's single result for every source (F073). The shared scan called
  `SubstrateProblem` ONCE at the run's global floor and returns on the
  FIRST problem it finds; feeding that single earliest value into every
  source's `problem < genesis` test meant a hole below a high-genesis
  source's own start silently masked a SECOND, later hole INSIDE that
  source's own range — the scan never got far enough to find it, so the
  source published `substrate_ok=true` over a range that demonstrably had a
  problem. `substrateForGenesis` now scans from `max(run-floor,
  source-genesis)`, memoised per distinct floor so sources sharing a genesis
  reuse one scan.
- **completeness (clickhouse-lake):** the same per-source substrate scan
  (F073, above) also closes a head-truncation blind spot (RLT-123): the
  shared scan's endpoint-presence head guard tripped on ANY truncation at
  the low end of the run's global query and returned immediately, before the
  windowed contiguity/hash walk ever ran — so a lake truncated below a
  low-genesis source (sdex, genesis 2) skipped the walk that would have
  found a real interior gap or hash break in a high-genesis source's own,
  otherwise-untruncated range. Scoping the scan to each source's own genesis
  means a truncation entirely below it no longer trips that source's guard.
- **docs(clickhouse):** `tier1_schema.sql`'s comment above `tx_hash_index`
  claimed the reader falls back to a bloom scan on any index miss —
  the opposite of what `ExplorerReader.TransactionByHash` has done since
  the 2026-07-30 account-filter class audit, which made a miss against a
  NON-EMPTY index authoritative (F106). Corrected to state the true
  contract and mark the one-time `ch-txindex-backfill` as a correctness
  PREREQUISITE on any lake with prior history, not a deferrable
  performance optimisation. The reader's remaining gap — its
  availability probe proves the index is non-empty, not that the
  backfill has finished — needs a completion signal from
  `internal/ops/chops/ch_txindex_backfill.go` that this package cannot
  add alone (NEEDS-COORDINATION; a naive row-count coverage check was
  tried and reverted, see the KNOWN GAP note in `tier1_schema.sql`).
- **clickhouse:** `ch-schema-drift.sh` now compares secondary INDEX
  declarations (name + TYPE + params + GRANULARITY) between the repo's
  intent and the live schema as real drift, not silence (T339). The
  checker's column parser previously discarded `INDEX` clauses inside a
  `CREATE TABLE` outright, so `ledger_entry_changes.idx_lec_key_xdr`'s
  bloom-filter false-positive rate could be retuned in
  `tier1_schema.sql` (or left un-applied on a live host via the
  operator-run `ledger_entry_changes_key_xdr_index_fp.sql` migration)
  and this checker would report "no drift" either way.
- **api:** four more surfaces now apply the dex-nonstandard-decimals
  normalisation instead of publishing the RAW `prices_1m` ratio for a
  confirmed non-7-decimals token (F017). The class was half-fixed, and
  the half that remained was worse than the whole: `/v1/assets/{id}`
  normalised `price_usd` on its canonical path, so **`change_24h_pct`
  divided a corrected price by an uncorrected 24h-ago anchor** — for a
  9-decimals token a flat market served about +9900%, where two raw legs
  had at least cancelled. The anchor (`storeChange24hReader`, shared by
  the detail page and the `/v1/price` batch row) is now normalised
  against the pair it was actually read from, the peg fallback included;
  for a flagged pair an anchor that cannot be corrected reads as "no
  anchor" rather than being served raw. On the same payload, the
  one-hop transitive `price_usd` — which exists precisely for
  Soroban-native contracts, the only class that can be non-7dp — the
  catalogue-row `price_usd`, `price_history_24h`, `price_history_7d`
  and `ath` are normalised too. One factor serves all of them,
  10^(asset decimals − 7): every leg these readers chain through (a USD
  proxy, XLM, or an intermediate hop) is either on the standard scale or
  cancels, which a test pins by flagging the hop and asserting nothing
  moves. The catalogue SQL rounds to 10 places BEFORE the correction can
  run, and scaling up promotes that rounding error by the same power of
  ten — an 18-decimals token worth $1 comes back as zero, one worth $14
  as exactly $10 — so a scaled-up value that kept fewer than 1000
  rounding quanta is withheld as a gap (the series keeps its bucket
  grid; a withheld row price falls through to the full-precision
  transitive fill) instead of being published as precise-looking
  fiction. The un-rounded ATH is exact and never floored.
  `/v1/changes/{entity_type}/{id}` normalises `current_value`, the four
  window values and the ATH/ATL **at read time, deliberately not in the
  rollup worker**: the upsert ratchets `ath_value`/`atl_value` with
  GREATEST/LEAST and a token is only flagged after it has been trading,
  so switching a row's scale at write would pin the ratchet to an
  extreme from the old scale for good. `change_summary_5m` stays raw
  like the CAGG under it; the percentages, streak and acceleration are
  scale-free and unchanged. A `pair` row's factor is exact (the id is the
  source pair); a `coin` row does not record which pair it was computed
  on, so its quote leg is taken as the standard scale. Every path is a
  byte-identical no-op for an asset with no confirmed row — no parse, no
  reformat — so no served value moves for any 7-decimals asset.
  The `/v1/assets` LISTING's `price_usd` and its `?include=sparkline7d`
  series, left out of that change, are covered by the three entries
  above.

- **customer webhooks:** a timed-out delivery is no longer re-POSTed
  forever (K025, webhook leg). The write recording a delivery's outcome
  ran on the attempt's context, whose deadline starts before the webhook
  lookup and the POST and so expires no later than the HTTP client's own
  timeout. A customer endpoint that hung — the
  commonest way a delivery fails — reached `MarkAttemptFailed` with a
  dead context; the write failed, `attempt_count` never advanced, and the
  row kept its claim lease and was re-POSTed every 5 minutes into the
  same timeout, forever, with `MaxAttempts` powerless because no attempt
  was ever counted. Every outcome write (`MarkDelivered`, the retry mark,
  the terminal mark) now goes through one `Worker.mark` chokepoint on a
  context detached from the attempt (`context.WithoutCancel`) and bounded
  by its own 1s `markWriteTimeout`. Detaching also covers shutdown: an
  event the customer has already accepted is recorded delivered instead
  of being re-sent when the lease expires. The mark budget is a new term
  in the double-delivery invariant, and the compile-time guard now holds
  `BatchLimit × (Timeout + markWriteTimeout)` under the lease
  (25 × 11s = 275s < 300s; the margin goes from 50s to 25s).
- **aggregator:** one store call that stops answering no longer stalls
  every price (K025, aggregator leg). `Orchestrator.Tick` ran on the
  process-lifetime context with no deadline, so a single wedged call —
  the trades fetch, either FX snap query (triangulation or the
  composite-reference evaluator), the freeze record, the divergence
  refresh — stalled every published price until a restart. A tick now
  runs under `Config.TickTimeout`, inherited by every call it makes. It
  is a wedge guard, not a budget: the default is 4 × `Interval` (120s, the
  `stellarindex_api_price_stale` threshold), because `refreshOrder` is
  fixed and a bound a slow-but-healthy tick could exceed would starve the
  same tail pairs every tick. A cut tick skips triangulation and the
  divergence pass (no composite over a partial edge set), counts as
  `ticks_total{outcome="error"}`, still emits the staleness gauges so
  they climb rather than freeze, and returns an error wrapping
  `context.DeadlineExceeded`; the next tick starts with a fresh budget.
  A caller cancellation (shutdown) is unchanged. **Operator note:**
  `TickTimeout` is not yet settable from TOML, and ticks have no duration
  metric — if `tick cut by its 2m0s wedge guard` appears in the
  aggregator log on a healthy database, ticks are legitimately slower
  than 120s and that, not the guard, is the finding. The third K025 site,
  the shared SEP-1 image refresh, was already detached and needed no
  change.
- **price/at, price/changes:** the thin-market gate now judges the
  market that existed AT the requested instant. `/v1/price/at` and every
  `/v1/price/changes` horizon serve the bucket at-or-before a past `ts`,
  but the gate deciding whether to serve it measured the trailing 24
  hours ending NOW, and the two are unrelated. It was wrong both ways: a
  market that was deep and honest at `ts` but is dormant today had its
  whole history withheld (a cost-basis read 404'd for data we hold and
  trust), and a market that is thick today but was attacker-seeded dust
  at `ts` PASSED, so the historical read published exactly the seeded
  price the gate exists to refuse. Substance is now measured over the
  policy window ending at `ts` (`Store.PairMarketSubstanceAt`,
  `SubstanceGate.AllowedAt`), through the same `priceWithheld`
  chokepoint and with the scam half unchanged. An instant the reader can
  answer from a raw minute bucket (within 48h — now one shared constant,
  `timescale.PriceAtMinuteRungMaxAge`) keeps the live floor unweakened
  at minute grain; an older instant, served from hour/day bars, is held
  to the same volume and span legs on `prices_1h` with a two-bucket
  floor — the largest hour floor that never refuses a market the minute
  floor admits. Live surfaces are untouched. Operators: historical
  verdicts depend on `prices_1h` being materialised as deep as
  `prices_1d`; where it is not, old instants read as "no market" and are
  withheld (T038).
- **dex tvl:** a USD price read that ERRORS is no longer published as
  "nobody prices this token". `rateFor` folded a resolver error into the
  unpriceable outcome and memoised it for the refresh, so the leg read
  `excluded: no_served_price`, the protocol's refresh succeeded, and the
  carry-forward — which runs only on a refresh error — could not fire:
  one transient Postgres error on the XLM rate dropped every XLM leg
  from the published DEX TVL (the fixture's soroswap figure went from
  $25.50 to $10.50) and the shrunken total was admitted as fresh. An
  error is now its own outcome. It is remembered for the refresh, so a
  failing store is asked once rather than once per protocol, and the one
  function that builds a protocol result refuses a pass that met one, so
  the protocol serves its previous figure with `carried_forward: true`
  and the refresher logs the cause — the same path a failed reserve read
  already took. Judged per protocol: one that never held the unreadable
  token still publishes this cycle's figure. A token with genuinely no
  served price is unchanged. A read cancelled by the refresh's own
  deadline arrives as an error too, so it takes the same path. No wire
  change. (#580; RLT-090, RLT-239)
- **trades (usd_volume):** the base-anchored tier now takes the same
  bound the quote-side FX tier does. For a non-XLM base the anchor read
  the identical resolver rate — usually the tier-3b `<token>/XLM x
  XLM/USD` bridge, writable for the price of `bridgeLegMinUSDVolume` —
  and stored `base_amount / 1e7 x rate` verbatim, with neither the
  two-leg cross-check nor the `singleLegMaxUSDVolume` ceiling. Planting
  `TOKEN_A/XLM` and swapping base=`TOKEN_A` against a never-priced quote
  made the FX tier decline and the anchor fire, so the $182M fake-print
  class stayed open through the base leg. Both guards move out of
  `tradeUSDVolumeViaFX` into one `boundUSDVolume`, called by both tiers
  (the quote side is unchanged: its cross-check and ceiling tests pass
  untouched). The bound sits inside the anchor rather than at the
  waterfall's call sites, so the `usd-volume-restamp` tiers, which reach
  the anchor directly, inherit it and a backfill cannot re-write what
  the live path refuses. XLM-anchored values are exempt and
  byte-identical: the rate is a direct XLM/USD market and the amount is
  XLM that moved, so the value is exact. Not in this change: the DEX TVL
  valuer still consumes the resolver rate uncapped behind its substance
  gate. (audit-2026-09-02 F044; K045, insert path only)
- **aggregator (freeze, money path):** the durable freeze ladder no
  longer trusts a `window_ladders` map the pair-level columns have
  outrun. Migration 0163 stays applied across a binary rollback
  (migrations rule 9), and the previous binary keeps ADVANCING the four
  0119 columns while never touching the map — so after a roll-forward
  inside one freeze the map is stale, and the reader preferred it simply
  because it was non-NULL: with the columns saying escalated, held 30
  minutes, the 1h window rehydrated the map's older rung, not escalated,
  4 minutes left, and resumed auto-unfreezing. When the columns record a
  worse freeze than the map's own summary (escalated where no entry is, a
  higher rung, a later hold) every held entry is now raised to the
  fail-closed fold of itself and the pair-level ladder, and that ladder
  is carried as the unowned `"0"` entry for windows the map does not
  name. Folded, not replaced: the old binary's 5m window can overwrite
  the columns with a fresh LATER hold while the map still holds a 1h
  escalation, and that survives too. The write path reads the row through
  the same rule, so the record converges on the next tick. This binary
  only ever writes the columns as the map's summary, so the rule never
  fires on a row no other binary touched — pinned through the production
  writer on real TimescaleDB with nanosecond holds, and, because the
  column is whole microseconds while the map keeps nanoseconds, the hold
  is compared at stored precision (the driver truncates today; the rule
  does not depend on it). The migration header and `migrations/README.md`
  claimed the rollback worst case was over-holding; that was false when
  written and is corrected — with this change it is true. Also adds the
  executing test for the 0163 DOWN beside a compressed chunk and a live
  escalated freeze. (audit-2026-09-02 F043, F036)
- **docs (ADR-0019):** amended to record that the freeze lifecycle runs
  per (pair, window) and that both records of it — the Redis marker and
  the durable ladder — are now window-scoped while the marker's PRESENCE
  (`flags.frozen`, the operator override) stays pair-scoped; the
  fail-closed rule for records with no recorded owner, and for a
  `window_ladders` map gone stale under a rolled-back binary; the three
  rules for anything that writes the marker (a release asks the record —
  the marker AND the durable ladders behind it — the lifecycle-free
  writer sets only the serving flag, no write shortens a sibling's hold);
  and that "frozen" for triangulation is not "frozen this tick".
  Describes the freeze entries around it — the one above and the four
  below — as shipped.
  (audit-2026-09-02 F011, F036, F043, F068, K003; RLT-261)
- **aggregator (freeze, money path):** a frozen pair's last-known-good
  price can no longer be laundered through triangulation on the ticks
  AFTER the one that froze it (MNY-22's second half). The guard read a
  set rebuilt every tick and written only from the freeze step, but
  `refreshPairWindow` returns before that step when the window is empty,
  under `min_usd_volume`, or has no VWAP — and a pair whose thin venue
  was just manipulated is exactly the pair whose next bucket is empty.
  On that tick the pair was still frozen, with its marker and its
  last-known-good value both deliberately still in Redis, yet in
  nobody's per-tick set, so the chain read the value as a fresh leg and
  published the product to a target carrying no frozen flag.
  `frozenLeg` now also honours a live freeze this process is holding for
  the window, bounded by the hold plus the marker grace — the span the
  value can still be read back — so an unevaluated ladder cannot refuse
  a chain indefinitely. The same guard protects a self-frozen
  triangulation target from being overwritten by a fresh composite on
  such a tick. (review 2026-09-17 RLT-261, #691)
- **aggregator (freeze, money path):** a window's auto-release no longer
  clears a sibling window's freeze it has never seen. The pair's marker
  is deleted by the last window to release, and "last" was decided from
  the in-memory ladder map alone — which a window only enters by reaching
  the freeze step in the current process. A window under
  `min_usd_volume` never does, and after a restart none has yet, so its
  freeze exists only in the marker: a recovering 5m window deleted the
  marker AND retired the durable ladder out from under a 1h sibling that
  had ESCALATED ("stays active until manual unfreeze"), whose next
  qualifying bucket — cold, with no prev-VWAP comparator to re-fire on —
  then published. The release now goes through
  `freeze.Writer.ReleaseWindow`, which asks the record rather than the
  process: the marker first, and — because the marker is only the first
  place the record lives — the durable per-window ladders behind it
  whenever the marker names no sibling. That second read is what covers
  a Redis loss, the one situation migration 0163 exists for: an absent,
  undecodable or pre-window marker used to read as "no sibling", and the
  clear that followed retires the WHOLE durable record, so a 5m window
  recovering while the marker was gone still ended an escalated 1h
  sibling's freeze. If another window still owns a live ladder in either
  place (or either carries a live unowned one) only the releasing
  window's ladder is retired, in both, and `flags.frozen` stays;
  otherwise the marker is cleared as before. A marker OR a durable record
  that cannot be read is left alone rather than cleared — not knowing
  whether a sibling is frozen is no ground for unfreezing it. The
  operator override (`stellarindex-ops freeze-unfreeze`) is unchanged and
  still ends every window: it retires the durable record itself, so the
  releases that follow find no sibling anywhere. Regressions at three
  levels, the orchestrator one on a fixture that wires a ladder store the
  way `cmd/stellarindex-aggregator` does — the window-isolation fixture
  wires none, which is how the first cut of this fix shipped green with
  the durable half unreached. (audit-2026-09-02 F011, K003)
- **aggregator (freeze, money path):** the lifecycle-free
  `freeze.Writer.Mark` — the triangulated-composite refusal's writer,
  whose targets are members of the aggregator's own pair set and whose
  call sits on the `ErrNoRoute` branch ahead of the guard that protects a
  self-frozen target — no longer rewrites a marker a live ADR-0019 ladder
  owns. It used to replace the marker's `remaining hold + grace` TTL with
  its flat five minutes, zero the pair-level state a legacy marker keeps
  its only ladder in, and relabel an escalated freeze as an inherited
  one; and as the first writer back after a Redis loss it re-created the
  marker WITHOUT the durable ladder, which is never consulted again once
  a marker is present. Mark now leaves an owned marker as it found it,
  and carries the durable ladders into one it has to re-create. The
  marker's single TTL is floored at the longest `remaining hold + grace`
  of any live ladder in it, for every writer, so a 5m window's short
  remainder cannot truncate a 1h sibling's hold either.
  `inheritLegFreeze`'s signature and its flat `cachekeys.FreezeTTL`
  last-known-good refresh are unchanged. (audit-2026-09-02 F068, K003)
- **aggregator (freeze, money path):** the DURABLE ADR-0019 freeze
  ladder is now recorded per aggregation window (migration 0163,
  `freeze_events.window_ladders`). The 0119 ladder columns are keyed
  (asset, quote) while the lifecycle runs one state machine per (pair,
  window) and every frozen window mirrors its ladder on every tick, so
  the durable record was whichever window wrote last. It is read at
  exactly one moment — after Redis has lost the marker — and at that
  moment a 1h window that had ESCALATED ("stays active until manual
  unfreeze") rehydrated a 5m sibling's fresh ten-minute ladder and
  resumed auto-unfreezing, while windows that were never frozen
  rehydrated the same freeze. Each window now reads, advances and retires
  only its own durable ladder; the first re-mark after a Redis loss
  rebuilds the marker with every window's ladder; and a window that
  releases while a sibling stays frozen has its durable entry retired
  with its marker entry. The window dimension is a jsonb map on the
  pair's open row, not a row per window: one `freeze_events` row stays
  one freeze event, so `/v1/anomalies`, the `anomaly.freeze` webhook and
  `stellarindex-ops freeze-unfreeze` are unchanged. The 0119 columns stay
  as the fail-closed summary (furthest hold, highest rung, escalated if
  any window is), which is what the recovery worker and `freeze-unfreeze
  -list` keep reading. A row written before 0163 has no recorded owner
  and keeps rehydrating onto every window rather than being dropped.
  (audit-2026-09-02 F043, F036, F011, K003)
- **markets (internal):** the static head of the `/v1/markets` listing
  query — the `prices_1d` active-pair CTE and the 24h `prices_1m` CTE —
  moves out of `buildDistinctPairsQuery` into one package-level literal,
  `distinctPairsActivityCTEs`, the shape `perSourcePoolsCTE` already
  uses. The F027/F028 fix below took the function to 101 lines against
  the 100-line limit; the limit ignores Go comments, so it was the SQL
  that crossed it, and the SQL is what moved. No statement changes: the
  composed query text is byte-identical across both orderings, with and
  without a cursor and an asset filter (sha256 compared before and
  after). (audit-2026-09-02 F027, F028, K031)
- **ops (runbook):** `docs/operations/runbooks/projector-replay.md`
  now describes the command that ships. It still promised a wall time of
  "≤ 5 s" and an impact of "None" for a command that, since the K006
  fix below, blocks by default until the projector has re-walked the
  rewound range (up to `-wait-timeout`, 30 min) and then re-materializes
  seven continuous aggregates — an operator running it from a bare ssh
  session on that advice would lose it mid-refresh. `-wait`,
  `-refresh-caggs` and `-wait-timeout` are documented with their exit
  semantics, including the two things the command does NOT do: a re-run
  with the same `-from` rewinds again rather than resuming, and
  `twap_1h`/`twap_1d` are outside the refresh set. The `sep41_*` source
  names are corrected to the underscored registry spelling. In the same
  change the post-rewind tail of `projectorReplay` moves into
  `rematerializeReplayedRange` (behaviour unchanged; the command was
  over the cyclomatic limit), which gives it a store seam: the wiring
  guard now pins both hops, and the tail is tested directly — it
  refreshes every view over exactly the replayed range, and never
  refreshes ahead of the projector. (audit-2026-09-02 K006)
- **ratelimit (security):** the limiter can now charge a request more
  than one token. It had no notion of cost at all: `Bucket.TakeN`'s
  third argument is the per-subject LIMIT, and the Lua script did a
  plain `INCR`, so nothing a caller could pass made a request dearer.
  `Bucket.Charge(ctx, key, cost, limit)` spends `cost` tokens in one
  atomic `INCRBY` round-trip (`TakeN` is now `Charge` at cost 1, so
  every existing caller is unchanged), on the Redis path and on the
  in-process fallback that enforces the limit when Redis is absent at
  boot. Cost is normalised into `[1, effective limit]`: a zero or
  negative cost still spends a token (a negative `INCRBY` would refund
  budget), and a cost above the ceiling spends the whole window rather
  than being refused in every window. The script's expire-on-create
  branch keyed on `current == 1`; it now keys on `current == cost`, or
  a key first written by a weighted charge would never drain (F046,
  reverification-2026-09-18).
- **api (security):** `GET` and `POST /v1/price/batch` now cost one
  rate-limit token PER ASSET ID instead of one per request. One token
  used to buy a whole batch — up to 1000 ids on the POST route, each an
  alias-looped price read plus the fallback chain, sixteen at a time
  against a 25-connection pool — so the deployed 6000/min anonymous
  budget was really six million price resolutions a minute, and a
  handful of POSTs from one unauthenticated client could pin the pool.
  The limiter still takes its one base token before dispatch, which is
  all it can know there (the POST cost is the length of an array in a
  JSON body, and pricing that ahead of the limiter would mean decoding
  up to 1 MiB for a caller not yet admitted). The handler then re-prices
  the request through the new `middleware.ChargeRateLimit` once the ids
  are parsed and BEFORE any is resolved: a denied batch is a 429 with
  `Retry-After` and no database work. The cost is the de-duplicated id
  count, and a request rejected as malformed costs the base token only.
  A batch priced above the caller's whole per-minute ceiling (1000 ids
  against the 60/min default) is served into an untouched window and
  spends all of it, rather than being refused in every window behind a
  `Retry-After` that never comes true. `X-RateLimit-Remaining` reports
  the post-charge figure. **Integrators:** a batch now draws down the
  same per-minute budget as the equivalent single-asset reads would;
  size batches against `X-RateLimit-Remaining` (F035, F046, K009,
  RLT-160).
- **api (security):** `GET /v1/assets` is now charged by the query plan
  a request selects, and its free-text `q` is bounded. The route is one
  URL and several plans, picked by the query string, and every one cost
  a single rate-limit token: the volume-ranked listing, measured on r1
  at 1523 ms against 82 ms for the default ordering at the same limit,
  and the `q` search — three unindexed `LIKE` predicates over the
  ~190K-row spine, behind a cache keyed on `q` verbatim, so a caller
  cycling values misses it every time. The volume-ranked plan now costs
  10 tokens and a `q` that reaches the store 5 (14 together), charged
  after validation and before any read, so a denied request is a 429
  that touched nothing. The price follows the PLAN, not the parameter:
  `asset_class=all` reaches the same volume-ranked store read as
  `order_by=volume_24h_usd_desc` and is charged the same, where pricing
  only the named parameter would have left the other as the way round
  it. The class-scoped listings (`fiat` / `stablecoin` / `crypto`) and a
  deployment with no assets store select no plan and still cost one
  token. The weights are deliberately below the measured ratio — a wrong
  weight should under-charge an abuser, not lock out the explorer — and
  are capacity knobs to re-derive from the slow-request log. `q` longer
  than 100 bytes is now a 400 (`invalid-parameter`) on every path; the
  longest value that can match a row is a 69-byte classic asset id
  (K009).
- **aggregator (alerting):** `stellarindex_price_staleness_seconds` no
  longer reads fresh through a per-quote price outage (F067). The
  aggregator stamped each successful VWAP publish under the pair's BASE
  asset alone, so with the shipped pair set (`crypto:XLM`, `native`,
  `crypto:BTC`, `crypto:ETH` × USD/EUR/GBP) three quotes shared one
  timestamp: every `XLM/USD` publish reset the clock a dead `XLM/GBP`
  was judged by, and `stellarindex_api_price_stale` — the only
  serving-freshness alert — stayed silent while
  `/v1/price?asset=crypto:XLM&quote=fiat:GBP` served nothing. Writes are
  now stamped per (base, quote), and the gauge for an asset is the age
  of its STALEST configured quote. The native ↔ `crypto:XLM` merge is
  applied within a quote only (either form's write answers a lookup for
  that quote; neither says anything about another quote), and both
  labels still carry the same value regardless of `aggregate.pairs`
  order. The gauge keeps its single `asset` label, so the alert rule,
  runbook and dashboards are unchanged — it names the asset, not the
  quote. A pair's clock is stamped by BOTH writers of its served VWAP
  key: the direct publish and the triangulation composite
  (`publishComposite`, on a confident publish only — never on
  `low_confidence`, `missing_leg`, a frozen leg or a Redis error). A
  configured pair served only through its chain, such as a thin
  `crypto:XLM/fiat:GBP` priced as XLM/USD × USD/GBP, therefore reads
  fresh while it publishes and climbs when its chain goes dry; it is a
  served pair, not a dead feed. **Operator note:** a configured pair that
  NEITHER writer publishes — no direct trades clearing
  `aggregate.min_usd_volume` in any window and no publishing chain — was
  previously masked by its siblings and will now raise
  `stellarindex_api_price_stale` for its base asset. That is hidden state
  becoming visible. Find the quote from the asset, then: restore the
  feed; or, where the pair has a deep USD market, give it a
  `[[aggregate.triangulations]]` chain through USD (check `fx_quotes`
  carries the fiat leg); and drop it from `aggregate.pairs` only if it
  is not meant to be served at all. Regression tests drive the real
  `Tick` → publish → gauge path
  (`TestTick_DeadQuoteIsNotMaskedByALiveSiblingQuote`,
  `TestTick_XLMDualFormIsMergedPerQuote`,
  `TestTick_CompositeServedPairReadsFresh`,
  `TestTick_CompositeThatDoesNotPublishStillClimbs`).

- **aggregator (money):** the ADR-0019 freeze marker now carries one
  lifecycle ladder PER aggregation window instead of a single
  pair-level one. The `freeze:<asset>:<quote>` key is pair-scoped
  because its presence is the `flags.frozen` the API serves for the
  whole pair, but the ladder inside it advances per (pair, window) —
  so one ladder per marker could only ever be one window's, with
  nothing recording whose. `refreshPairWindow` drops a window under
  `min_usd_volume` BEFORE the VWAP, confidence and freeze steps, so a
  thin window's key never enters the in-memory ladder map however many
  ticks pass; its first bucket above the floor was therefore a COLD key
  that rehydrated the marker wholesale, adopting a SIBLING window's
  `fired_at`, `hold_until`, `extensions_used` and `escalated`. That
  window then served a last-known-good price with nothing wrong with
  it, kept the `severity:page` sustained-freeze rule firing, and — once
  the inherited ladder escalated, which the ADR holds "until manual
  unfreeze" — could only be ended by an operator. Each window now
  writes, reads and retires its own ladder (`ladders: {"5m0s": …,
  "1h0m0s": …}`), merged into the shared marker so a write for one
  window never disturbs another's, and a window that auto-releases
  while a sibling is still frozen has its ladder retired from the
  marker rather than left for the next restart to resurrect. Because
  every frozen window's ladder is recorded, a restart still rehydrates
  EVERY frozen window: a single owning-window tag would have stranded
  all but one, and a restarted process has no prev-VWAP comparator, so
  a stranded window cannot re-fire on its own signal — it publishes the
  manipulated bucket the freeze existed to withhold. The two records
  that predate the window dimension keep answering pair-wide for the
  same reason, carried in the marker as an explicit `unowned_ladder`:
  a marker written before this change, and the migration-0119 durable
  ladder — keyed (asset, quote) with no window column — which is the
  only authority left when Redis has lost the marker, so the first
  window to re-mark during that recovery must not narrow "this pair is
  frozen" into "only I am". An unowned ladder is a snapshot nobody
  advances, so it retires itself on the same `LadderStillLive` bound
  the durable rehydrate already uses, by which time every genuinely
  frozen window has claimed its own entry. Presence semantics are
  untouched: `flags.frozen` stays pair-wide and `stellarindex-ops
  freeze-unfreeze` still releases every window. (audit-2026-09-02 E1)
- **monitoring (storage):** `stellarindex_timescale_job_failures_climbing`
  can now fire for a slow TimescaleDB job. The counter is per job, so a
  job scheduled every T accrues at most `6h / T` failures in the rule's
  window — `increase(...[6h]) > 10` therefore required a schedule under
  ~36 minutes, and every compression policy (12h) plus five of the CAGG
  refresh policies were arithmetically unable to trip it, which is
  precisely the set r1's probe found failing 66-81%. A second arm, 3 or
  more failures in 3 days, judges the slow half of the fleet. Measured on
  r1 while adding it: `policy_compression` on `trades` had failed 6 times
  in 7 days with "Failed to convert '1' chunks to columnstore" and
  nothing could fire on it, while the only other failing job in that week
  had a single failure — below the new threshold. Both rule trees and the
  runbook updated. (audit-2026-09-02 F160)
- **api (observability):** `stellarindex_dependency_up{dependency=
  "clickhouse"}` is now published when ClickHouse is configured but was
  unreachable at API start. The readiness checker was appended inside
  the success branch of the boot dial, so the one state the alert's own
  annotation calls "the only signal that it is gone" produced no series
  at all — and the alert is `stellarindex_dependency_up == 0`, with an
  in-file rationale deliberately rejecting `absent()`, so it had nothing
  to match while every lake-backed endpoint 503'd. A configured lake now
  always registers the checker; when the boot dial failed it reports
  down with an error saying a restart is what re-wires the ten
  lake-backed seams, since none of them is re-dialled. A deployment with
  no lake configured still publishes nothing. (audit-2026-09-02 F122)
- **data-freshness (observability):** a feed dead long enough no longer
  deletes its own alarm. Each per-source leg of the watchdog enumerated
  its sources from the same window it judged them in (`WHERE ingested_at
  > now() - 30 days GROUP BY source`, 7 days for supply), so a source
  dead past that window left the GROUP BY entirely: its
  `stellarindex_data_freshness_stale` series went ABSENT rather than to
  1, Prometheus aged it out, and the `== 1` alert RESOLVED — the
  watchdog went quiet the worse the outage got. The universe is now
  every source the table has ever held (the scan cost is unchanged: the
  oracle predicate was on `ingested_at`, not the hypertable's time
  dimension, so it never pruned a chunk), a domain that has never
  observed anything reads stale rather than rendering an empty value the
  publication guard withholds, and the CS-102 per-asset supply window is
  anchored to the newest supply row instead of `now()` so an all-asset
  freeze no longer empties the CTE and reports a literal healthy 0.
  (audit-2026-09-02 F149)
- **continuous aggregates (completeness):** an emptied price aggregate
  is now detected, and a replay re-materializes the range it rewrote.
  Migrations 0115 and 0147 drop and recreate all nine price/TWAP views
  `WITH NO DATA` and leave `refresh_continuous_aggregate` to an operator
  banner; each view's refresh policy then re-fills only its trailing
  `start_offset` window, so the newest bars reappear and every freshness,
  bar-age and last-refresh signal reads green while the whole
  back-history serves empty. Only `twap_1h`/`twap_1d` had a detector, and
  it derived its reference from `prices_1m` — a view the same migrations
  empty — so it published a healthy 0 in exactly the state it existed to
  name. The data-freshness watchdog now judges all nine views against the
  `trades` hypertable (allowing for an armed retention policy on a view,
  migration 0156), publishing `stellarindex_cagg_history_missing{view}`
  alongside the TWAP gauge; both arm the same alert. Separately,
  `stellarindex-ops projector-replay` now waits for the projector to
  re-walk the rewound range and re-materializes the price CAGGs over it,
  failing loudly if it cannot: the aggregates' policies only roll
  forward, so re-projected historical trades were durable in the
  hypertable and invisible to every /v1/ohlc, /v1/chart, /v1/vwap and
  /v1/history/since-inception read. (audit-2026-09-02 F047, F116, K006,
  T425)
- **markets (money):** `/v1/markets?source=X` now reports X's OWN 24h
  volume, 24h trade count and last price, and the unfiltered listing
  serves a last price from the current day rather than the previous
  one. The per-source listing read the pair-wide `prices_1m` /
  `prices_1d` continuous aggregates and filtered them by `$source =
  ANY(sources)` — a test for the BUCKETS a venue printed in, which then
  summed every venue's trades in those buckets: a pair soroswap printed
  twice into minutes SDEX printed six times in came back with all
  eight, and `/v1/pools?source=soroswap` (which always read the
  per-source aggregate) disagreed with it by orders of magnitude on the
  same venue's same pair. `SourceMarkets` now computes from
  `pools_per_source_1h`, the same CTE `/v1/pools` uses, so the two
  surfaces agree structurally. In the same read, `last_price` and
  listing membership no longer come from `prices_1d` alone: that view
  is materialized_only with a 6-hour end_offset, so its newest bucket
  is the PREVIOUS UTC day's close for every pair that traded today, and
  a market whose first trade was today had no row in it at all and went
  unlisted. The 24h `prices_1m` scan already in the query now supplies
  both. (audit-2026-09-02 F027, F028, K031)
- **price serving (money):** a cached VWAP can no longer outlive the
  aggregator that publishes it. `cachekeys.VWAPTTL` now bounds every
  `vwap:` (and, through it, `confidence:`) key by a 5-minute silence
  grace — 10 missed ticks at the default 30 s cadence, the same number
  and the same reasoning as `FreezeTTL` — instead of keying the TTL to
  the aggregation window. Before this, `/v1/price?window=86400` served
  the 24 h key for a full day after the aggregator stopped (crash,
  deploy, OOM, failing Redis writes) with `observed_at` stamped at
  request time and `flags.stale` unset, so a day-old price was asserted
  as current and no field on the wire could reveal it; the same lie
  covered a window that had simply run out of trades. A stopped
  publisher's value now expires and the surface answers its documented
  404 rather than substituting a different TIME for the window the
  caller asked for. Windows at or below the grace (the 300 s surface,
  the API's 5-minute triangulation fallback) are unchanged, and a
  freeze still extends the last-known-good value to cover the ADR-0019
  hold. (audit-2026-09-02 F034)
- **api (security):** `/v1/twap`, `/v1/chart` and
  `/v1/history/since-inception` now ask the scam-issuer gate about BOTH
  legs of the pair. Keyed on the base alone they withheld
  `?base=<FLAGGED>&quote=native` at 404 while serving
  `?base=native&quote=<FLAGGED>` at 200 — the same market, the same
  number, inverted, because the aggregate reads fold both stored
  directions. On the series surfaces that published the flagged
  issuer's whole trajectory. All three now route through the package's
  single `scamWithheld` spelling, whose fold lives in `pricingguard`,
  and the two files leave the pair-question ratchet so they cannot
  regress. (audit-2026-09-02 F019, F032)
- **api (security):** `/v1/price/tip` and `/v1/price/stream` — the last
  two price surfaces keyed on one leg — now ask the scam-issuer gate
  about BOTH legs of the pair. A directory-scam-flagged issuer named as
  the QUOTE was served at 200, unauthenticated: on the tip, the freshest
  number we publish, computed from that issuer's own trades; on the
  closed-bucket SSE stream, fanned out once per bucket for the hours a
  connection lives, with no gate anywhere on the producer path. Both are
  the same market, the same number, inverted, that
  `?asset=<FLAGGED>&quote=native` was refused. Both now route through
  the package's single `scamWithheld` spelling, whose fold lives in
  `pricingguard`. With them migrated, the pair-question ratchet's
  exemption list is not empty but DELETED — the guard permits zero
  base-only consultations and has no mechanism to park a new one.
  (audit-2026-09-02 F002, K001)
- **api / explorer (money display):** `/v1/ohlc`'s single-bar
  `base_volume` / `quote_volume` now state their own smallest-unit scale
  (`base_volume_decimals` / `quote_volume_decimals`). The smallest unit
  is the trading venues' — 7 decimals on-chain, 8 on a CEX, 6 on the FX
  feeds — never a fixed stroop, and with nothing on the wire saying so
  the `/markets/[pair]` page divided by a hardcoded `1e7` and printed
  every Coinbase-quoted pair (`crypto:XLM/fiat:USD` among them) at ten
  times the market's quote volume. The page now renders
  `volume / 10^decimals`, and an em-dash rather than a guessed divisor
  when a response omits the field. The scale is resolved over the
  PRE-outlier-filter population — the set `NormalizeAmountScale` lifted
  to a common scale — so a window whose only 8-decimal venue is filtered
  out still reports 8 for the survivors that were lifted to meet it.
  (audit-2026-09-02 F096)
- **api (billing/security):** the monthly request ceiling is now metered
  per OWNER ACCOUNT, not per credential. `middleware.UsageKeyForSubject`
  — the single derivation the usage writer and both readers share — now
  prefers `Subject.Identifier` (`acct:<slug>`, the owner reference the
  key stores stamp) and falls back to `KeyID` only for a credential that
  carries none. The ceiling it is compared against is a PLAN budget
  (`platform.Tier.MaxMonthlyQuota`, clamped by the account-level
  override, which a customer may only ever LOWER), but the counter's
  identity was the credential's: N live keys under one account meant N
  independent month-to-date counters, so the plan allowance multiplied
  by the number of keys held, and a revoke-and-mint minted a fresh
  `KeyID` — hence a month-to-date of zero — resetting the cap on demand
  mid-month at no cost, since the key-count check counts only un-revoked
  keys. Writer and readers moved together: pointing a reader at an
  account key the writer never writes would have metered nothing at all.
  Two consequences, both intended: `/v1/account/usage` now reports every
  key the account holds (which is what its name promised) and its
  history is keyed on the account, so rollup rows written under the old
  per-credential subject fall out of the trailing 30-day window as it
  rolls forward; and the quota 429's Problem+JSON `detail` now says the
  account's quota, not the API key's. (RLT-404)

- **storage (test):** the both-directions query-shape guard no longer
  depends on a hand-maintained list of subjects — the list is why four
  readers carrying the exact shape it forbids shipped green. It now
  parses `aggregates.go`, recovers each declaration's SQL from its
  string literals (so a statement assembled from a template plus
  optional clause fragments is checked whole), and selects its subjects
  by shape: a read that folds both stored orientations AND emits
  bucket-ordered output must use `UNION ALL`, and no query in the file
  may put an INTERVAL on the left of a bucket comparison. A new reader
  in that file is covered the moment it is written. The scan asserts a
  minimum subject count so it cannot go quietly vacuous.
  (audit-2026-09-02 F117, F038)
- **ohlc / anomaly baseline (perf):** the same both-directions OR
  disjunction is gone from `OHLCSeries`, `OHLCSeriesReBucketed`,
  `TimedVWAPsForPair1m` and `VWAPsForPair1m`. A `[from, to)` bind range
  does not rescue the shape — `/v1/ohlc`'s window is caller-chosen and
  can span the whole retained history — so each now folds the
  orientations as a `UNION ALL` of two single-direction branches feeding
  the existing normalise/group pass. `OHLCSeries`'s closed-bucket guard
  and `OHLCSeriesReBucketed`'s post-fold `HAVING` move to the sargable
  spelling. Bars and baselines are byte-identical; proven by the
  existing on-Postgres direction-fold, dust-floor and interval-fold
  integration tests. (audit-2026-09-02 F038)
- **history/chart (perf, unauth DoS lever):** `/v1/history/since-inception`
  and `/v1/chart` no longer fold the two stored market orientations with
  an `(A AND B) OR (B AND A)` disjunction. Postgres cannot drive
  `prices_*_pair_bucket_idx` from an OR of two different (base, quote)
  equality pairs, so a bucket-ordered read fell back to the plain bucket
  index with the pair test as a post-index FILTER — and proving an
  unknown pair EMPTY then walked every chunk to exhaustion (10682.994 ms
  vs 3.610 ms, measured on r1 for a zero-row pair). `HistoryPoints` is
  the worst case: it is anon-reachable and carries no lower time bound
  at all. `HistoryPoints`, `HistoryPointsInRange` and
  `TWAPPointsInRange` now read a `UNION ALL` of two single-direction
  branches, each with its own bound and `LIMIT`, merged under an
  `ORDER BY bucket ASC, base_asset` outer sort; the closed-bucket guard
  moves to its sargable spelling (`bucket <= now() - INTERVAL …`, never
  `bucket + INTERVAL … <= now()`). Served values are unchanged.
  (audit-2026-09-02 F169, F117, F038)
- **pricing-guard:** the scam-issuer price gate is now keyed on the PAIR,
  not on the base leg alone. Asking for a directory-flagged issuer as the
  QUOTE — `?base=native&quote=<FLAGGED>` — republished at 200, and
  unauthenticated, the exact reciprocal of the number the very same
  endpoint had just withheld for the opposite orientation, together with
  its volumes and trade counts. `ScamGate.WithheldPair` folds both legs
  (each still resolved through the SAC/classic alias family, so a wrapper
  spelling cannot re-open it one orientation at a time) INSIDE the guard
  package, so no call site can consult one leg and forget the other. The
  fold reaches `/v1/price`, `/v1/price/batch`, `/v1/price/at`, the SEP-40
  oracle paths, the asset headline and the DEX-TVL valuation via the
  chokepoint, plus `/v1/vwap`. `/v1/twap`, `/v1/chart`, `/v1/price/tip`
  and the closed-price SSE stream still ask the base-only question; they
  are named in a shrinking ratchet
  (`TestScamGateIsAskedThePairQuestion`) so no NEW surface can join them.
  (audit-2026-09-02 F002/K001, partial: F019/F032/T039 carry the four
  remaining handlers)
- **price-alerts:** the aggregator's price-alert evaluator now consults
  the same withholding chokepoint the API serves under
  (`pricingguard.PriceWithheld`), rather than the thin-market half alone.
  It reads the same closed `prices_1m` bucket `/v1/price` reads and
  delivers the number to a customer's webhook, signed — so a
  directory-scam-flagged issuer's price, refused with 404 on every API
  surface, was still deliverable from this binary, whose seam the API's
  gate guard structurally cannot see. Both binaries now import one
  expression instead of keeping a copy each, and this binary has its own
  seam guard. Off by default (`[price_alerts] enabled=false`).
  (audit-2026-09-02 K001)
- **api:** a key minted through `POST /v1/account/keys` now inherits the
  calling key's monthly request ceiling. `auth.CreateAPIKeyRequest` had
  no `MonthlyQuota` field at all, so the self-service rotation path
  persisted `monthly_quota: 0` on every child — and the quota
  middleware's `MonthlyQuota <= 0` short-circuit treats zero as "no
  ceiling". A metered customer could therefore mint an UNMETERED
  credential from their capped one in a single call, and the cap the
  operator sold them survived exactly until the first rotation. The
  request now carries the ceiling and the store persists it, so the
  validator maps it onto the Subject the middleware reads. It copies a
  cap and never invents one: a caller without a ceiling still mints a
  child without one, because the cap is opt-in by contract and
  defaulting it here would arm a 429 for keys that never had a limit.
  (reverification-2026-09-18 RLT-404)
- **divergence:** the Chainlink reference no longer writes the operator's
  RPC API key into the divergence cache. `[divergence.chainlink].rpc_url`
  is populated from the same `CHAINLINK_RPC_URL` the ingest poller uses,
  and keyed providers carry the key in the URL PATH (`.../v2/<KEY>`) —
  config already documents the whole value as a secret. Both URL-bearing
  error paths in the reference's `eth_call` wrapper (request build and
  transport) wrapped the raw `*url.Error`, whose `Error()` quotes that
  URL verbatim; `Compare` copies the message into `Result.Failures` and
  the worker JSON-marshals it into the per-pair `div:` key in Redis,
  which is a no-AUTH internal bind. So an ordinary timeout, TLS failure
  or 429 — no attacker action at all — parked the key in plaintext at
  rest and re-wrote it every refresh cycle, and the decimals() retry WARN
  logged the same string. Both paths now render the error through the
  redactor the sibling ingest client already had, which is exported
  rather than copied so the two clients onto the same endpoint cannot
  drift apart again: the host stays (it is the whole diagnostic), the
  path becomes `/<redacted>`. (audit-2026-09-02 NS12)
- **api (docs only):** the `/v1/ohlc` bar's `base_volume` /
  `quote_volume` are no longer documented as "stroop-equivalent". They
  are raw smallest-unit sums at a per-SOURCE scale — 7 decimals on-chain,
  8 for a CEX-fed leg — so a consumer that divided by a fixed 1e7
  overstated every CEX-quoted pair tenfold. The served values are
  unchanged; making the scale readable needs a wire field across the
  OpenAPI spec, `pkg/client` and the explorer's generated types, which
  this change does not touch. (audit-2026-09-02 F096, partial: the wire
  field and the `/markets/[pair]` divisor remain)

- **aggregator:** a window's published VWAP is reproducible from its own
  inputs again. `aggregate.FiatBackers` returned the stablecoin backers
  in Go map-iteration order, so the orchestrator's fetch plan — and the
  order it appends each source's batch into one merged window — differed
  call to call; the time-local outlier filter then sorted only on
  timestamp, and ledger-close timestamps are shared by every trade in
  the ledger, so same-timestamp prints kept that merge order. Since the
  filter takes a print's neighbourhood reference BY POSITION, the
  reference centre and the trim decision moved tick to tick on identical
  data. Backers are now sorted at the source (call sites that had
  noticed were re-sorting defensively), and the local index breaks
  timestamp ties on the trades primary key (ledger, source, tx_hash,
  op_index) — the same comparator `sortTradesChronological` already uses
  for the same reason. (audit-2026-09-02 K036)

- **api:** `/v1/price/at` and `/v1/price/changes` now apply the
  serving-sanity guard to a prices_1m answer. Their shared reader seam
  resolves an instant through a CAGG ladder whose finest rung is the
  same raw closed 1-minute bucket `/v1/price` serves, and it carried the
  withholding gates but not the trailing-baseline guard — so one extra
  path segment republished the manipulated minute `/v1/price` refuses
  (as the current price AND as every horizon reference behind a
  `change_pct`), while `pricingguard`'s package doc claimed it covered
  "every raw prices_1m closed-bucket serving path". A rejected candidate
  is replaced by the newest clean trailing bucket, but only while that
  bucket still closes within the caller's own at-or-before staleness
  bound; otherwise the instant is reported unavailable (a 404, or a null
  horizon) rather than answered with a value the manipulation band
  rejected or one that silently breaches the requested staleness. The
  package doc now enumerates wired call sites. (audit-2026-09-02 F031)

- **aggregator/pricing:** the robust outlier and served-price bands are
  no longer blind to downward prints. Every band in `internal/aggregate`
  was ADDITIVE in price space (`|p − centre| > K·1.4826·MAD`), and a
  price can only be `centre` below the centre — so once the window's
  relative MAD reached 1/K the lower edge went non-positive and NO
  downward print could be rejected at all, while the mirror-image
  up-move still was. That threshold is 16.9 % relative MAD for the
  window filter at the default σ=4 and 6.75 % for the served-VWAP
  guard's MAD arm at K=10 — dispersion an ordinary long-tail pair
  reaches routinely, after which a single crafted minute bucket at any
  price down to 0 was published into the VWAP and served as a confident
  price. The deviation is now measured symmetrically in RATIO space
  (ADR-0046 §1's direction symmetry — a ½× and a 2× print are equally
  outlying; the scale is still a price-space MAD, not §1's MAD(log p)),
  giving the
  band `[centre²/(centre + K·scale), centre + K·scale]`: always strictly
  positive, identical to the old band above the centre and never lower
  than it below, so nothing previously rejected is newly accepted and a
  volatile pair still earns its wider band from its own spread. Applied
  to the window filter, the time-local published-VWAP filter and the
  served-VWAP guard alike, exact `*big.Rat` throughout. (audit-2026-09-02
  F037, F039, K004, RLT-391 / #788 — `rejectAggregatorOutliers` in
  `internal/aggregate/global.go` carries the same additive band and is
  swept by the next entry)
- **aggregator:** the oracle-aggregator tier's divergence filter is
  symmetric in ratio space too, closing the seventh and last site of
  that band. `rejectAggregatorOutliers` scored a vendor quote additively
  against the median of its peers at K=5, so the band's lower edge went
  non-positive at a relative MAD of 1/(5·1.4826) = 13.5 % — routine
  disagreement between three vendors on a thin RWA, or on a major
  mid-crash — and from there NO downward quote could be rejected while
  its mirror-image pump still was. A vendor publishing a decimal-shifted
  or stale-to-zero price was then averaged straight into the plain-mean
  headline that this filter exists to protect: on a 70/85/100/115/130
  source set a 1.00 quote dragged the served price from 100.00 to 83.50.
  It now uses the same `symmetricDev` helper as the other six sites, so
  the band is `[centre²/(centre + K·scale), centre + K·scale]` —
  unchanged above the centre and never below the old edge underneath it,
  so no vendor that used to survive is newly dropped for being merely
  low, and the tier still never fails closed. (audit-2026-09-02 K004 /
  #788)
- **api:** a client-abort flood can no longer fail the rate limiter
  CLOSED for every caller on a bucket. The throttle's Redis round-trip
  ran on the REQUEST's context, and `ratelimit.Bucket` cannot tell a
  caller-cancelled call from a Redis outage — every error out of the
  take arms its dwell clock and resets the recovery streak — so a client
  that connected, sent a request and immediately RST, a few times a
  second, kept the fail-closed clock armed indefinitely and denied the
  30 s unbroken-success streak needed to disarm it. Past the window the
  limiter answered `throttle-unavailable` and the middleware returned
  503 to the whole anonymous (or whole authenticated) tier while Redis
  was healthy: a remote kill switch costing one TCP handshake per tick.
  The take now runs on the request's values WITHOUT its cancellation,
  bounded by its own 5 s timeout, mirroring the post-response usage
  writes. That also closes the mirror-image hole — an aborted request
  used to fail open and spend no token, so aborting was free traffic.
  A real Redis outage still fails open inside the dwell window and
  closed past it, with a regression test pinning both.
  (reverification-2026-09-18 F059 / RLT-160)
- **api:** the two sibling seams that mirror the same dwell clock are
  swept with it. The monthly-quota gate's month-to-date read is
  detached the same way: its clock is process-wide, so an abort flood
  pre-armed it and turned the next genuine blip — which the gate
  documents as fail-OPEN, since the cap is billing fairness and not a
  security boundary — into a 429 for whichever metered customer hit it
  first. The signup per-IP throttle's increment likewise no longer
  inherits the caller's cancellation, which also stops an aborted
  signup from being an UNCOUNTED one: abandoning the connection
  mid-flight used to buy unlimited attempts against the cap that exists
  to stop bulk account minting. (reverification-2026-09-18 F059 class
  sweep)

- **api:** `GET /v1/history/since-inception` now applies the
  directory-scam gate, so a flagged issuer's full VWAP trajectory is no
  longer served at 200 while every other aggregated-price surface —
  `/v1/price`, `/v1/price/tip`, `/v1/price/batch`, `/v1/vwap`,
  `/v1/twap`, the SEP-40 oracle, the asset headline and
  `/v1/chart?timeframe=all` — withholds it. The two endpoints run the
  identical CAGG series chain over the identical pair, differing only in
  the read closure, and the gate sat in `handleChart` rather than in the
  shared chain, so the cheaper route answered the same question
  ungated. A series is worse than a point: withholding one number denies
  a quote, an ungated series hands over the whole trajectory, which is
  what makes a manufactured market look legitimate. The raw surfaces
  `scam.go` promises stay visible — `/v1/history`'s trade rows,
  `/v1/observations`, `/v1/ohlc` — are untouched; the distinction is raw
  trades versus an aggregated price claim, not the route prefix. The one
  gate helper is now shared by both callers and labels its metric per
  surface (`chart`, `history_series`). (audit-2026-09-02 T012)

- **api:** `POST /v1/admin/keys` now runs the same delegation clamp
  (`middleware.ClampMintScopes`) the self-service mint path runs, so a
  scope-narrowed operator credential can no longer mint itself an
  unscoped — i.e. full-access — key. The handler gated on tier alone and
  passed the requested scope list straight into the account store, while
  the clamp's own doc described itself as "the single chokepoint every
  mint path funnels through"; that held for `POST /v1/account/keys`
  only. The escalation was closed rather than theoretical: this handler
  mints `tier: operator` keys WITH an explicit scope list, so a narrowed
  operator key exists by construction, and an empty scope list means
  every capability. A scoped caller asking for nothing now inherits its
  own scopes; asking for a scope it does not hold is refused with 403
  before the mint, and the audit row records the clamped set actually
  issued. (reverification-2026-09-18 RSEC-A2 / RLT-162)

- **aggregator:** a ClickHouse that is still loading metadata at boot no
  longer disables the decimals-assumption guard for the whole process
  lifetime. The lake reader was dialled inline at startup and one failed
  ping emitted a single WARN and skipped the guard entirely — Backfill
  and periodic Sweep both — with no retry and no metric that separates
  "the guard found nothing" from "the guard never ran". That is the
  EXPECTED shape after a reboot, not an edge case: `clickhouse-server`
  spends minutes loading metadata for the 150B-row lake and the
  aggregator unit's ordering does not wait for it. A non-7-decimal
  SEP-41 token listing afterwards then gets no
  `nonstandard_decimals_assets` row, `aggregate.AdjustPrice` applies no
  correction, and every served price on its pairs is skewed by
  10^(7-decimals) with no other alarm. The dial now lives inside the
  guard's own goroutine and retries with exponential backoff (15 s to a
  5 min ceiling) until it succeeds or the process shuts down, logging
  once at the first failure, once per ceiling-length interval while it
  persists, and once when the guard finally arms. A source-level
  tripwire keeps the wiring — and accounts for the two other components
  that still dial the reader inline. (audit-2026-09-02 F040, retry half;
  the sweep-heartbeat gauge and its staleness alert land in the next
  entry below, closing the finding)
- **aggregator:** the decimals-assumption guard now emits
  `stellarindex_decimals_guard_sweep_last_success_unix`, stamped by
  `internal/decimalsguard.Guard.Sweep` on every pass that completes its
  trade enumeration, whether or not it finds an offender. Closes the
  observability half of audit-2026-09-02 F040: the offender counters
  above (`stellarindex_dex_trade_nonstandard_decimals_total`,
  `stellarindex_nonstandard_decimals_lockstep_mismatch_total`) sit at a
  healthy-looking zero whether the guard swept and found nothing or
  never armed at all, so an operator had no metric to tell the two
  apart. The new `stellarindex_decimals_guard_sweep_stale` alert (>
  45 min, three sweep intervals, `for: 15m`, scoped to
  `job="stellarindex-aggregator"` so the indexer/api binaries exporting
  the same gauge at its zero value cannot page a permanent false
  positive) ships in this same commit in both rule trees —
  `deploy/monitoring/rules/aggregator.yml` and
  `configs/prometheus/rules.r1/aggregator.yml` — together with its
  `docs/operations/alerts-catalog.md` row, because the repo's own
  `lint-rule-equivalence` and `lint-alerts-catalog` gates require all
  three in lockstep for any commit touching an alert tree. Both
  companion trees are current as of this commit; nothing here is
  tracked as a follow-up.
- **aggregator:** `internal/aggregate/anomaly`'s package doc no longer
  describes a decision model the code does not implement. It documented
  the Phase-1 thresholds as calibrated against "the previous
  closed-bucket VWAP", while the orchestrator computes a ROLLING window
  on the tick clock and compares against the PREVIOUS TICK's value over
  the same rolling window — consecutive comparands overlap 90% at 5 m,
  99.17% at 1 h and 99.965% at 24 h, so `deviation_pct` shrinks with
  window length and `freeze_pct` is structurally unreachable at 1 h and
  24 h. The doc now states the deviation as a known, named defect
  (audit-2026-09-02 RLT-356) so the thresholds are not re-derived from
  observations produced by the wrong comparand. The code fix spans the
  orchestrator, the baseline's one-minute MAD grain and the confidence
  step together and is not landed here.

- **api:** the SSE Hub no longer reserves a 20 KiB replay ring for a
  topic nothing has ever published to. `DefaultMaxTopics` was never a
  ceiling — `getOrCreateTopic` inserts unconditionally and the reaper
  evicts only SUBSCRIBER-LESS topics, since dropping a subscribed one
  would silently detach an open stream — so the live topic count scales
  with concurrent streams x alias fan-out, and with the ring allocated
  eagerly so did resident memory: `/v1/price/stream` subscribes one
  connection to `assetAliases(base)` x `assetAliases(quote)` (up to 9
  topics, of which the aggregator publishes to at most a few), which at
  the shipped 8192-stream cap reserved ~1.5 GiB of rings that could
  never hold an event. The ring is now allocated on a topic's first
  PUBLISH, so a subscriber-only topic costs a map entry (measured 317
  bytes) instead of ~22 KiB, and ring memory scales with topics that
  actually carry data — which the reaper does bound. Replay,
  `Last-Event-ID` resume and reaping are unchanged; the new
  `Hub.BufferedTopicCount` reports the count that carries the memory,
  and `DefaultMaxTopics`' doc now says what it is (a reap threshold)
  rather than what it never was. (audit-2026-09-02 F058, K010)

- **api:** one unauthenticated address can no longer take the whole
  shared tip-producer pool and 503 everybody else's
  `/v1/price/tip/stream`. The 512-slot ceiling bounded the total but
  partitioned it by nothing, and a tip-stream connection mints a
  DETACHED producer — `context.Background()`, 30 s of linger — so
  looping the key space (~9 real pairs x `window_seconds` 1..60,
  aborting each connection as the headers arrive) filled every slot
  with junk the connection caps cannot see, after which every OTHER
  caller's first request for an unwatched pair was refused. Each
  producer is now charged to the caller that MINTED it, for as long as
  its registry entry lives — through the linger, since the linger is
  the window the flood runs in — and a new producer is refused above a
  per-caller quota (24, against the shipped per-IP concurrent-stream
  cap of 20) BEFORE the global ceiling is consulted, so one address
  holds under 5% of the pool instead of all of it. Joining an
  already-running producer is never charged, so a page reload — the
  case the linger exists for — can never hit the quota. The caller key
  is the /64 prefix for IPv6 (SEC-15): keying on the full address is
  bypassed by rotating within a prefix the caller already owns. The two
  refusals are distinct outcomes, not one boolean, and are separated in
  the log and in the problem detail so an operator can tell "one client
  is enumerating the key space" from "the deployment has outgrown its
  ceiling"; the 503 + `Retry-After` wire contract is unchanged.
  (audit-2026-09-02 F054, K010)

- **supply:** a failed supply-snapshot run no longer erases the
  staleness key its own escalation depends on. node_exporter's
  textfile collector serves exactly what the `.prom` file holds on
  each scrape, so the failure path's whole-file rewrite — which emits
  only `unit_failed` and the run duration — retired
  `stellarindex_supply_snapshot_last_success_timestamp` after the
  FIRST failure. Both `stellarindex_supply_snapshot_stale` (36 h
  ticket) and `stellarindex_supply_snapshot_critical_stale` (72 h
  **page**) evaluate `time() - <that metric>`, which is no-data rather
  than "very old" once the series is gone: days 2, 3 and 4 of an
  outage produced only the repeating "most recent run failed" ticket
  plus, at 36 h, the misleading "never initialized" one, and the page
  tier could never fire. Every write through `internal/supply` now
  carries the previous file's samples for that family through
  verbatim (labels and value untouched, so the timestamp still names
  the last genuinely successful run) whenever the run has no fresh
  success to stamp; with no prior file nothing is emitted, leaving
  `_never_initialized` to cover a first run that fails. A prior
  exposition that exists but cannot be read is left intact rather
  than truncated — a surviving staleness key still escalates, an
  erased one never does. Value gauges are still not carried: they
  would report supply the failed run never computed.
- **sources:** pre-P23 classic movements now record a MUXED
  counterparty under the G-account that actually holds the balance.
  `PaymentOp.Destination`, `ClawbackOp.From` and
  `AccountMergeOp.Destination` are muxed-typed, and their `M…` strkey
  was written verbatim into `stellar.account_movements` — an address no
  reader can ask for (`/v1/accounts/{g}/movements` filters by G-strkey
  equality), so the movement was invisible to everyone AND missing from
  the underlying account's own feed. They resolve to the base account
  now, the same rule the shared participant derivation and the lake's
  op-source extraction already apply; the memo id is a routing detail
  and is not carried into the movement. Non-muxed counterparties
  round-trip byte-identically.

- **clickhouse,ops:** a `classic-movements-backfill` run interrupted
  mid-batch can no longer leave a silent hole in
  `stellar.account_movements`. The chunked INSERT sorted its rows by
  ADDRESS first, so each 20k-row chunk was an address prefix spanning
  the window's whole ledger range — after a partial send `max(ledger)`
  already sat at the top of the window while every address past the
  failure point held nothing for any of it, and the data-derived
  `-resume` (which restarts from exactly that `max(ledger)`) skipped
  those addresses for the entire window with no row, log line or count
  to show for it. Rows are now sent in LEDGER order, so what survives a
  partial send is complete for every ledger below the highest one
  written and the one-ledger resume overlap repairs the rest. The
  cancellation notice no longer claims resume picks up at the window's
  start — it states the data-derived rule it actually follows.

- **api,clickhouse:** `GET /v1/accounts/{g}/movements` no longer strands
  an account's pre-watermark history. The cap67 ceiling that splits the
  ClickHouse archive arm from the Postgres tail was applied to the page
  the archive query had ALREADY returned — after its SQL `LIMIT` — so
  whenever the `limit` newest rows for an address sat above the ceiling
  (routine while the cap67 follow daemon is mid-window, and continuous
  for any account moving more than a page per derive tick) the page
  collapsed to zero rows, `next_cursor` was suppressed, and every
  movement BELOW the ceiling became unreachable through the endpoint.
  The ceiling now travels to ClickHouse as a `ledger <= ?` predicate
  (`AccountMovementFilter.MaxLedger` with an explicit `HasMaxLedger`
  set-signal, so a ceiling of **0** — an installed genesis movements
  floor with no watermark — still serves nothing from the archive arm
  rather than everything), and each page fills from the rows that are
  actually servable. Regression tests: the handler's full-page +
  `next_cursor` case, the genesis-floor fail-closed case, the query
  builder's ceiling-zero clause, and an executing ClickHouse
  integration test.
- **aggregator:** every asset's served `confidence` is no longer pinned
  at exactly `0.5`. ADR-0019's bootstrap cap — "for an asset with < 30
  days of history, cap confidence at 0.5 regardless of other factors" —
  compared its CALENDAR constant (`BootstrapDays = 30.0`) against a
  sample-DENSITY measure that could not reach it. The 30-day baseline
  window holds at most 43,200 one-minute buckets, `Day30.N` counts
  bucket-to-bucket RETURNS (one fewer than the buckets behind them), so
  `N/1440` peaked at 29.99931: the cap engaged for every pair forever,
  and the multi-factor score underneath it was unobservable — including
  to the Phase 2 freeze leg that reads it. Live on r1, BTC, ETH and XLM
  against USD all served `confidence: 0.5` with `z_score` 0.98,
  `cross_oracle` 1.0 and `baseline_quality` 0.996 — a 99.2%-dense
  baseline treated as freshly listed. Two corrections: the density
  reading now counts the N+1 buckets behind the N returns, so a
  completely-observed window reads exactly 30.0 days-equivalent; and the
  cap gates on a new `confidence.BootstrapDensityDays` (95% of the
  window, 28.5) instead of the calendar constant. The signal itself is
  unchanged — density over calendar age (W8.8) stands, and a
  mature-but-sparse pair trading 200 minutes a day still reads 4.17
  days-equivalent and stays capped. Buckets accrue at no more than 1,440
  a day, so clearing 28.5 days-equivalent still proves at least 28.5
  calendar days of observed history: the gate cannot un-cap a genuinely
  new asset.
- **api,storage:** the DEX/AMM protocol page's **24h USD volume no
  longer drops XLM-denominated trades**. `source_volume_1h` (migration
  0068) cannot materialize a finished USD figure — the XLM/USD multiply
  cross-references `prices_1m`, which a continuous aggregate may not
  join — so the CAGG materializes the raw inputs and its migration
  prescribes the read expression `sum_usd_priced + (sum_xlm_base +
  sum_xlm_quote)/10^7 * <XLM/USD vwap>`. The bespoke DEX block's two 24h
  readers (the volume KPI and the hourly volume series) summed only
  `sum_usd_priced`, so every leg the ingest-time valuation left unpriced
  was served as $0 — while the OTHER reader of the same CAGG
  (`/v1/sources`' per-source volume, rendered by the same source page's
  own 24h chart) applied the whole expression. One page, one source, one
  window, two different volumes. Both readers now apply the full
  expression, and the block's served note + KPI hint say so: the 24h
  figures are usd_volume plus XLM legs valued at the current on-chain
  vwap, while the per-pair surfaces below stay usd_volume-only (no CAGG
  carries XLM inputs per pair). Longer windows read the daily pair CAGG
  (0064), which materializes no XLM inputs, and are unchanged. Proven
  against a real TimescaleDB: a fixture of 57 USD priced + 30 XLM at
  0.5 served 57.00 before and 72.00 after, matching the source chart.
- **storage:** a SEP-41 supply event whose write hit a TRANSIENT fault is
  no longer **dropped from served supply** when it finally lands. The
  projector does not abort a cycle on a deadlock / statement_timeout: it
  writes the rest of the window and caps its ingestion cursor at (held
  ledger − 1) so the failed row is retried later. The 5-minute rollup
  pass, bounding its "settled tail" by `max(ledger)` alone, folded the
  later rows and pushed `sep41_supply_rollup.last_ledger` ABOVE the held
  ledger — after which the retried row was invisible to BOTH halves of
  the read (folds look only above `last_ledger`; the reader's live delta
  adds only `ledger > last_ledger`), so its mint/burn was silently and
  permanently missing from the token's total supply while the pass
  reported a normal advance. The fold now requires BOTH bounds — below
  `max(ledger)` (the tip may be mid-write) **and** at-or-below the
  projector's `sep41_supply` ingestion cursor — i.e. it folds only what
  the sole writer has COMMITTED, and leaves the held ledger to the
  reader's live delta until its row exists. With no cursor row at all
  (the projector has never committed a cycle) the pass folds nothing
  rather than assuming settlement: the reader stays exact via the
  full-sum path and the fold resumes on the first cursor commit. Proven
  against a real TimescaleDB — a 1,000,000-unit mint retried after the
  fold passed its ledger was absent from served supply before and is
  present after. (audit-2026-09-02 F118)

- **ci,ops:** `config_acknowledged=true` can no longer clear a config
  surface the deploy PROVED unapplied by asking the host. The deploy's
  ClickHouse evidence step has asked the target which objects exist
  since 2026-09-07, but it published only what it CERTIFIED — so the
  gate could not tell "nobody asked" from "asked, and the answer was
  no", and an acknowledgement cleared both alike. That is not a
  theoretical gap: on **2026-09-18 it cleared v0.91.0 to r1** while the
  step's own `::warning::` named `stellar.asset_month_usd_prices` and
  `…_staging` as absent, and because the month-priced cohort flows
  `LEFT JOIN` that table, **every**
  `GET /v1/accounts/{g}/graph/cohort` answered **500** — in 0.33 s,
  against 200 in ~1.5 s the release before — until the DDL was applied
  by hand. The step now publishes a `refuted` output beside `applied`,
  and the gate refuses those surfaces whatever the operator asserted:
  an acknowledgement asserts a surface IS applied, and here the host
  said otherwise in the same run. It NARROWS the acknowledgement rather
  than removing it — a surface no step could check behaves exactly as
  before, matching is exact-path so a refutation never spreads to a
  file nobody asked about, and an evidence step that faults publishes
  nothing and falls back to asking the operator. Five assertions in
  `scripts/ci/config-apply-gate-test.sh` are red against the previous
  code, including the end-to-end one that extracts deploy.yml's real
  step, runs it over the real `v0.61.1..v0.62.0` range against a host
  lacking the object, and feeds its output to the gate.
- **supply:** the SEP-41 rollup pass now reads its OWN input boundary
  (`last_ledger`) and floor (`genesis_baseline_ledger`) inside the folding
  statement, behind `SELECT … FOR UPDATE` on the rollup row, instead of in a
  round trip beforehand — so a fold reset that commits mid-pass is no longer
  stranded. Both resetters run against a live aggregator (`ch-rebuild -sep41
  -write`, and now `supply seed-sep41-genesis -write` when the floor moves);
  with the boundary decided before the write, a reset landing in that gap was
  silently undone — the pass added its delta over (stale `last_ledger`, max)
  on top of the freshly-zeroed totals and pushed `last_ledger` back up, so
  every row at-or-below the stale checkpoint was excluded from the fold
  forever (measured on the regression fixture: 4,000,001 served for a true
  5,500,001), exactly the undercount the reset exists to prevent. The
  interleave is now pinned by an integration test that waits on
  `pg_stat_activity` until the pass is genuinely blocked on the lock before
  committing the reset. No behaviour change on the uncontended path, and no
  migration — schema unchanged. Closes audit-2026-09-02 K005/F108.

- **supply:** seeding a SEP-41 pre-Soroban genesis baseline now zeroes the
  worker-owned rollup fold whenever the baseline ledger MOVES, so the
  documented `missing_baseline` remedy no longer re-arms the pre-Soroban
  double-count it exists to fix. `genesis_baseline_ledger` is also the
  Soroban-era slice's floor, and the aggregator's rollup worker folds a
  newly-watched contract immediately — with no baseline seeded the floor is 0,
  so the fold swept the CAP-67-replayed pre-boundary rows into `mint_total` and
  moved `last_ledger` past them; a later `supply seed-sep41-genesis -write`
  wrote only the genesis columns, and the reader then added that same band a
  second time (measured on the regression fixture: 1901 served for a true
  1001). `UpsertSEP41GenesisBaseline` now resets `mint_total` / `burn_total` /
  `clawback_total` / `last_ledger` in the same statement when the floor is
  distinct from the stored one, and leaves them untouched when it is not — so a
  repeat seed of the same boundary stays the no-op the runbook calls
  idempotent. The reader serves the exact floored full-sum fallback until the
  worker re-folds, so supply stays correct throughout. Restores the invariant
  that the fold columns sum exactly the rows with
  `COALESCE(genesis_baseline_ledger, 0) <= ledger <= last_ledger`. No migration
  — schema unchanged. Closes audit-2026-09-02 F022 / F029.
- **directory:** a false-positive scam flag is now correctable durably. The
  daily `directory-sync` upsert rewrote `source` on conflict, so a
  hand-held correction to `account_directory` was adopted into the
  upstream snapshot and overwritten on the next run — and a scam-class tag
  there is not cosmetic: it withholds the issuer's published price and
  market cap on `/v1/price`, `/v1/vwap`, `/v1/twap`, `/v1/chart` and
  `/v1/price/tip` (`pricingguard.ScamGate`), demotes its assets below
  every unflagged one in the `/v1/assets` ranking, and draws the
  explorer's flag pill. The conflict arm is now ownership-scoped (`WHERE
  account_directory.source = EXCLUDED.source`), which is what migration
  0136 already promised ("scoped by `source` so a future second directory
  source can coexist without the syncs deleting each other's rows") and
  which no sync kept: a second upstream also stole every address the first
  one carried. An operator correction is a row carrying the reserved
  source `operator-override` (`timescale.Store.UpsertDirectoryOverride`,
  undone by `.DeleteDirectoryOverride`); no sync of any upstream updates
  or prunes it, and `ReplaceDirectory` refuses to run AS that source. The
  correction is made on the directory row all three consumers read, so
  "price withheld", "demoted in the ranking" and "shows a flag pill" can
  never disagree.
- **ops:** `curated-rwa-sync` and `listing-sync` now refuse to follow a
  redirect that leaves the origin they dialled. Go's redirect header
  copier strips only `Authorization`, `WWW-Authenticate` and `Cookie`
  across hosts, so `X-Dune-API-Key` and `x-cg-*-api-key` were re-sent
  verbatim to whatever a `302` named — a vendor redirect, a hijacked
  edge or a mistyped `-base-url`, including an `https://` → `http://`
  downgrade. The `-base-url` https check only ever saw the CONFIGURED
  URL, never the dialled one. A same-origin hop is still followed
  (RSEC-E1).
- **rwa:** `curated-rwa-sync` no longer publishes a curated series it
  could only read part of, and no longer discards a month the curator
  printed in exponent form. A row that failed to parse was counted and
  skipped under a green run — and because the cache is replaced
  series-whole, the survivors DELETED the month already cached; when the
  lost row was the newest month, the published total was silently
  re-dated to an older month and understated. Exponent literals
  (`4.28e9`) are now stored as the plain figure (`4280000000`) by an
  exact point shift — no float, no rounding, the curator's own digits —
  and any row that still cannot be read refuses the run before anything
  is written, leaving the previous whole series served and the unit in
  `failed` for the catch-all alert (RLT-182).
- **api:** `GET /v1/price/stream` no longer fans out the aggregated
  prices `/v1/price` withholds. The closed-bucket SSE path ran
  aggregator → Redis → Hub → handler without consulting either serving
  gate on any leg, so a market below the thin-market substance floor
  (the 2026-08-04 valuation-incident class) and a directory-scam-flagged
  issuer's VWAP were both obtainable in real time from the surface that
  shares `/v1/price`'s consistency contract — while `/v1/price`,
  `/v1/price/tip`, `/v1/price/tip/stream`, `/v1/vwap`, `/v1/twap`,
  `/v1/chart` and the SEP-40 oracle all answered 404
  `errors/price-withheld` for the same pair. The handler now consults
  both gates (surface label `price_stream`) at connect — refusing with
  the same 404 and problem type before the response switches into SSE
  mode — and again for every forwarded bucket, because an SSE connection
  outlives a withholding verdict by hours and a connect-time-only check
  would keep serving every connection opened before a pair was flagged.
  A withheld bucket is dropped and the stream stays open heartbeat-only,
  matching the tip stream's shared producer, so a pair that clears the
  floor again simply resumes. Nil gates (operator disabled
  `[pricing_guard]`) withhold nothing, as everywhere else.
  `openapi/stellar-index.v1.yaml` documents the new 404 for this path —
  it listed only 200/400/429/503 while both siblings that answer the
  same refusal, `/price` and `/price/tip/stream`, already listed it —
  and the reference mirror, the Postman collection and the explorer's
  generated types are regenerated from it.

- **api / QueryShape caps a non-allow-listed parameter's NAME and the
  shape's term count (F170, K026, T651, F174, T665):** the slow-request
  query shape already redacted every unrecognised parameter's VALUE but
  logged its NAME verbatim at unbounded length, and put no cap on how many
  distinct parameters could each contribute a term — either one let a
  caller roll the journal with a single request. `QueryShape` now runs
  every logged name and value through a shared `boundedLogField` (strip
  control characters, cap length) and truncates the term list past
  `maxShapeParts`, applied after the existing sort so a truncated shape is
  still a deterministic prefix.
- **api / the access log caps `path` and `user_agent` length
  (Q176):** both were logged verbatim with nothing but Go's ~1 MB default
  header/request-line size bounding them — an attacker-chosen path or
  User-Agent was the same journal-flooding channel `QueryShape` already
  guards its parameters against. `Logger` now truncates both through the
  same `boundedLogField` helper.
- **docs / ha-plan.md's two stale `file:line` citations now point at the
  right lines (HO-361):** the `sla-probe.sh` BASE_URL citation still said
  `:21` after comment lines were added above the assignment (now `:27`), and
  the `18-pgbackrest-backup.yml` restore-drill-enable citation still said
  `:333-350` after a ClickHouse schema-drift task block landed earlier in
  the file and pushed it to `:512-537`. Neither underlying claim was wrong,
  only the pointer — but a stale citation sends an operator mid-incident to
  the wrong lines. `TestHAPlanFileLineCitationsResolve`
  (`internal/ops/chops/ha_plan_citations_test.go`) pins every citation in
  the doc against the cited file's actual content so a future reflow fails
  the test instead of leaving a silently wrong line number.
- **pipeline / the soroswap-router and defindex `entries` bump follows the
  landed insert (Q062):** the four inline `handleEvent` cases bumped
  `source_entry_counts` BEFORE their insert, unlike every persist helper,
  so an infra retry (REL-08 re-invokes `handleEvent` per attempt) counted
  one entry per attempt of the same event and a row the store rejected
  still counted one. The bump now runs only after the row landed, as in
  the other 35 sites. `TestHandleEvent_EntryCountFollowsTheLandedInsert`
  pins all four cases on the nil-store validation seam (the old order
  surfaced as a recovered sink panic); the build-tagged
  `TestSourceEntryCounts_RouterBumpFollowsTheLandedInsert` asserts the
  tally on a real Postgres.
- **pipeline / external trades left in the retry buffer at shutdown count
  as undrained rows (RLT-190):** `externalRetryBuffer.finalDrain` only
  logged a Warn for whatever its final bounded pass could not land, so a
  vendor-refillable loss never reached `stellarindex_sink_undrained_rows_total`
  or its alert — the runbook's Resolution C existed but nothing routed to
  it. The pass now adds the remaining rows to the counter (`kind="trade"`,
  by row, like `reportAbandonedTrades`) and reports at ERROR with
  `remaining=` and the sorted `venues=`; the runbook's symptom grep,
  triage tree and increment-site inventory follow the code.
  `TestExternalRetryBuffer_FinalDrainCountsUndrainedRows` pins it, with a
  landed-rows guard beside it.
- **projector / the per-row "holding cursor for retry" warning is throttled
  (RLT-142):** a held row is retried every cycle for as long as its fault
  lasts — forever, for an infra fault — and the projector warned once per row
  per cycle with no rate limit, so a sustained Postgres outage produced
  thousands of identical lines that buried the ERROR lines an operator needs.
  The warning now fires on the first failing cycle and every 20th after
  (`heldRowLogEvery`, mirroring the sink's `infraRetryLogEvery`), still
  carrying the running `consecutive_cycles` count. The per-cycle
  held-progress warning and the `sink_retry` metrics are unchanged.
  `TestCycle_HeldRowWarningIsThrottled` pins it (41 cycles → 3 lines).
- **projector / a recovered sink panic is shed, not held for the whole
  quarantine budget (Q053):** `pipeline.HandleEvent` recovers a sink panic
  and returns it wrapped in the (now exported) `pipeline.ErrSinkPanic`,
  which the sink's own classifier drops as permanent for that event. The
  projector's `classifySinkFault` did not recognise the sentinel, so the
  same error fell to the unclassified arm and held the sole-writer cursor
  while the panicking decode re-ran every cycle — 20 cycles with a
  sink-health proof, 720 without. It is now a skip verdict on both sides,
  under the same shed cap and health proof as a class-22/23 rejection.
  `TestClassifySinkFault` and `TestCycle_SinkPanicIsShedLikeAPermanentFault`
  pin it.
- **api / a served price is never an all-zero string (T045, RLT-009):**
  `/v1/ohlc`, `/v1/vwap`, `/v1/twap`, `/v1/price/tip`, `/v1/history` and the
  fiat chart legs render prices through `ratToDecimal`, which floored at ten
  fractional places with no escape — any price below 1e-10 served as
  `0.0000000000`, and `/v1/history` pinned exactly that for a
  3/1000000000000000001 trade. The renderer now extends its scale
  magnitude-relatively (twelve significant digits, sixty places at most),
  the same rule the aggregator's own `formatRatFixed` already applied, so
  normal-magnitude prices are byte-identical and a sub-1e-10 price keeps its
  digits. The direct fiat chart leg no longer formats a float with
  `%.10f` (round-to-nearest, unlike every other surface's truncation); both
  fiat legs lift the reader's float64 through its shortest round-trip
  decimal — the NUMERIC value for any rate of ≤15 significant digits — so
  0.3/0.1 renders `3.0000000000`, not `2.9999999999`. The float64 itself
  enters at `FXQuotePoint`, whose storage reader scans the NUMERIC into a
  float; threading the column's text form through is the remaining leg.
- **docs / ADR-0020 and launch-readiness L7.8 stop claiming
  `/v1/chart?price_type=twap` returns 400 (T513):** it has served the
  `twap_1h` / `twap_1d` aggregates since 2026-07-05, and the ADR's own
  2026-09-07 amendment already said so. §price_type and the Consequences
  carry a superseded marker, a dated amendment records what shipped and
  where it is pinned, and L7.8 is closed.
- **clickhouse / the dead `contracts_census_daily_staging` table is gone
  from both DDL files (T411):** `tier1_schema.sql` and the operator mirror
  `contracts_census_daily.sql` both declared a shared
  `stellar.contracts_census_daily_staging`, but no code path has written it
  since the rollup moved to a crypto-random-suffixed private staging table
  per run (the concurrent timer + backfill isolation) — so every host
  carried an empty orphan that read as a second writer path. The
  declaration is removed; the operator file now carries the one-line
  `DROP TABLE IF EXISTS` for hosts provisioned while it was declared.
  `TestCensusStagingTableIsPerRunOnly` pins both files, and the census
  integration test asserts no `contracts_census_daily_staging%` table
  survives a run on a real server.
- **clickhouse / `ch-schema-drift.sh` sees the whole shape of a declared
  table, in both directions, and the cut-over DDL beside its intent (T358,
  T453):** the T339 fix compared secondary indices the repo declares
  against live, but only that way round — an index hand-added live on a
  declared table (or dropped from `tier1_schema.sql` and never dropped
  live) reported rc=0, "0 divergent, 0 uncodified", because the
  uncodified bucket counts TABLES and the code comment claiming otherwise
  was wrong. Indices are now compared textually and PROJECTION /
  CONSTRAINT by name, each in BOTH directions, as real DRIFT — the rule
  the column list already applied to a hand-added column; a SHOW CREATE
  multi-line projection body is skipped by the column parser instead of
  being read as a column named `SELECT`. The intent side was also only
  `tier1_schema.sql`, so the `si-cutover-object` v2 tables
  (`ledger_entries_current_v2`, `contract_events_daily_v2` and their MVs)
  landed in UNCODIFIED with their ENGINE / ORDER BY never compared —
  `ledger_entries_current_v2` exists to change the ReplacingMergeTree
  version column, and hand-applied with the wrong one it produced zero
  signal. Every marked `*.sql` beside the intent (or named in
  `INTENT_CUTOVER`) is now parsed with the same parser; its objects are
  compared like any declared table when live, reported as "no cut-over in
  progress" when absent, and never counted uncodified. Eleven harness
  cases pin this (nine fail on the previous script). Hosts only compare
  cut-over objects once the role ships those files beside
  `/usr/local/share/stellarindex/tier1_schema.sql`; a checkout run finds
  them in `deploy/clickhouse/` unaided.
- **clickhouse / the census rollup's day window now has an index to prune
  on (T354):** `stellar.contract_events` declares
  `INDEX idx_ce_close_time close_time TYPE minmax GRANULARITY 1`. The
  30-minute `ch-census-rollup` filters `WHERE close_time >= day AND
  close_time < day+1`, but the table is partitioned and sorted by
  `ledger_seq` and ClickHouse keeps part-level min/max for partition-key
  columns only, so every run read every granule of the billions-row table
  while the code's own comment claimed a "close_time minmax index" that no
  DDL declared. close_time is monotone in ledger_seq, so the per-granule
  minmax prunes a one-day window to about a day of granules. Existing
  hosts need the one-time `ADD INDEX` + `MATERIALIZE INDEX` written next
  to the declaration; the drift checker reports the index absent until
  then. `TestCensusDayFilterHasASkipIndex` pins the predicate column to a
  declared minmax index, and the integration test
  `TestRunCensusDay_PrunesByCloseTimeAndSwapsPartition` proves on a real
  server that the planner drops the neighbouring day's granules and that
  the rollup lands exact per-contract counts.
- **test / the repo-wide guards fail only on the packages they protect
  (Q132):** `loadRepoPackages`, shared by `TestI128TruncationGuard` and
  `TestAssetTypeExhaustiveGuard`, hard-failed on a load error in ANY
  package under the repo root — a broken `scripts/dev` tool took both
  guards down and hid whatever they would have said. A load error is
  now fatal only under `internal/`, `cmd/` and `pkg/` (the set
  `scripts/ci/lint-i128.sh` scans); an out-of-scope failure is logged
  and that package skipped. The loader also refuses to return fewer
  than 100 in-scope packages (132 today), so a narrowed load cannot
  green either guard on a near-empty tree.
  `TestLoadRepoPackages_ScopesLoadErrors` pins the partition.
- **test / the i128 truncation guard now sees through locals (F053):**
  `TestI128TruncationGuard` judged only the inline shapes `int64(p.Lo)`
  and `int64(a.MustI128())`; binding the word to a variable first
  (`lo := p.Lo; int64(lo)`) was silently accepted, and the grep sibling
  `scripts/ci/lint-i128.sh` was defeated by the same rewrite. The guard
  now records every local bound to a parts word or a `Must*` result (and
  to another such local, to a fixpoint) and applies the same rule at the
  conversion; the positive control pins the through-a-local cases red.
  The grep gains a file-local second pass for `x := <p>.Lo` followed by
  `int(8|16|32|64)?(x)`.
- **ci / the money-column gate now reads the lake DDL (T454):**
  `scripts/ci/lint-migrations.sh` gains a fourth pass over
  `deploy/clickhouse/*.sql`: a monetary column typed `Float32`/`Float64`
  (Nullable or not) fails, keyed `table.column`, with the same inline
  `-- lint-money:ok <reason>` escape as the Postgres pass and the same
  stale-marker check. Before this the ADR-0003 gate globbed only
  `migrations/*.up.sql`, so `volume_usd Float64` in the ClickHouse DDL was
  invisible while the identical column in a migration was a CI failure.
  The four pre-existing float money columns (`asset_month_usd_prices
  .volume_usd`, `account_cohort_positions.amount`, each in the operator
  file and its `tier1_schema.sql` mirror) sit in a baseline inside the
  script that only shrinks; retyping one, or giving it the inline
  escape, is a change to that DDL. `scripts/ci/lint-migrations-test.sh`
  pins every verdict on fixtures.
- **aggregate / a source's VWAP weight is the correctly-rounded exact
  ratio (K048):** `SourceContributions` rounded each source's quote
  volume and the total to `float64` before dividing, so above 2^53 —
  every Soroban i128 volume — the served `Weight` landed ulps away from
  the true share. It is now one `big.Rat` division of the exact
  integers with a single correctly-rounded conversion.
  `TestSourceContributions_WeightIsCorrectlyRoundedAboveTwoPow53` pins
  the value for 2^53+1 over 2^54+11.
- **api / `/v1/accounts/{g}/movements` `provenance` enum (RLT-208):** the
  ClickHouse arm serves rows the `ch-cap67-movements` derive stamps
  `cap67_derived`, and the handler passes the value through, but the OpenAPI
  `AccountMovement.provenance` enum listed only `classic_derived` and
  `cap67_event`, so the generated explorer union and Postman collection
  rejected a value the endpoint returns for every post-P23 archive row. The
  spec now carries all three values with what each means, and
  `TestAccountMovementProvenance_SpecEnumIsTheGoVocabulary` pins the enum
  to the Go constants the writers stamp.
- **api / `/v1/assets/{id}/supply` `as_of_ledger` on the storage-derived arm
  (RLT-008):** a `contract_storage_balances` reading stamped `as_of_ledger`
  from the storage reader's own max(last_modified_ledger) — the last ledger
  any holder's balance moved at — while `flags.stale` was judged from the
  lake watermark, so the two fields in one response described different
  ledgers and an idle token could carry a months-old `as_of_ledger` beside
  a fresh stale verdict. Both now come from the same watermark read, as the
  mint/burn arm already did; `TestSupplyStorageFallbackAsOfLedgerIsLakeWatermark`
  pins it, including the no-reader case where the field is omitted.
- **api / `/v1/assets/{id}/supply` spec names and `supply_consistent` (RLT-002, RWC-532):**
  the OpenAPI `AssetSupply` schema documented `total_supply_lower_bound`,
  `contract_self_checks_agreed` and `declared_decimals` while the handler
  has only ever served `circulating_supply_lower_bound`, `supply_consistent`
  and `decimals`, so the generated explorer types and the Postman collection
  typed three fields no response carries. The spec now names the served
  fields and `TestAssetSupplyResponseFieldsMatchSpec` pins the handler struct
  to the schema in both directions. On the same path `supply_consistent` was
  `true` for a token that published no `TotalSupply` or `HolderCount` to
  check against; it is now omitted there, as its own description said, and
  set only when the contract offered something to agree with
  (`ContractStorageSupply.HasSelfChecks`).
- **usage / `usage_daily` upserts are chunked multi-row statements (F060,
  Q155):** `Store.UpsertUsageDaily` sent one prepared `INSERT … ON CONFLICT`
  per row inside a single transaction that only committed at the end, so a
  sweep's round-trip count and its transaction size both grew with the number
  of active subject-days. Rows now go as multi-row `VALUES` statements of at
  most 500 rows, each autocommitted, with duplicate keys inside a batch folded
  to their per-column maximum before the statement is built (Postgres refuses
  the same conflict target twice in one statement). The batch is validated
  whole before the first statement is sent. Unit tests pin the statement
  count against a recording driver; an integration test drives a 1,201-row
  batch across three chunks on real Postgres.
- **usage / the rollup sweep is bounded by its own cadence and writes only
  what changed (K052, Q155):** `Rollup.Sweep` passed the worker's context
  straight through, so a stalled Redis or Postgres held the sweep open
  indefinitely, and every tick re-sent every active subject-day's cumulative
  counters even when nothing had moved. A sweep now runs under a deadline of
  one interval (five minutes in production), holds a per-row mirror of what
  the sink last acknowledged, and sends only rows whose counters differ from
  it — a quiet tick writes nothing, a change sends the full cumulative value
  (never a delta), a failed upsert leaves its rows unacknowledged so the next
  sweep resends them, and a restart resends everything once, which the
  GREATEST merge absorbs. One sweep runs at a time.
- **storage / the directory sync refuses a snapshot that prunes or newly
  scam-flags more than one day plausibly does (F178, RSEC-D1):**
  `ReplaceDirectory` refused only an EMPTY snapshot, then ran the prune
  unconditionally in the same transaction. The upstream is an unpinned
  branch of a third-party repo and a scam-class tag withholds the issuer's
  price, so a hijacked, truncated or mis-generated snapshot that kept one
  row could un-flag every scam issuer, or flag thousands of issuers and
  withhold their prices, in one commit with nothing failing and no number
  reported. `ReplaceDirectoryWithin` now judges both counts inside the
  transaction against `DirectoryChurnLimit` (default 5 % of the rows the
  source held, never below 100; a source's first sync is unbounded) and
  rolls the whole snapshot back past it with `ErrDirectoryChurnExceeded`;
  `directory-sync` exits nonzero on that (which the
  `stellarindex_systemd_unit_failed` catch-all tickets), prints upserted /
  pruned / newly-flagged / held-before on success, and takes an explicit
  `-accept-churn` for a known upstream mass change. Pinned against real
  Timescale by `TestDirectorySync_ChurnCeiling` (a 4000-row source, a
  1000-row prune and a 1000-address flagging both refused and rolled back,
  150/150 lands, the opt-in accepts) and `TestDirectoryChurnLimit_Ceiling`.
- **ops / directory-sync refuses a truncated, corrupt or oversized tarball
  (T219, RSEC-D1):** the 256 MiB read bound was a plain `io.LimitReader` —
  a clean EOF at an exact multiple of tar's 512-byte block — so a stream
  cut between two entries handed `tar.Reader` a well-formed end of archive
  and the walk returned the entries read so far with a nil error, a
  partial snapshot `ReplaceDirectory` then pruned the table down to; and
  `tar.Reader` stops at the end-of-archive marker before the gzip trailer,
  so the archive's CRC-32 was never verified. The bound is now a refusal
  (`boundedReader`), and the stream is drained to EOF after the walk so
  gzip checks its CRC and length. Pinned by
  `TestParseDirectoryTarball_RefusesTruncationAtBlockBoundary` (a 2048 B
  bound over a 3-entry archive used to return 2 entries, nil) and
  `TestParseDirectoryTarball_RejectsCorruptGzipTrailer` (a flipped CRC
  byte used to return all 3 entries, nil).

### Added

- **phoenix (factory create event — evidence, F048 STILL OPEN):** real lake
  captures of the factory's `("create","liquidity_pool")` events now live
  under `test/fixtures/phoenix/factory-create/`, pinned in the default suite
  by `test/controlwiring/phoenix_factory_create_fixture_test.go`. They prove
  the events are in the lake from ledger 51,572,026 (the decoder comments
  claiming they "predate the lake" were false and are corrected), that both
  topics are `ScvString` — so the lake's `topic_0_sym` is empty for them and
  a ClickHouse walk keyed on it matches nothing — and that the body is one
  pool `Address` with the stake contract never announced. The decoder still
  does **not** self-register pools: nobody has yet shown, for the factory
  WASM installed today, that `create_liquidity_pool` is allow-listed and
  publishes the address it deployed rather than a caller-supplied one (the
  vector that removed defindex's self-registration). The red target test
  `TestK023_PhoenixFactoryCreateEventIsAdmissible` (`-tags k023evidence`)
  now runs on the real captures and asserts admission, not recognition; the
  open verification steps are in `docs/operations/wasm-audits/phoenix.md`.
  (F048)
- **ci (alert rules):** `scripts/ci/lint-tripwire-window.py` rejects an
  alert whose `for:` equals the window of an event function it compares
  `> 0` — `increase(m[W]) > 0` with `for: W`, the `rate()` twin, and
  `irate`/`changes`/`resets`, directly or under aggregations. That shape
  parses, passes every structural check, and cannot fire on one isolated
  increment; ten rules per tree had it and five were any-increase tripwires
  that had never been able to report the event they exist for (audit Q261).
  A rule that WANTS only a continuing condition to notify says so where it
  lives — `# lint-tripwire-window:sustained: <reason>` in the comment block
  directly above `- alert:` — and the five sustained rules in each tree now
  do (`discovery_drops`, `discovery_record_failures`, `ch_live_sink_drops`,
  `ch_live_sink_drops_sustained`, `stellar_archive_publish_fail`). The
  reason is mandatory, waivers are counted in the output, a waiver covers
  only the rule directly beneath it, and one left on a rule without the
  shape fails as stale. Deliberately NOT flagged: a non-zero threshold, the
  right-hand side of an `unless` (a suppressor, not a trigger —
  `duplicate_flood`), and a `for:` LONGER than the window, which reads as
  "failing continuously for" and which fifteen rules per tree use; Q261 did
  not examine those. The gate is run by `lint-rule-structure.py`, self-test
  first, so it is live wherever that lint already is (`lint-changed.sh`,
  `verify.sh`, the alert-rules CI job) without a wiring edit.
  `lint-tripwire-window-test.sh` pins exit codes, vacuity (a tree with no
  alert rule exits 2), the waiver rules, the wiring, and that each SHIPPED
  tree goes red when `trade_buffer_drop` or `webhook_delivery_exhausted` has
  its old `for:` put back; against the rule trees as they were before this
  work the gate reports 10 rules per tree. Follow-up outside this change:
  give the `-test.sh` its own line in `verify.sh` and `ci.yml`, as sibling
  self-tests have — until then it runs from `lint-changed.sh` when touched,
  and the in-process self-test runs on every gate.
- **test:** `test/controlwiring` — a build-tagged (`k023evidence`)
  reproduction of audit class K023, "a control exists in the tree but
  the production path never invokes it". One test per control, each
  reading BOTH sides of the seam: the verify-archive units that engage
  the checkpoint tier pass no `-fail-on-missed` (F144 — the tier-a
  units run `-tier chain`, where that flag is inert, so they are not
  the site); `pnpm build`, the build command Cloudflare Pages runs,
  reaches neither the `__next.*` prune nor `explorer-file-budget.sh`
  (F085); neither `projector-replay` nor `ch-rebuild` calls
  `external.BackfillSafe` (F050); phoenix's `Matches` rejects the
  factory's `("create","liquidity_pool")` event, so
  `seed-protocol-contracts` and the live-upsert hook cannot admit a
  pool (F048); and `branch-protection-status` is `continue-on-error`
  (F133). All five are red today and this entry fixes none of them —
  each fix belongs with the files that consume the control. Run
  `go test -tags k023evidence ./test/controlwiring/ -run TestK023 -v`
  for the live status; drop the tag once all five are green.
- **test (storage):** `TestSeriesReadsUnknownPairCostIsBounded` — an
  executing measurement of what an anonymous request for a pair that has
  never traded costs the three series reads that can run with no lower
  time bound (`HistoryPoints`, and `HistoryPointsInRange` /
  `TWAPPointsInRange` with no `from`/`to`), audit class K008. It reads
  each statement back out of `pg_prepared_statements` after the Store
  method has run, so it measures the shipped SQL rather than a copy, and
  `EXPLAIN (ANALYZE, BUFFERS)`s it over a 14-chunk, 124,800-row CAGG
  under both the custom and the generic plan, for a pair whose assets
  exist nowhere and for one whose assets are both heavily traded but
  never together. Every chunk is visited — no bound exists to exclude
  one — but each visit is an index probe: 68–84 shared buffers in total
  (2.4–3.0 per chunk per direction), no row fetched and discarded, under
  1 ms. The retired OR disjunction on the same fixture reads 5,597
  buffers and discards all 124,800 rows under the generic plan, and the
  test's bounds reject it. This entry changes no query and adds no gate:
  the cost is O(chunks) and stays O(chunks). Getting below that needs a
  lookup that does not touch the hypertable (a plain-table pair
  registry), which is a migration and is not attempted here.

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

## [v0.89.1] — 2026-09-17

### Fixed

- **aggregator:** the priceless-popular coverage tripwire resolves a
  Stellar Asset Contract candidate to its classic asset before calling it
  priceless. A Soroban-venue trade is keyed by the token contract while
  the same asset's price is served under its classic id, so yBTC —
  $43.8k on aquarius under its SAC, priced at $75,582 as `yBTC-GBUV…` —
  ticketed as a "market-popular asset with no price" on 2026-09-17. The
  probe asks the sweep's own priced set (same CTEs, same floors) about
  the classic id; a resolver miss or probe error leaves the candidate as
  read, and the gap log now names the classic asset it checked.

### Changed

- **ci:** the weekly ansible-drift verdict reports a task whose live
  file differs from the repo in comment text only — same effective lines
  after stripping trailing comments — as *comment-only*, and a handler
  that only those changes notified as *consequential*, without failing
  the run. r1's `postgresql.conf` had been failing the check for weeks
  over an incident note beside an unchanged `max_wal_size`. A changed
  task with no diff (`diff: false`) stays drift: the check does not guess.
