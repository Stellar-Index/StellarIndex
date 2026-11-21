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

## [v0.127.0] - 2026-11-21

Tested against Stellar protocol 27. No migrations, no `pkg/*` change, no breaking API change. Not run by the deploy: run `stellarindex-ops ch-index-proof -write` once per host to record the proof watermark, which could not be recorded before (#4222); after an earlier `ch-rebuild -write` that failed after writing rows, refresh the price aggregates over that range with `stellarindex-ops trades-cagg-refresh -from L -to H -write` (#4223). Host payload changed: the alert rules in both rule trees (a new ticket alert) and the operator runbooks; run the host role from the tag to install the rules.

### Added

- **api:** `/v1/stablecoins` per-peg subtotals carry `assets_valued` and `lower_bound`, set when a member is unvalued, a supplied total is a floor, or the member list was truncated (#4218).
- **api:** `/v1/lending/protocols` serves a thin-market protocol-token price with `thin_market`, its substance evidence and `price_basis`; no market cap, emission value, APR or protocol KPI is computed from a thin price (#4221).
- **api:** lending reserve state reasons `reserve_archived`, `decode_failed` and `not_captured`; `no_decoded_reserve_state` stays in the enum but is no longer sent (#4221).
- **monitoring:** ticket alert `stellarindex_api_degraded_responses_high`, for a route where more than half of responses stayed degraded for 30 minutes above a floor rate (#4220).
- **monitoring:** `stellarindex_lending_reserve_undecodable_total` counts reserve decode failures, with a ticket alert (#4221).
- **ci:** `lint-ops-write-flag` requires every gated `stellarindex-ops` command in `docs/operations` to state `-write` or `-dry-run` (#4219).

### Changed

- **api:** lending reserve USD pricing follows the asset page's steps in order; a gated price reports `withheld`, a failed read reports `price_read_failed` and marks the response degraded, and a one-hop price carries `price_basis: transitive`. Backstop, auction, earnings and emissions routes do the same and are not cached when degraded; a degraded pools build is cached for 10 seconds, not a minute (#4221).
- **api:** asset detail gets a market cap for a price filled after the direct read, through the same guards as a direct price, and none when the direct read refused or failed (#4221).
- **api:** the catalogue market cap is the row's own price times its supply, and supply is stamped on an unpriced row; a twin the substance gate refused or could not measure gets no cap (#4223).
- **api:** asset detail 24h volume adds a lake-confirmed derived SAC the alias registry does not list, without counting a registered alias twice; a failed SAC lookup or read sets `volume_lower_bound` and marks the read degraded, and a classic asset's volume is a lower bound when no lake reader is configured (#4223).
- **explorer:** archived lending reserves are shown as a state with their own count (#4221).
- **ops:** `ch-index-proof` starts at ledger 2, the lake's first ledger (`-from` below 2 reads as 2), and a run without `-to` refuses when the lake has a hole below its highest ledger instead of recording a partial proof (#4222).
- **ops:** a `ch-rebuild -write` run that fails after writing rows makes the same aggregate-refresh decision as a successful run, or prints `FOLLOW-UP OWED` with the command, and still exits non-zero; a cancelled run prints the owed command and starts no refresh (#4223).
- **ops:** `cut-release.sh` refuses any SLA proof verdict except `PROVEN`; an override needs `SLA_PROOF_OVERRIDE_REASON`, which is echoed (#4219).
- **ci:** `lint-openapi-compat` also catches properties removed by dropping the `properties` block, loosening to a free-form object or moving fields into an `allOf` base, a removed 2xx response code, media type or body schema, and a request body re-pointed at a schema with a new required field (#4220).
- **docs:** wrong-row recovery runbooks name `projected-rebuild` followed by the aggregate refresh; backfill, credential-exposure, privacy-request, credential-rotation, corrupt-chunk recovery and test-net reset procedures give the real commands, with 37 runbook lines corrected; the durability, backup, pricing and TVL documents and ADR-0041 and ADR-0043 match the code (#4218, #4219).

### Fixed

- **pipeline:** trades the external retry buffer abandons at shutdown, or that are enqueued after its final pass, are counted once in the sink lane's shutdown loss, which names the venues of abandoned off-chain trades; the indexer's error line gives the refetch command. Nothing is written or acknowledged differently (#4220).
- **ops:** `ch-index-proof` no longer refuses a default run with "missing ledger 1", so its probe alert can clear (#4222).

## [v0.126.0] - 2026-11-21

Tested against Stellar protocol 27. Migrations 0231-0232: 0231 creates the empty `source_exclusions` control table, and 0232 switches `pools_per_source_1h` back to real-time when the view's watermark is within six hours (or `trades` is empty), otherwise leaves it as it is; it takes a five second lock timeout and rewrites no data. Breaking for API clients: `DELETE /v1/account/keys/{id}` changes its status codes, and `decimals` is no longer always present on three response types (details under Changed). Breaking for the Go SDK: `pkg/client` `AssetDetail.Decimals` changes from `int` to `*int`; there is no separate `pkg/client` tag, so this section is the notice. The OpenAPI contract carries `OpenAPI-Break:` trailers on #4193 only. Host payload changed: ClickHouse `account_cohort_rollup.sql`, `tier1_schema.sql`, `ops_by_source.sql` and `contract_instance_changes.sql`; the indexer, API and aggregator units; alert rules in both rule trees; the ansible HA roles and node-healthcheck. Not run by the deploy: stop `cohort-rollup.timer`, apply `deploy/clickhouse/account_cohort_rollup.sql`, check `system.columns` shows `Int256`, then restart the timer, before or right after rolling the binary (until then the cohort cycle refuses and the previous snapshot stays served); run the host role from the tag to install the alert rules and unit changes (rules otherwise reach hosts on the next rules sync); on a host where `pools_per_source_1h` stays materialized-only after 0232, refresh it and set `materialized_only=false` per `docs/operations/cagg-broad-recompute.md`; ranges rebuilt with `projected-rebuild` before this release keep stale price aggregates until `stellarindex-ops trades-cagg-refresh -from L -to H -oracle -write` covers them (#4194).

### Added

- **api:** BLNT protocol block, and USD values on lending protocol stats, backstops, emissions, auctions and account accrued interest (#4202, #4206).
- **api:** the listing valuation status `scale_unknown`, for a token whose scale could not be read (#4193).
- **api:** `flags.truncated` on `/v1/admin/status-notices` when more notices exist than were served (#4190).
- **explorer:** lending pages show dollar values, charts and names for Blend and BLNT (#4203).
- **ops:** `source_exclusions` control table and the `source-exclude` command (#4189).
- **monitoring:** alerts for five previously unread failure signals, a dropped pgBackRest WAL, a stalled cohort rollup or trades-by-account window, a stalled webhook worker, and an old directory snapshot (#4207, #4180, #4214, #4188).

### Changed

- **api (breaking):** `DELETE /v1/account/keys/{id}` answers 403 `key-not-delegated` for a key the caller may not revoke, including unknown ids (was 204); a key revoking itself answers 204 (was 409 `cannot-revoke-self`); a malformed id answers 400 `invalid-key-id`. A key may revoke itself and the keys minted from it, never its minter, a sibling or another root; account-wide revoke is for a SEP-10 session. Clients that treated 204 as "gone" must treat 403 as "not yours" (#4209).
- **api (breaking):** `decimals` is omitted, not served as 7, when a token's scale was not read, on the transfers response, `AssetDetail` and `PoolReserveToken`. Market cap, FDV, RWA valuation and stablecoin figures are refused for such a token. Clients must treat `decimals` as optional. Native XLM, classic assets and their SACs stay 7 (#4193).
- **sdk (breaking):** `pkg/client` `AssetDetail.Decimals` is `*int`; nil means the server omitted it (#4193).
- **api:** under `auth_mode=sep10`, callers behind one IPv4 address or one IPv6 /64 share one per-minute budget; API-key callers and a single SEP-10 user are unchanged (#4195).
- **api:** the 1h, 24h and 7d change figures compare against the last closed price at or before the target time (within 5 min, 30 min and 2 h), where they used a band around it; an asset with no such reference shows no change. `/v1/pools` and `/v1/markets?source=` 24h figures cover (now-24h, now] and are flagged a lower bound when the view lags more than six hours (#4197).
- **api:** a capped read is never served as the whole set: protocol `contract_count` counts every rostered contract, and a detail page whose analytics covered only part of the contracts reports the status `unavailable` (#4190).
- **api:** asset reads match every identity of an asset, and divergence `our_price` stays exact (#4191).
- **api:** RWA summaries and series, charts, depeg and divergence state what they cover and what they could not check (#4198, #4199).
- **api:** the listing market cap is exact and equal to the asset detail value; `org_name` is served only when the issuer's own `stellar.toml` lists that issuer; the storage floor is served when the event log is partial; Soroswap TVL counts pairs a read missed (#4212, #4188, #4185).
- **explorer:** the asset page keeps safety warnings, labels every price and converts exactly; RWA totals reconcile and floors are marked; a failed read shows an error and a partial list says so; the exchange and venue 24h totals show a lower bound with "≥"; the pair page shows the server's 24h volume (#4208, #4210, #4196, #4190, #4197).
- **auth:** key revocation is bounded to the key and its descendants, a retried revoke answers 204 for 24 hours, and a key id must be `kid_` plus 12-64 lowercase hex (#4209).
- **ops:** an external backfill never mixes fills and candles in one bucket, the gap finder sees range edges, and overrides are audited; a rebuild refreshes the price aggregates; `wasm-drift` checks every per-WASM source; `ch-backfill -changes-only` fails on an unreadable transaction (#4187, #4194, #4186, #4212).
- **ops:** the indexer restarts with backoff, the API unit runs with `ProtectSystem=strict`, node-healthcheck uses the read-only MinIO reader key, and heavy-job locks have tighter permissions and refuse unsafe lock files; the API and aggregator keep retrying through a long Postgres outage (#4201, #4180).
- **ansible:** the HA roles fail closed on re-run, restart one node at a time and refuse unsafe hosts; tasks that retire a timer or service fail on a real stop failure (#4205, #4195).
- **lending:** the state proof bounds its lake reads and reports what it did not compare (#4184).
- **monitoring:** the rules-sync self-tests run in CI and the deadman no longer fires under a day-long silence (#4214).
- **ci:** the lake-dedup, migration and baseline-growth lints close blind spots; new lints for unflagged `LIMIT`, rolling-window grain, Int128 sums, a defaulted `decimals` and swallowed ansible failures (#4215, #4190, #4197, #4200, #4193, #4195).
- **docs:** methodology, security, rollback, recovery and credential runbooks match what the tools do; key replacement is described for cascading revoke (#4185, #4183, #4213).
- **deps:** `next` and `eslint-config-next` 16.4.5 in the explorer (#4204).

### Fixed

- **supply:** account-cohort flow sums are 256-bit; three 2^126 receipts were served as -2^126. The rollup refuses a cycle against narrower columns and keeps the previous snapshot (#4200).
- **lake:** a claimable balance created and claimed in the same ledger is no longer seeded as live; undecodable supply and movement event bodies are classified, and non-token bodies no longer fail a run (#4216).
- **api:** `/v1/pools` is real-time again (migration 0232) (#4216).
- **api:** account movement pages keep their pinned boundary when the watermark read fails, and a failed first page writes no zero boundary into `next_cursor`; backups diagnostics serve the kept snapshot as unknown and stale when every Prometheus query fails (#4212).
- **ops:** a failed parallelism reset in the volume-character rollup no longer returns a capped connection to the pool; a rebuild's `-sep41-supply-only` refusal runs before `-preflight`; the sponsorship proof reports "not comparable" for a stale read instead of "disagree" (#4195, #4212, #4200).
- **ci:** a test-fixture key id no longer trips the secret scan; the doc-walking supply test tolerates a fixture that vanishes mid-walk (#4211, #4182).

## [v0.125.0] - 2026-11-17

Tested against Stellar protocol 27. Migrations 0227-0230: 0227 corrects the stored comment on `nonstandard_decimals_assets` (its rows drive a price correction and must not be deleted), 0228 sets 7-day chunks on the thirteen trades-derived continuous aggregates (new chunks only), 0229 adds `issuers.sep1_payload_previous` so a home-domain change leaves an identity-history row, and 0230 makes account erasure also strip the audit `reason`, a staff `actor_email`, and the erased account's own ip and user agent; the 0191 down migration is also made safer over compressed chunks. No `pkg/*` break; `pkg/client` gains `below_floor`, `substance` and `volume_lower_bound` fields. The OpenAPI contract gains `/v1/lending/protocols` and additive flags and fields; nothing is removed and no commit carries an `OpenAPI-Break:` trailer. Host payload changed: ClickHouse `lending_state.sql`, `contract_event_shapes.sql` and `tier1_schema.sql`, the lake dedup driver, the indexer, `verify-archive-tier-b` and `pg-logrotate` units, alert rules in both rule trees and `apply-rules.sh`, and the archival-node, haproxy, loki, patroni, prometheus and redis-sentinel roles. Not run by the deploy: apply `deploy/clickhouse/lending_state.sql`; run the host role from the tag before deploying; re-derive sponsorship history with `ch-entry-history -write -from 32747293`; backfill the ancestor chain of every legacy API key once.

### Added

- **api:** `/v1/lending/protocols` lists each lending protocol with its token, pool and reserve counts, and total supplied, borrowed and available in USD; a total that leaves out a pool or reserve is a lower bound and names what was excluded.
- **api:** lending account earnings carry USD values and net APR.
- **api:** OHLC bars and series flag `below_floor` when no trade cleared the $0.01 notional floor, and carry the measurement behind a thin-market bar; RWA rows carry `volume_lower_bound`.
- **explorer:** lending pages show floors, stale reads and the true auction fill; the asset page shows the supply basis it has.
- **monitoring:** alerts for failed key clamps, failed token sweeps, a stalled census rollup, and a degraded cold tier.
- **ci:** the integration suite runs in eight shards.

### Changed

- **api:** asset search finds catalogue assets by issuer or full asset id, and a thin-market twin keeps its supply when its market cap is withheld.
- **api:** the 24h change on the asset detail and batch price routes is absent when the price it compares against is more than 30 minutes off 24 hours ago.
- **api:** an Aquarius pool the reserve read did not value is counted and named, and makes DEX TVL a lower bound; a source's volume floor is flagged as a lower bound.
- **explorer:** the verified badge is decided by asset id on every surface; a look-alike SAC address redirects to its own asset; the converter uses the headline price and empties when that price is withheld; RWA figures show their bounds and staleness; a failed backups refresh reads "last known, not current".
- **pricing:** one notional floor decides which OHLC bars and which minutes may set a served price, and a rollup past its age bound is not served.
- **auth:** a key minted by another key never exceeds its minter in any field, stores its full ancestor chain, and revoking a key revokes every key minted from it.
- **lake:** the dedup driver rejects bad arguments and counts duplicates per partition before optimising; sponsorship ends when a consumed pre-auth signer is removed.
- **migrations:** `migrate force` refuses a version the dirty row does not justify unless `-force-unsafe` is passed.
- **ops:** a unit copied to a host is hardened and compared directive by directive; collectors that return nothing fail instead of passing; `ch-backfill` exits non-zero when any ledger was read short.
- **ansible:** the prometheus and loki roles wire basic auth, replica labels and restarts; the patroni, haproxy and redis-sentinel roles fail safe.
- **monitoring:** rule apply is safe to run twice at once.
- **ci:** the migration gates no longer have bypasses, the deploy numbering gate uses what the host ran, and eight lints are tightened.

### Fixed

- **api:** account positions are exact only when every event that moves them is counted, and a Blend or BLNT leg closes on a zero token balance; the curated gap compares totals of the same day.
- **lending:** earnings, claims and auctions that may be incomplete are withheld or flagged; a memory-limited history fill splits its window instead of failing, and a replayed hourly state row replaces the older one.
- **rwa:** premiums are judged against the published value, and partial and stale views are flagged.
- **pricing:** change summaries use only guard-validated prices, and a failed freeze-ladder read is no longer reported as never frozen.
- **auth:** account erasure leaves no free-text reason, staff email, or own ip and user agent in the audit log; a request line is never logged with a key in it, at any escape depth.
- **api:** `/v1/stablecoins` folds an expired classic-supply map into its stale flag.
- **lake:** the `contract_event_shapes` backfill includes the first ledger.
- **ops:** `ch-census-rollup` exits non-zero when a lake hole stops it short of yesterday.
- **docs:** the API design doc matches the code.

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
