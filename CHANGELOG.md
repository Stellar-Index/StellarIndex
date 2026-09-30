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

## [v0.97.0] — 2026-09-30

36 commits since v0.96.0. Operator-visible: `GET /v1/operations` rejects any
`ledger` parameter with a 400 that names the new
`GET /v1/ledgers/{seq}/operations` route (OpenAPI 1.31.0); two new off-chain
feeds ship disabled — `[external.openexchangerates]` (`enabled = false`,
`OPENEXCHANGERATES_APP_ID`) and `[external.tiingo]` (`enabled = false`,
`TIINGO_API_KEY`), the latter valuing twelve WisdomTree fund shares at their
daily NAV once enabled; the forex worker polls every
`[external.massive] refresh_interval` (default 1h, floored at 10m); a new
`stellarindex-ops wasm-drift` check with two ticket alerts, for which the
repo ships no scheduling unit; `verify-lake`, `verify-contiguity` and
`verify-hashchain` with `-to 0` fail closed when the lake trails the history
archive tip by more than 100 ledgers; `compute-completeness` takes
`-timeout` (default 120m, `PASS_TIMEOUT` in the driver); `sla-probe` paces
at `-max-rps` (default 100); test nets stop serving pubnet reference
listings; the next archival-node apply renders WAL archiving and restarts
Postgres. After deploy, run `projector-replay -source phoenix -from
63295145` to add the Map-schema pool's liquidity rows (#2002). No new
migration.

### Breaking

- **api — `GET /v1/operations?ledger=` retired (#1992, #2007,
  OPERATOR-VISIBLE):** the one-ledger read is now
  `GET /v1/ledgers/{seq}/operations` (limit 1–2000, closed-ledger cache
  band); any `ledger` parameter on `/v1/operations` returns a 400
  `invalid-parameter` problem naming that route, so `/v1/operations` is the
  tip-advancing directory only (ADR-0018).

### Changed

- **ops — lake verifiers' auto `-to` (#1974, OPERATOR-VISIBLE):** with
  `-to 0`, `verify-lake`, `verify-contiguity` and `verify-hashchain` also
  read the tip from `stellar.history_archive_url` and fail closed when the
  lake trails it by more than 100 ledgers, so a Galexie stall no longer
  certifies the lake complete; an unreachable tip warns and is skipped.
