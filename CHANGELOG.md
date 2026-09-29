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

## [v0.94.0] — 2026-09-29

30 commits since v0.93.0: the cohort DeFi amounts now sum exactly in
Int256 (OPERATOR ACTION REQUIRED, see below), SEP-1 outages on our side
stop being published as the issuer's broken domain, the Tier E archive
verification cron that could never pass is retired, two explorer export
aborts are fixed, and three pilot waves of inventory fixes land.

### Changed

- **cohort — DeFi position amounts are summed exactly in `Int256`,
  never through a float (#1628, OPERATOR ACTION REQUIRED):**
  `stellar.account_cohort_positions{,_staging}.amount` moves from
  `Float64` to `Int256`. `deploy/clickhouse/account_cohort_rollup.sql`
  is `si-apply-scope: operator`, so deploy does not apply it. Immediately
  before rolling the API binary, run
  `clickhouse-client --port 9300 --multiquery < deploy/clickhouse/account_cohort_rollup.sql`
  (idempotent). The ch-float lint baseline moves to
  `lint-migrations-ch-float.baseline`.
- **ops — verify-archive:** the monthly Tier E cron
  (`stellar-archivist scan --verify` against the local mirror) is retired
  along with its alert, rule test and runbook. The mirror was trimmed to
  `history/` + `ledger/` on 2026-05-21, so the scan failed on every result
  set and the staleness alert only ever measured that. The `-tier`
  archivist code stays for operator runs with `-archivist-url`; the
  scheduled tiers are A + B + D (#1637).

### Fixed

- **metadata — SEP-1 (#1634, #1635):** `sep1_status=unreachable` and the
  RWA funnel's served-nothing count read `sep1_consecutive_failures > 0`
  instead of `sep1_resolved_at`. A run judged a systemic outage on our
  side no longer publishes every touched issuer as having a broken
  domain. A key that failed on our side is unwound from the retry
  ladder at the end of its run, and a payload Postgres rejects
  (SQLSTATE class 22) counts as the document's fault.
- **rwa:** a classic row valued by the listing directory or a
  prospectus constant NAV is no longer withheld as
  `reference_unavailable` when the oracle read fails; only oracle-bound
  rows depend on it (#1630). `/v1/rwa/history` sends `as_of` and
  `stale` on a carried-forward series and no longer shares its build
  with the caller's context (#1623).
  Contract-only rows are no longer grouped under a blank `by_issuer` key,
  and `home_domain` carries the issuer's domain rather than the listing
  directory's (#1636).
- **explorer:** convert pages read identity from `/v1/external/assets`
  and bake only served hub tickers (#1629). The markets OHLC strip
  soft-fails, so an hour whose trades were all filtered as outliers
  cannot abort the export (#1631).
- **changesummary:** the four native/fiat entities are no longer
  emitted (no trade is recorded against them), and a failing pass warns
  once instead of logging each failure at Debug (#1632).
- **recognition:** events whose `topic[0]` is not a Symbol are split
  into distinct shapes by `topics_xdr[1..2]` and arity, so one
  recognised exemplar no longer hides its unrecognised siblings (#1622).
- **api — protocols:** when the materialised contract-activity read
  errors, the protocol page's enrich block falls back to the raw
  `contract_events` read (already bounded by the raw scan ceiling)
  instead of degrading to empty (#1645).
- **timescale:** `PoolsFilter` with no sources binds an empty `text[]`
  rather than `NULL`, so "no source filter" means match everything (#1644).
- **timescale — CCTP:** the per-chain volume series breaks ties on
  `chain_key`, so two chains with equal window volume no longer interleave
  into many one-row series and the top-5 cut is stable (#1646).
- **ingest — backfill-router:** the default bucket is the archive bucket,
  falling back to the live one only when no archive is configured; a
  historic range against the trimmed live bucket used to exit 0 with
  "done. 0 ledgers" (#1649). `ch-gate` takes the same default, so a gate
  over a backfilled range no longer walks 0 ledgers and passes (#1651).
- **tests:** the checkpoint parent-directory test drives the production
  `fetchOne` path instead of creating the directories itself (#1647); the
  SDK spec-contract harness unions `allOf` required lists and walks
  nullable `oneOf` fields, so `AssetDetail.unverified_warning` and
  `.fiat_code_anchor` are now compared against the Go types (#1650); the
  auth rate-limit cross-key isolation test can now fail (#1642); the
  Chainlink `AnswerUpdated` topic0 is pinned to its known keccak256 (#1643).
- **docs — launch checklist:** the public-flip dry-run and customer demo
  boxes are struck; neither applies at 1.0 (#1648).
- **api — backups:** the diagnostics cache lock is no longer held across
  the rebuild and the response write, so one slow rebuild cannot stall
  every concurrent `/v1/backups` read (#1641).
- **chops — rebuild:** orphaned events evicted during a rebuild are
  reported in the summary instead of dropped from the count (#1640).
- **ingest — issuer-enrich:** a non-positive `-batch` is rejected before
  the command opens any connection (#1639).
- **diagnostics — rpc-probe:** the getEvents probe is skipped when the
  node reports `latestLedger` 0 instead of underflowing the range (#1638).
- **ansible — redis:** the role drops a `CONFIG REWRITE` `nopass` ACL
  line that overrode `requirepass` (#1627).
- **ci:** the nightly chaos job gets the dev stack's Postgres DSN (#1625).
- Band relay trailing-arg tolerance pinned by a test; stale runbook
  citations and an overdue retirement corrected (#1626, #1633).

## [v0.93.0] — 2026-09-29

166 commits since v0.92.1 — self-service account erasure/export, a
dashboard-secret boot check (OPERATOR ACTION REQUIRED, see below), and a
coverage-honesty pass over the history/rate/account-state read paths.

### Added

- **api/platform — self-service account erasure and data export (GH #809):**
  `DELETE /v1/dashboard/account` and `GET /v1/dashboard/account/export`,
  backed by one `accounterasure.Eraser` writer shared with the new
  `stellarindex-ops account-erase` CLI. Erasure runs as one transaction
  (close, scrub `audit_log` subject-identifying fields, delete invites,
  alerts, webhooks, keys, usage events, magic links, users, rename
  `usage_daily` rows to an `erased:<uuid>` subject, tombstone the slug so
  a reused email can't inherit a prior member's Redis/usage state) and
  reports a committed erasure as done even when only its cleanup step
  fails. Export covers Redis-held self-service key/usage rows. Erasure
  and account creation are serialised per slug behind an advisory lock;
  billing-address login state is preserved when only a member address is
  erased. Migration 0188 adds `audit_log_erase_metadata()` and the slug
  tombstone table.
- **supply — provenance stamping for reserve-account seed observations
  (migration 0189):** `supply seed-observations` now stamps a checkpoint
  row on a full pass, matching the SAC-balance and claimable-balance
  seeders; a partial, failed or `-dry-run` pass stamps nothing.
- **ops — `supply verify-rollup` on a timer with alerts:** new
  `supply-verify-rollup.{service,timer}` on archival-node hosts (daily
  06:53 UTC, read-only), gated on `watched_sep41_contracts` being
  non-empty; unit-failed/stale/never-initialized Prometheus rules and a
  `-textfile-output` scrape target replace the pasted-transcript verdict.
  `supply audit -cross-check` now prints both cross-check legs instead of
  only the pass/fail verdict.
- **metadata — SEP-1 identity-change history (migration 0190):**
  `issuer_identity_history` records one row per changed field
  (`OrgName`, `ORG_URL`, `ORG_LOGO`, currency `Image`, …) whenever a
  refresh overwrites an established issuer's SEP-1 payload; the
  data-freshness watchdog now keys off the last *successful* fetch
  instead of the last attempt, so a domain failing every refresh no
  longer reads fresh.
- **streaming:** a resume past the replay ring's eviction floor now
  emits an explicit `stream_gap` event naming the requested and oldest
  available cursor, instead of silently truncating; SSE connections open
  with a retry hint in the prelude.

### Fixed

- **auth — dashboard MAC keys derived per purpose, secret now required
  (OPERATOR ACTION REQUIRED, #1615):** the login-code HMAC,
  passkey-ceremony cookie, magic-link tag and login-device marker each
  MACed under one shared root secret; each now derives its own
  HKDF-SHA256 key. The API refuses to boot when passkeys are wired (every
  host that mounts the dashboard) and `STELLARINDEX_DASHBOARD_CODE_SECRET`
  is empty. In-flight codes/links/ceremonies/device markers invalidate
  once on deploy; hashed sessions are unaffected. The ansible env
  template (#1618) now refuses to render on a dashboard-mounting host
  with no `vault_dashboard_code_secret`, rather than deploying green and
  crashlooping the API.
- **ops — archival-node Redis requires auth (T641):** `requirepass` +
  loopback bind, wired into api/indexer/aggregator and the Prometheus
  redis exporter via a new vault `redis_password` (empty by default, so
  a fresh checkout fails ansible's preflight assert instead of shipping
  an open cache).
- **security — redirect/origin leakage:** the API's CORS lists trust
  only served origins; the dashboard client and vendor HTTP sources (CEX
  connectors) keep their API key on the origin instead of forwarding it
  to a redirect target; the Chainlink Ethereum RPC client refuses to
  follow any redirect.
- **chainlink:** rounds older than a per-feed `MaxAge` (heartbeat-derived;
  `[external.chainlink.feed_map]` gains `max_age_hours`) or with an
  undecoded carried-forward `answeredInRound` are refused instead of
  projected as a fresh publication; per-feed poll outcome and
  last-success metrics replace the single tick-level WARN.
- **api — coverage-honesty gaps on history/rate/account-state responses:**
  `/v1/history/since-inception` flags row-cap truncation
  (`row_cap_truncated` + `data_ends_at`) instead of silently stopping
  mid-series; `/v1/vwap`, `/v1/twap` and `/v1/ohlc` clamp an explicit
  `to` inside the still-filling bucket the same way a defaulted `to`
  already was, and surface it as `clamped`; `/v1/accounts/{g}` stamps
  `as_of_ledger` from its own 30s cache vintage instead of a
  serve-time watermark read that could outrun the cached body; the SDEX
  orderbook handler now flags `stale` when the source has stopped
  advancing instead of always reporting fresh.
- **streaming:** a multi-topic SSE resume (up to 9 alias topics) merged
  each topic's buffered replay sequentially instead of by id, so
  resuming after a Hub-wide id gap walked backwards at topic boundaries;
  replay is now merge-sorted by id across topics before sending.
- **api — spec/handler drift:** reconciled OpenAPI vs. handler on
  methodology, diagnostics and required/nullable fields; monthly-quota
  429 now sends `Retry-After`; SSE streams are re-billed periodically
  instead of once at open; refused mint-scope escalations are counted,
  not just logged; the `/v1/rwa/assets` rate-limit charge is weighted by
  its listing reads.
- **storage:** unbounded latest-row hypertable reads are bounded, and new
  ones are lint-gated from being added unbounded; the trades floor is
  derived from the newest *compressed* chunk; a baseline minute only
  counts past the notional floor; explorer sums/ratios and source
  contributions are computed as exact decimals rather than float
  aggregation; `contract-code-history` ClickHouse reads fall back to the
  legacy scan on an empty index; the aggregator's baseline-bar VWAP
  narrowing is marked explicitly non-monetary.
- **ops — monitoring/gate gaps closed:** cursor-hold, trustline-error and
  cap67-heartbeat detection; `archive-completeness fix`/`verify` gated
  behind `-write`; `detect-gaps` catches reaped catalogue sources and
  stale RPC tips; `curated-rwa-sync` asks Dune for a valid execution tier
  (it asked for a tier Dune doesn't name, so every keyed run 400'd);
  `sdex-claim-audit` fails on a silent reader/tx read drop; two more
  vacuous-pass shell-gate holes closed (#1097, #1093).
  `prometheus-rules-drift`'s live-check compares recording rules, not
  only alert rules, closing a weekly false-positive; the MinIO
  Prometheus scrape token is now a dedicated service-account secret
  (rotation-safe — rotating MinIO root no longer invalidates it)
  instead of a root-signed JWT.
- **ops — MinIO repinned to a pullable source:** MinIO Inc closed
  anonymous registry/binary pulls in 2025; images and binaries now come
  from `cgr.dev/chainguard/minio`, pinned by digest. An apply keeps an
  existing host's binaries unless `-e minio_allow_binary_upgrade=true` is
  passed, so this does not touch r1's running MinIO by default.
- **dashboardauth:** credential cookies are host-only and the
  login-intent witness is keyed to match; magic links are bound to a
  per-browser id so a throttled login can't evict another session; the
  durable code budget is only charged when a code is actually live;
  passkey add/remove notifies the account holder; dashboard-minted keys
  are mirrored into the Redis validator store; the login-client
  description is drawn from a closed vocabulary.
- **auth — scam-gate the aggregator tier of the asset headline;**
  `RateLimitBySubject`'s skip predicate is now exercised by test; admin
  key minting inherits the caller's rate limit when unset (GH-1147).
- **a batch of Intel-track fixes** across pricing, storage, ops and
  explorer surfaces (two batches, 22 issues: #1331, #1332, #530, #594,
  #600, #1346, #948, #834, #799, #767, #1261, #1148, #645, #711, #605,
  #1055, #1235, #1337, #724, plus a macOS lexicon fix; #1541, #1540).
- **W3 slice 78:** usage bookkeeping now runs on panic as well as the
  success path, with a usage-write alert; explorer price-freshness
  fixes.
- **release process:** `cut-release.sh` refuses to tag on a missing,
  stale or failing weekly SLA proof.
- **ci:** textfile discovery, budget floor, lexicon regex and a SIGPIPE
  gap fixed in the served-value gates; `lint-doc-links` no longer
  silently skips an unreadable file; `lint-migration-commands` no longer
  crashes under bash 3.2; migration-compat lint gains a `dml` class;
  gosec SSRF/nilerr false positives silenced on HEAD; Served-path smoke
  now guarded by the same `if:` as fleet-parity.
- **deps:** routine npm/Go/GitHub Actions bumps; the golang 1.27
  Dockerfile bump was reverted until `go.mod` moves with it.
- Various: `funlen`/`gocognit` cleanups, SDEX `op_index` fanout guarded
  against claim-index overflow, three cache-control mis-bands corrected,
  `LatestOracleStreams`' dropped-row counter increment fixed,
  `defaultRequestTimeout` exercised on a route with no handler-level
  wrap, the response cache write skipped when a dead context assembled
  the body, a `NULL`-start TWAP-view refresh refused outside migration
  headers, `window_seconds` omitted on tip fallbacks with concurrent tip
  ticks capped, the pricingguard call kept inside `LatestVWAP`,
  `Worker.Run` made one-shot for its lifetime (Q143), `Counter.Read`
  guarded against a nil receiver, the heavy-job scope's memory cap moved
  onto the unit wrapper, the ledger-read undercount metered and alerted
  (T399), the verify sweep alerted when permanently idle (T132), and the
  `ops_batch` ClickHouse user granted `system` database access so
  `compute-completeness`'s event census can read `system.parts`
  (already hot-patched on r1; this codifies it).

## [v0.92.1] — 2026-09-28

One day, 20 commits since v0.92.0 — a deploy-safety release.
**v0.92.0 must not be deployed.**

### Fixed

- **migrations — 0174 neutralised before any environment applied it:**
  the 0174 shipped in v0.92.0 ran `decompress_chunk` over every
  `sep41_transfers` chunk before adding `sep41_transfers_amount_check`,
  in one implicit transaction — on r1, 32 chunks, 55 GB compressed /
  780 GB decompressed. Deployed under the 5-minute `statement_timeout`
  migrate runs under, it fails partway with 0164 and 0166 already
  committed (cctp/rozo emptied, `prices_1m`/`twap_1h`/`twap_1d`
  recreated `WITH NO DATA` under the v0.91.0 binaries), version dirty
  at 174; out of band it is hours of chunk locks and ~725 GB of writes
  through a `pg_wal` that, until this release's ZFS move lands, still
  sits on a 15 GB root filesystem. golang-migrate runs 0174 before any
  later number, so a corrective migration can't help: the UP body is
  now `SELECT 1;`, under `migrations/README.md`'s narrow "neutralising
  an unapplied migration" exception — nobody we operate had applied it
  (measured 2026-09-28: `schema_migrations` = 162, dirty = false on r1,
  testnet and futurenet, and v0.92.0's release assets show 0
  downloads). The amount rule — any amount present is `>= 0`,
  `transfer`/`approve` rows must carry one — is now enforced solely by
  `validateSEP41TransferRows`, shared by both write paths (the COPY
  writer validated nothing before; the per-row writer checked the sign
  only on `transfer`/`approve`, missing a negative `set_admin`/
  `set_authorized` amount). The database `CHECK` becomes a later,
  offline step, tested against `pg_constraint` first. Anyone who built
  from source since 2026-09-24 (`d65ddaae3`) and applied the old 0174
  keeps the `CHECK` — a superset of the Go row contract, so it stays
  satisfied, not violated — and the down migration (`DROP CONSTRAINT IF
  EXISTS`) reverses either state.
- **migrations — 0164's header corrected, its replay needs `-write`:**
  the header claimed neither `cctp_events` nor `rozo_events` runs a
  compression policy. Both do on r1 (measured 2026-09-28: `cctp_events`
  148,660 rows across 21/23 compressed chunks, `rozo_events` 407 rows
  across 31/32) — 148,660 is above
  `max_tuples_decompressed_per_dml_transaction` (100,000), so a
  decompressing DELETE would fail the cap. It doesn't: the unqualified
  DELETE takes the direct compressed-batch path and never decompresses.
  Body unchanged; the header now says so, and its replay commands gain
  `-write` (they were dry runs as written) and `-refresh-caggs=false`
  (cctp/rozo write neither trades nor oracle rows, so the default
  refresh would burn a full-range price-cagg pass for nothing on top
  of 0187's own re-materialisation walk). A new test seeds chunks past
  the cap, compresses them, and proves the direct batch delete clears
  both tables without hitting it.
- **migrations — 0187 operator steps: archive `pools_per_source_1h`
  first, refresh 14 days:** header-only. 0187 drops and recreates
  `pools_per_source_1h` `WITH NO DATA` from `trades`, but the dropped
  view was materialized when `trades` held history it no longer holds
  (r1, 2026-09-28: the view's 2023-06 buckets sum 76,392,293 SDEX
  trades against 35,624 `trades` rows now live for that month) — the
  rebuild can't re-derive it. The header now tells an operator on such
  a deployment to copy the materialization hypertable into a plain
  archive table first (refresh job paused, one transaction per year,
  exact count/sum check before migrating) and to keep reading the
  archive for that lost history. It also corrects the
  re-materialisation window itself: the recipe refreshed 7 days, but
  `/v1/pools` and `/v1/markets?source=` read the view over
  `MarketsRecencyWindow` (14 days), so a pool whose last trade was
  7–14 days old would vanish from both until the walk reached it.
- **ops — ZFS snapshots of `data/postgres` are recursive; ansible
  codifies `data/postgres/wal` and tunes it for WAL:** r1's `pg_wal`
  has been a symlink to `/pgwal` on the 49 GB root filesystem since the
  2026-05-17 pool-full emergency; measured 2026-09-28, root had 15 GB
  free against ~210 GiB/day of WAL, so an archive stall of ~1.7 h fills
  it — the 2026-09-16 P1 shape, recurring. Ansible now codifies a
  `data/postgres/wal` ZFS dataset (128K recordsize, lz4,
  `logbias=latency`, `refreservation=256GiB` so no sibling dataset can
  starve it) and renders `wal_init_zero`/`wal_recycle = off` only when
  `pg_wal` sits on ZFS; `postgres_max_wal_size` moves to 16 GB (2 GB
  forced 84% of checkpoints, and the guard previously refused 16 GB
  while `pg_wal` was still on root, so it couldn't land before this
  move). The symlink move itself stays a manual operator step inside a
  Postgres stop window. `zfs-snapshot.sh` and `zfs-snapshot-now.sh` now
  snapshot and destroy `-r`ecursively for every managed dataset, so the
  new WAL child is captured atomically with its parent instead of
  silently missed — a non-recursive snapshot of `data/postgres` alone
  would lack the WAL of the same instant, unable to start without
  `pg_resetwal`. The Postgres ZFS runbook is corrected to say `pg_wal`
  has not shared a dataset with the cluster data since the 2026-05-17
  move (so every snapshot since then needs pgBackRest for WAL, not the
  ZFS snapshot alone), and gains rollback and clone procedures — the
  rollback step now checks `pgbackrest info`'s next available timeline
  before promoting, so a rehearsal or the offsite restore-drill can't
  collide with a timeline number already taken. The root-disk alert now
  tells a responder how to check whether `pg_wal` has moved yet.
- **ops/clickhouse — the d3 MV-tip gate, the wasm code-history
  instance-miss fix, and a dependabot Go-minor hold also landed since
  v0.92.0:** `d3` cutover now refuses unless a reproject-progress mark
  is strictly past the ledger the target MV was created at — the gap
  it closes is real: production's v2 MV was created at tip 63683991
  while the reproject that fed it used an exclusive `TO=63683991`, so
  that ledger's 221 `contract_data` rows, 221 TTLs and 6 offers were
  never reprojected, and the crossed SDEX order books that followed
  traced back to it. `ContractCodeHistory` now trusts a
  `contract_instance_changes` miss as authoritative instead of falling
  back to a full `ledger_entry_changes` bloom-index scan (44 GiB,
  30.6 s measured, past the API's 8 s deadline) — `/v1/contracts/{id}
  /code-history` no longer 503s for a contract with no wasm instance
  write; it serves `versions: []`. Dependabot now holds a Go minor bump
  in a Dockerfile pin until the matching `go.mod` toolchain change
  lands beside it (patch bumps still flow).

Also since v0.92.0: `warnOpenCORS` now catches a wildcard origin mixed
into a longer `allowed_origins` list instead of only a bare `["*"]`;
`BlendPoolReserves` stops inventing `decimals=7` for a reserve outside
the served config tier — a non-SAC token at a different exponent was
mispriced by a power of ten — and instead resolves decimals from the
reserve config, the SAC default, or the lake, withholding the USD
valuation rather than mis-scaling it; the explorer's asset table sorts
by the verified currency class instead of a constant field and gains a
market-cap-mismatch verdict cell; and `idx_lec_key_xdr` is declared at
the live `bloom_filter(0.01)` (the 0.0001 retune was reviewed and
rejected — its only reader no longer probes it).

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