- **ops — compute-completeness (#1879, OPERATOR-VISIBLE):** in `-pass` mode
  sources that re-verify from genesis run after the incremental ones, the
  recognition snapshot is written before the loop, a deadline stop names
  every skipped source, and the deadline is a `-timeout` flag (default
  120m) the driver passes through `PASS_TIMEOUT`.
- **sla-probe — request pacing (#1905, OPERATOR-VISIBLE):** `-max-rps`
  (default 100, 0 = unpaced) caps all workers through one limiter so a run
  no longer trips the per-key rate limit and pages on its own 429s; stats
  carry `failed_by_status` and the availability reason names the most
  frequent cause.
- **api — test-net listings (#1951, OPERATOR-VISIBLE):** on testnet and
  futurenet `/v1/sources` and its health route keep only sources that apply
  to the network, `/v1/assets/verified`, `/v1/external/assets` and
  `/v1/aggregators` return an empty list, `/v1/external/assets/{slug}`
  returns 404, and the pubnet warning stamps are skipped; pubnet and an
  unset network are unchanged.
- **ansible — archival-node WAL archiving (#1731, OPERATOR-VISIBLE):**
  `postgresql.conf.j2` renders `archive_mode`, `archive_command` and
  `archive_timeout`, gated on `pgbackrest_backup_enabled`, so a rebuilt host
  keeps point-in-time recovery; the next apply restarts Postgres with the
  values the reference host already runs.
- **ansible — pg-logrotate (#1897, OPERATOR-VISIBLE):** the hourly
  `pg-logrotate.service` reads a role-owned
  `/etc/stellarindex-pg-logrotate.conf` (stock policy plus `maxsize 500M`)
  instead of the uncapped distro file.
- **ci — deprecations (#1969):** `check-deprecations.sh` fails a
  `// Deprecated:` paragraph with no `vX.Y.Z` removal version in CI,
  `verify.sh` and `lint-changed`; the legacy tier constants are scheduled
  for v2.0.0.
- **release — build provenance (#1945):** `release.yml` attests every
  subject in `SHA256SUMS` (binaries and `migrations.tar.gz`) with
  `actions/attest-build-provenance` after signing, and a failed attestation
  stops the release; `release-process.md` documents
  `gh attestation verify`.

### Added

- **forex — Open Exchange Rates (#2011, OPERATOR-VISIBLE):**
  `[external.openexchangerates]` (`enabled` default false, `app_id` /
  `OPENEXCHANGERATES_APP_ID`) builds a provider for the hourly USD-base
  board, sending the app id only in the Authorization header and refusing a
  malformed board whole; the worker stores it but never fetches it, so
  serving stays massive then ECB. The worker cadence is
  `[external.massive] refresh_interval` (default 1h; under 10m is raised to
  10m and logged).
- **rwa — WisdomTree fund NAV (#2012, OPERATOR-VISIBLE):** a Tiingo poller
  in the indexer (`[external.tiingo]`, disabled by default,
  `TIINGO_API_KEY`, hourly) stores daily NAVs for twelve WisdomTree
  fund-share tokens bound on exact `(code, issuer)` as reference-only
  `raw:<TICKER>` rows; the new `fund_nav` provenance ranks between the
  oracle and listing arms with `decimals_published: 2` and no premium, a
  NAV older than 5 days is `reference_expired`, and oracle rows gain
  `nav_disagreement` past half a cent.
- **ops — wasm-drift (#2004, #2013, OPERATOR-VISIBLE):**
  `stellarindex-ops wasm-drift` resolves the current WASM hash of every
  audited gated source's contracts from the lake and flags any hash not in
  `internal/ops/chops/audited_wasm.json`, writing `wasm_drift.prom`;
  `stellarindex_wasm_drift` and `stellarindex_wasm_drift_stale` (no run in
  2 days) are ticket alerts with a runbook. sushiswap_v3 and upshift gain
  audit logs and manifest hashes.
- **web — legal pages (#2010):** `/terms` and `/privacy` are live, linked
  from the sidebar rail, footer and sitemap; the sign-up form states that
  creating an account accepts them, and `/pricing` describes usage as
  per-account.

### Fixed

- **forex — held rates after restart (#1995):** on its first refresh the
  worker seeds held tickers from the newest `fx_quotes` row within 7 days,
  so a fiat the upstream has not yet republished no longer drops out of
  `/v1/price` after a restart.
- **openapi — envelope declared (#1972):** the 38 enveloped 2xx data
  schemas declare `EnvelopeMeta` (`as_of`, `flags`) as `allOf`; the data
  subtrees are unchanged, and the 18 session-cookie dashboard/auth
  operations are named as the bare-on-the-wire exemption.
- **phoenix (#2002, #2001):** the Map-schema pool's `provide_liquidity` and
  `withdraw_liquidity` events decode into `phoenix_liquidity` rows (replay
  above); a bond-instrument contract that only shares the `"bond"` topic
  word leaves the curated stake set.
- **oracle — raw rows (#1925):** the `oracle_prices_1d` read behind the
  RWA history series drops `raw:` assets in both its keys and its SQL.
- **deploy — cut-over DDL (#1961):** the evidence step no longer refutes a
  ClickHouse DDL whose created objects are declared transient
  (`si-cutover-object`); it leaves it to the operator's acknowledgement.
- **markets (#1950):** empty commit; the SAC-spelling fold in
  `/v1/markets` and `/v1/pools` shipped in v0.96.0 as #1966.
- **docs:** the frozen-price flag docs state what a held value carries,
  including its own `observed_at` and `/v1/price/batch` (#1847); the launch
  plan's D1 freeze passage matches the measured state (#2009); the HA plan
  gains a ClickHouse lake tier (#1767); the design system records the
  Tailwind v4 browser baseline (#1930); the host-down runbook records the
  Hetzner Robot server numbers (#2008); the DNS/email perimeter's
  owner-side steps are closed (#2014); the metrics reference cross-links
  the monthly-quota fail-closed alert (#2020); the coverage doc cites the
  commit behind a reused PR number instead of the number (#1901).

## [v0.96.0] — 2026-09-30

58 commits since v0.95.0. Operator-visible: SDEX fills with one zero leg are
now stored as trades and serve `"price": null` (the field is nullable in
OpenAPI and `*string` in `pkg/client`); catalogue listing cursors name the
slugs already served, so an offset cursor from an older release returns 400;
dashboard sessions idle for over 7 days are revoked; `POST /v1/account/keys`
is limited to `apikey`/`operator` callers and self-service keys expire when
idle; `verify-lake` censuses all seven raw tables on a new daily timer; a
page inhibits only the ticket/info alerts of its own `alert_family`; and the
unread `idx_lec_asset` skip index leaves the ClickHouse schema (a live host
reports it as drift until the operator drops it). New migration: 0191 drops
the two `trades` amount CHECKs — catalog-only DDL that runs with compressed
chunks in place (no decompress) under the deploy's 5 s `lock_timeout`; its
commit carries a `Replay-Plan:` trailer (SDEX step 3f re-derives the served
trades with `ch-rebuild -sdex`).

### Breaking

- **pkg/client — `TradeRow.Price` (#1658):** now `*string`; it is nil for a
  zero-leg SDEX fill, which has no price.

### Changed

- **sdex — zero-leg fills, migration 0191 (#1658, OPERATOR-VISIBLE):** 0191
  drops `trades_base_amount_check` and `trades_quote_amount_check`, and its
  down refuses while compressed chunks or zero-leg rows exist;
  `Trade.Validate` rejects a negative or both-zero fill and admits one zero
  leg (`stellarindex_trades_zero_leg_admitted_total{source}`); price readers,
  the outlier filter and the sdex reconciliation count read only priceable
  rows; migration lint refuses a new division by a trade leg without a
  priceable guard.
- **api — catalogue pagination (#1904, OPERATOR-VISIBLE):**
  `/v1/external/assets`, `/v1/assets?asset_class=` and the unified
  `/v1/assets` catalogue phase page on the served slugs, so a re-rank between
  reads no longer repeats or skips a row; offset cursors return 400.
- **dashboardauth — idle sessions (#1967, OPERATOR-VISIBLE):** a session
  unused for `SessionIdleTimeout` (default 7 days) is revoked; API-key auth
  is unchanged.
- **auth — self-service API keys (#1849, OPERATOR-VISIBLE):**
  `POST /v1/account/keys` returns 403 unless the caller's tier is `apikey`
  or `operator`, and a self-service child key is stored with
  `MirroredKeyIdleTTL`, refreshed on every use, so only an abandoned key
  ages out; operator, admin and signup mints stay persistent.
- **ops — verify-lake (#1980, OPERATOR-VISIBLE):** a raw-table census checks
  transactions, operations, contract_events, results and participants per
  1M-ledger partition, writes `lake_verify.prom`, and runs daily under
  `run-heavy-job` with stale/failed alerts and a runbook.
- **alerting — page→ticket inhibition (#1973, OPERATOR-VISIBLE):** the 34
  family alerts across 11 r1 rule files carry an `alert_family` label and
  both Alertmanager configs inhibit a ticket/info alert only when it shares
  the page's `component` *and* `alert_family`, so one page no longer mutes
  every lower-severity alert on that component; `inhibit-rules-test.sh`
  runs in CI, `verify.sh` and `lint-changed`.
- **clickhouse — `idx_lec_asset` (#1990, OPERATOR-VISIBLE):** the bloom
  index on `ledger_entry_changes.asset` had no reader and leaves the tier-1
  schema and the retrofit script; a host that still carries it shows
  live-only drift in `ch-schema-drift` until step 4 of
  `tier1_skip_indexes.sql` (`DROP INDEX` under `run-heavy-job.sh`) runs.
- **divergence — CoinGecko reference (#1712):** the divergence price
  reference authenticates with `external.coingecko.api_key` /
  `demo_api_key` (Pro key → `pro-api` host) instead of hitting the public
  host anonymously; a failed batch logs status and path only.
- **ops — projector-replay (#1891):** the command's help, its run note and
  the replay decision rule state its generation limit: it writes at
  `derive_generation` 0, so it cannot correct a row a re-derive already
  stamped higher — use `projected-rebuild -write` after a decoder fix;
  `backfill-router`'s comments match what it does.
- **ci — package docs (#1769):** `lint-docs` fails on an `internal/` or
  `pkg/` package with no package comment; the Definition of Done asks for a
  `CAPABILITY-INVENTORY.md` check before new utility code.

### Added

- **clickhouse — live_daemon identity (#1939):** the indexer, aggregator and
  API read `STELLARINDEX_CLICKHOUSE_LIVE_USER`/`_PASSWORD` before falling back
  to `default`; the archival-node role provisions the flag-gated user.
- **api — `flags.pivot_unverified` (#1935):** set on `/v1/price` when a
  composite's pivot leg is made only of stablecoin-proxy prints; served
  prices are unchanged.
- **explorer — market chart (#1943, #1983):** the OHLC series downloads as
  CSV or JSON exactly as served, and an opt-in 1h/4h/24h trailing high/low
  band overlays the candles.
- **monitoring (#1918, #1751):** `stellarindex_sep41_supply_freshness_absent`
  fires when the SEP-41 supply freshness series vanishes, and
  `stellarindex_monthly_quota_fail_closed` pages on a sustained fail-closed
  quota gate.

### Fixed

- **api:** `/v1/markets` and `/v1/pools` fold SAC spellings into one row per
  pair (#1966); a dead domain's held SEP-1 payload stops serving as verified
  (#1875); `/v1/chart` walks read only the missing buckets and stop stamping
  `flags.stale` on healthy windows (#1975); SAC code history answers from the
  instance index instead of timing out (#1977); a computed 0%
  `completeness_pct` is emitted (#1944); `/v1/assets` resolves issuer home
  domains in one batched read and counts LCM fallbacks (#1940); SSE
  subscriptions past the topic-map ceiling get a 503 (#1942).
- **auth:** the API-key index is marked ready only by the build generation
  that walked it, and the API invalidates the index at start so records
  written raw become listable and revocable (#1857).
- **api:** `/v1/ledger/stream` connections share one cursors read per tick
  instead of each polling the store (#1915).
- **pricing / divergence:** the synthetic USD cross requires legs from
  independent publishers (#1929).
- **clickhouse / chops:** compute-completeness floors its scans at the
  network's Soroban genesis instead of the pubnet ledger, so the test-net
  unit no longer scans an inverted range (#1706); the cap67 watermark
  refuses to advance over missing contract events (#1931); hash-chain rows pair their hashes from one row
  (#1944); gated factory children preseed from the ClickHouse lake and an
  empty seed is an error (#1876); the chunk-restamp report counts clean
  chunks and config-assertions flags a paused `trades` compression policy
  (#1873).
- **deploy / build:** the config-apply gate baselines on the region's
  manifest set (#1959); r1 rule applies stage in a per-run `mktemp` dir
  (#1663); Go builder images are pinned by digest (#1949).
- **chaos:** scenario 04 asserts the limiter's MISCONF policy (#1922).
- **docs:** READMEs, runbooks and plans corrected to match the code at HEAD —
  sorocredit and defindex surfaces (#1984, #1981), the automated stale-deploy
  check (#1976), the monthly archive-trim timer (#1934), the withdrawn
  `drop_chunks` drill item (#1965), the phantom Aquarius router gap (#1971),
  and the divergence webhook payload (#1908); the WASM audit logs record the
  Soroswap factory `set_pair_wasm` rotation (#1991), the sorocredit
  early-window walk (#1997) and the Phoenix WASM lineage captured from the
  lake, including a 14th pool the registry did not know (#1996).

## [v0.95.0] — 2026-09-30

175 commits since v0.94.0: the inventory-closure waves. Three operator-visible
behaviour changes (`verify-archive -fail-on-missed` now defaults on, SSE streams
end after a bounded lifetime with a `:reconnect` hint, and `galexie-archive-fill`
reads its destination from `ARCHIVE_DEST`), a dashboard sign-out-everywhere
feature, and a long tail of read-path honesty fixes across the API, explorer,
ops tooling and Ansible roles. No new migration.

### Changed

- **ops — verify-archive (#1691, #1868, OPERATOR-VISIBLE):** `-fail-on-missed`
  defaults to on; a checkpoint-tier run that tolerates scattered misses must
  now pass `-fail-on-missed=false` (the deployed Tier B units already pass the
  flag). Tier D (`-tier peers`) compares every checkpoint the peers agree on
  with the local `history/` copy under `-archive-root` and fails if ours
  diverges or matched none of them; a `file://` peer that fails every fetch
  no longer passes as "unreachable".
- **api — SSE (#1882, #1854, #1738):** every stream gets
  `StreamOptions.MaxLifetime` (default 30 min + up to 10 % jitter); at expiry
  the server writes a `:reconnect` comment and ends the response cleanly, so a
  client that keeps its socket open but stops reading no longer holds a
  goroutine, connection and stream slot for days. A billable tick re-reads
  month-to-date usage and ends the stream once the subject's `MonthlyQuota` is
  spent (the reconnect gets the 429). Only the exact SSE routes are exempt
  from `RequestTimeout`.
- **ops — galexie-archive-fill (#1778):** the destination is read from
  `ARCHIVE_DEST` (default `local/galexie-archive`, so existing hosts are
  unchanged); anything that is not `<alias>/<bucket>[/<prefix>]` is refused
  before the first `mc` call.
- **ansible (#1672, #1701, #1667, #1669, #1710, #1696, #1851):** the test-net
  inventories are untracked like `r1.yml` (only `*.example.yml` stays in the
  tree) and both SSH hops verify against an untracked `inventory/known_hosts`
  instead of trust-on-first-use; `listing-sync` installs on pubnet only
  (`listing_sync_enabled`), with a retire block on the other networks; the
  textfile-collector directory mode is pinned; the redis-sentinel ACL file is
  loadable by redis-server; exporter binds are asserted after restart; the
  rollup units and `cap67-movements` can write their lock file and ops
  heartbeat textfile.
- **deploy (#1727, #1735):** `config_acknowledged` cannot clear a ClickHouse
  DDL surface the host did not answer for (the step publishes `unanswered`);
  `--refresh-manifest` is no longer offered as the missing-row remedy.
- **oracle — reflector (#1687):** `oracle.reflector.{dex,cex,fx}_decimals`
  make each contract's SEP-40 price scale configurable (0 keeps 14; values
  above 38 are rejected at config load).
- **external — kraken (#1729):** XLM/AUD, XLM/CAD and XLM/CHF leave the
  default pairs; Kraken never listed them and the subscription-rejected alert
  fired on every connect.
- **dashboardauth (#1655, #1666):** sign-out-everywhere; enrolling a passkey
  ends the user's other sessions; live sessions are capped at 10 per user,
  oldest revoked first.
- **monitoring (#1671, #1690, #1714, #1718, #1761, #1902, #1755):** an
  aggregate transactional-mail send-rate alert; a `webhook_deliveries`
  row-count gauge from the retention reaper; an alert for an optional
  Alertmanager receiver installed with no URL; the weekly Tier D textfile is
  excluded from the 24 h staleness rule; the escalated-freeze page links its
  own runbook; Healthchecks `/fail` pings are debounced and the aggregator
  heartbeat is skipped when it is off; incidents require
  `affected_components` and alert severity values are linted.
- **ci (#1719, #1740, #1837, #1862, #1794, #1785, #1804):** the unit-test job
  enforces the coverage floor (65.1 % total against a 54.0 % floor);
  `lint-apikey-scan` runs in CI and `verify.sh`; migration money columns are
  gated on resolved catalog types; the main-CI health check pages past
  cancelled runs; `lint-metric-refs` scans folded and chomped expr blocks;
  histogram fixtures are checked for label realism.

### Fixed

- **api:** `/v1/assets/fiat:USD` serves the identity `price_usd` (#1682); the
  SEP-40 point reads refuse a declared peg (#1661); served-price guard
  substitutions are surfaced on price and headline (#1670); one gate verdict
  is shared across queued price-stream buckets (#1665);
  `POST /v1/account/keys` is refused for SEP-10 subjects (#1668);
  `/v1/operations?ledger=` gets the closed-ledger cache band (#1709); every
  `/v1/issuers` limit is served from one ceiling-sized entry (#1760); a late
  pool-tokens flight no longer re-reads a fresh entry (#1798); `/v1/status`
  freshness query failures surface as absent counts (#1741);
  `?include=` on `/v1/markets` and `/v1/sources` is documented (#1834).
- **pricing / aggregation:** each pair's VWAP is read once per sweep (#1664);
  triangulation rejects fiat/fiat legs without USD (#1676); the
  `MinUSDVolume` floor compares as an exact `big.Rat` (#1853); the FX feed
  that published each fiat-cross rate is credited (#1836); a served snapshot
  price counts as priced in the tripwire (#1888); composite-reference metrics
  carry `windowLabel` (#1662); `known_anchors` outside an issuance-free fiat
  entry are rejected (#1736).
- **sources:** SEP-41 decodes the legacy admin-prefixed `set_authorized` id
  (#1846); unseeded SAC-wrapper supply is withheld at any total sign (#1852);
  sorocredit refuses negative amounts and counts unpromoted debt legs (#1886);
  blend_backstop genesis starts before the V1 backstop era (#1692); the SDEX
  decoder stops counting both-zero no-op claims as failures (#1878); MEV
  arbitrage cycle and venue guards run per connected component (#1839);
  metadata TOML closes strings and skips BOMs where the decoder does (#1657);
  `issuer-flags` stops gracefully when the timeout hits a read (#1812); the
  external `GetBody` read cap rejects non-positive values (#1802);
  backfill-external trade inserts are batched (#1884).
- **timescale / clickhouse:** positions folds are ordered before their venue
  cap (#1783); the batch-upsert decompression cap is lifted only on
  re-derives (#1864); per-source 24 h breakdown orders by USD volume
  (#1810); `account_activity` backfills the newest window first (#1694); the
  per-hash contract WASM fill is memoised and coalesced (#1746);
  near-unique segment-by tables stay off the compression list (#1856).
- **ops / chops:** `ch-rebuild -write` fails on lost rows and empty
  re-derives (#1723); the local statfs pre-flight is refused when the DSN
  host is remote (#1866); `projector-replay` records the ledger it actually
  rewound from (#1693); `archive-completeness -to` beyond the uint32 range is
  refused and `rehydratePaths` no longer wraps near `MaxUint32` (#1703,
  #1704); `ingestion_cursors` is captured in the CH schema snapshot (#1684);
  hubble-soroban-events filters on the real topics column (#1713); the
  config-assertions textfile is written atomically (#1702); watched contract
  instance removals are recorded in wasm-history (#1775); the fx backfill
  provenance label is skipped in data-freshness (#1711); RPC-failed batches
  are accounted in `fetch-wasm-rpc.py` (#1698).
- **ratelimit / streaming:** per-/48 key inserts are capped in the in-process
  limiter (#1659); caller cancellations stay off the fail-closed clock (#1686);
  a missing object below the datastore tip is refused and tolerated missing
  ledgers count as walk-complete (#1872, #1685).
- **explorer:** a partial lending-pool TVL renders as a lower bound (#1675);
  sub-cap dust prices render as a signed bound, not "0" (#1720); negative
  decimals scale on the string (#1674); the home archive-completeness light
  derives from `/v1/coverage` (#1699); lake staleness is stamped on the
  remaining lake-backed routes (#1869); `/exchanges` pair-table venues derive
  from the registry (#1792); asset page title and JSON-LD use `assetSymbol`
  (#1784); a failed `/v1/sources` fetch is distinguished from zero activity
  (#1700); the refresh-gate classes are bounded so client keys cannot fill
  them (#1892); proven SAC labels and the contract attribution registry map
  are cached (#1708, #1748); prev-ledger navigation is disabled at genesis
  (#1819); the recent-trades merge comparator is consistent on ties (#1807);
  stale Try-the-API responses are dropped after switching example (#1789);
  `crypto:`/`fiat:` ids match in the asset markets side label (#1788);
  SourceBreakdown shows its error state on a failed first load (#1790);
  venue pages are listed in the sitemap (#1796); long-tail shell pages no
  longer inherit homepage social tags (#1782); `/dexes/sdex` is framed as an
  order book of pairs (#1774); paged operations no longer flash page 1 on
  return (#1817); swap-picker crypto rows show the asset name (#1823); a
  mixed RWA reference total no longer claims oracle provenance (#1833).
- **load / chaos / dev:** the SSE scenario gates on subscribe success and is
  dropped from the mixed SLA scenario (#1813, #1779); the k6 production-target
  guard matches case-insensitively (#1787); chaos scenarios 02 and 04 measure
  what they claim (#1861); `verify-cdn.sh` no longer aborts on a zero-valued
  counter bump (#1786); `bootstrap-worktree` reinstalls a stale web
  `node_modules` (#1772).
- **docs:** runbooks, ADRs, README files and the launch plan corrected to
  match the code at HEAD — among them the phantom `make test-alerts` (#1907),
  the ch-gate and verify-archive invocations that could not run (#1732), the
  real trade-sink backpressure log line (#1818), the 8M claimable-balance
  index cap (#1800), the postmortem draft for the 2026-09-02 log-store
  exposure (#1855), and the reused pre-migration PR citations (#1896, #1899).

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
