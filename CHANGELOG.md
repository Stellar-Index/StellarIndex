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

## [v0.130.0] - 2026-11-22

Tested against Stellar protocol 27. Migrations 0235-0236 (schema version 236): 0235 allows a customer webhook only one current signing-key form and refuses to apply, listing the ids, if a row holds both; 0236 caps `nonstandard_decimals_assets.decimals` at 38 and refuses to apply if a row is above it; neither rewrites rows, and both tables are tiny. Run the pre-check queries in `docs/operations/credential-rotation.md` (webhook keys) and `docs/operations/runbooks/dex.md` (decimals) before deploying (#4265). No `pkg/*` break. Behaviour change for operators: apply the host role (archival-node) before or with the new binary; the API now refuses to start with a ClickHouse serving password but no serving user, and the role renders the two together (#4258). After deploy, run `stellarindex-ops ch-backfill -config <toml> -changes-only -from 2 -to <tip>` once per host to extract ledger-upgrade entry changes (#4265). New optional setting `webhook_seal_previous_key_env` (default `STELLARINDEX_WEBHOOK_SEAL_PREVIOUS_KEY`, role variable `vault_webhook_seal_previous_key`) rotates the webhook seal secret; steps are in `docs/operations/credential-rotation.md` (#4265). Behaviour changes for API clients, all additive in shape: asset rows and the detail carry `circulating_supply_lower_bound` (#4266); `price_withheld_reason` appears on the global asset view and takes the value `source_excluded` when a price depends on an excluded source (#4263); a fiat price is withheld when its only FX rate is older than 76 hours (#4270); the RWA valuation status is `market_cap_withheld` with a reason for any refusal other than a missing supply (#4259); a scoped key can revoke itself (#4257). Behaviour change for sign-up: a first email sign-in has its own hourly counter with the same cap as `api.register_max_per_hour`, so the total ceiling is twice that setting (#4264). Deploys now restart the long-running units that run `stellarindex-ops` (#4254).

### Added

- **api:** `circulating_supply_lower_bound` on asset rows and the asset detail, set when a supply or valuation comes from a floor (#4266).
- **api:** `price_withheld_reason` on the global asset view; `source_excluded` marks a price that depends on a currently excluded source (#4263).
- **api:** the OHLC, chart and oracle reads flag `starts_before_window`, `thin_market` and stale points; a `raw:<symbol>` oracle leg is a 400 (#4262).
- **storage:** migration 0235, one current signing-key form per customer webhook; migration 0236, `decimals <= 38` on `nonstandard_decimals_assets` (#4265).
- **webhooks:** optional previous seal secret (`webhook_seal_previous_key_env`) for rotating the secret that seals signing keys; keys are re-sealed at startup and on customer rotation, and the API refuses a previous secret without a current one (#4265).
- **lake:** ledger-upgrade entry changes are extracted, kept out of entry history; `tag-tx-index` gains `-retag` (#4265).
- **monitoring:** ticket alerts for a unit still running a replaced release binary (30 minutes), for MEV worker failed runs outnumbering good ones over an hour, and for no MEV run recorded in an hour; a `pool` label on `registrations_total` (#4256, #4267, #4264).
- **ci:** a conflict-marker lint, a lint that every long-running unit running a release binary is on the deploy restart list, and a check that every package the integration tests import is a recorded decision (#4255, #4254).

### Changed

- **api:** served prices honour the source kill switch: `/v1/price`, batch, the tip and its stream, SEP-40, `/v1/vwap`, `/v1/twap` and the asset headline withhold or recompute a price that depends on an excluded source, and an unreadable exclusion list fails closed after 60 seconds (#4263).
- **api:** the 76-hour FX max age applies to every fixing, including the daily fallback; DEX TVL legs are priced the way `usd_volume` prices them (#4270).
- **api:** asset listings carry a stale supply map, a failed sparkline batch or a failed contract lookup in their flags and are not cached while degraded; the catalogue twin lookup keys on code and issuer (#4266).
- **api:** RWA, protocol and listing reads refuse for the same reason the asset detail does; a licence-barred NAV source is never served; markets, pools and pairs answer a retryable 503 when the scam gate could not finish a page; rows that lose a market cap count as unvalued and raise the totals' `lower_bound` (#4259).
- **api:** chart series are withheld or flagged where the asset detail withholds the figure; OHLC raw-trade reads are charged per book read (#4262).
- **api:** lake reads recover without a restart when ClickHouse is down at boot; until it is back they degrade the way a later outage does (#4253).
- **api:** `DELETE /v1/account/keys/{id}` lets a scoped key revoke itself and only itself; a self-revoke on the Redis key store also removes keys minted under it (#4257, #4270).
- **auth:** a first email sign-in draws on its own hourly budget; `registrations_total` counts passkey and email sign-ups (#4264).
- **storage:** markets skip buckets with no price; the completeness check reports the earliest missing ledger (#4265).
- **supply:** the write band checks the first write after a restart against the last stored total; it only flags, it never blocks a write (#4267).
- **explorer:** the asset page shows a withheld value as withheld with its reason, a stale one as stale, and a partial total as a bound; derived headline prices re-poll every 60 seconds; the holders board says it counts accounts only and lists the top 100 (#4261).
- **deploy:** a deploy restarts the long-running units that run `stellarindex-ops` and rolls back if one still runs the old build; HA restart handlers record a refused restart as owed instead of dropping it, and VRRP and Alertmanager gossip are accepted only from their peers (#4254, #4260).
- **ci:** the baseline, migration and test-selection guards no longer accept inputs they were meant to refuse, and the main-CI health check judges scheduled runs on their own (#4268, #4270).
- **docs:** corrected where a document promised more than the code does (#4269).

### Fixed

- **api:** listing valuation no longer serves a thin market's own rate, and a read that failed is reported rather than served as clean (#4266).
- **auth:** a failed self-revoke cascade can be retried by the holder, and a key minted during the sweep is removed (#4270).
- **deploy:** the API environment file no longer renders a ClickHouse serving password when the serving user is off (#4258).

## [v0.129.0] - 2026-11-21

Tested against Stellar protocol 27. Migrations 0233-0234 (schema version 234): 0233 makes `fx_fixings` append-only in the database, 0234 corrects a table comment; neither reads or rewrites rows. `pkg/client` gains `RevokeKeyWithReason`; `RevokeKey` is unchanged. Behaviour changes for API clients: asset detail, listing rows and the lending token block carry `market_cap_withheld_reason` (#4232); `DELETE /v1/account/keys/{id}` now revokes the calling key under the postgres auth backend, where it answered 503 (#4240); the staff account look-up answers 400 when both `email` and `slug` are given (#4235). Behaviour change for operators: the public API runs as its own OS user, `stellarindex-api`; apply the host role before deploying this release, which is phase 1 of the split (#4233). Alert change: a silence held for most of a day now raises a ticket and stops the dead-man heartbeat even if split or rotated; run `amtool silence query` before deploying (#4234). Further behaviour changes for API clients: a price is withheld with `price_withheld_reason` `decimals_mismatch` when a successful lake read contradicts the scale it was computed on (#4248); a supply whose latest refresh failed is served with `flags.stale` (#4249); `api.register_disabled` and `api.register_max_per_hour` now cover passkey and email-link or code sign-up as well as `/v1/register` (#4249); `/v1/rwa` `both_bases` gains `lower_bound` (#4244). Behaviour change for operators: `ch-rebuild -write` and the cap67 movements, entry history, entry index and lending state derive jobs now refuse a range they cannot cover (#4242); the first rules apply moves the prometheus-owned rule directory aside as a backup and needs the `prometheus` group, and an always-enabled timer that is disabled on a host now raises a ticket (#4246).

### Added

- **api:** `market_cap_withheld_reason` on the asset detail, listing rows and the lending token block, set whenever `price_usd` and a supply are served and `market_cap_usd` is not. Values: the direct price verdict (`substance`, `upstream_leg`, `manipulation_guard`, `fx_leg_unavailable`), `price_unread`, `thin_market`, `price_basis`, `supply_stale`, `decimals_mismatch`, `decimals_unknown`, `supply_floor`, `ticker_collision`, `low_liquidity`. No guard changed: no cap is served that was refused before, and none is withheld that was served. An absent reason beside a null cap, a price and a supply means a data or compute gap, and the field explains `market_cap_usd` only (#4232).
- **client:** `RevokeKeyWithReason` sends `X-Reason`, which operator callers must send; `X-Reason` is now declared on the operation in the OpenAPI spec (#4235, #4240).
- **deploy:** system user `stellarindex-api` (role variable `stellarindex_api_os_user`; setting it to `stellarindex` is the rollback) and `/etc/default/stellarindex-api` (root:stellarindex-api 0640), which holds only the names the API reads (#4233).
- **monitoring:** `stellarindex_registration_table_growing` also fires on more than 100 sign-ups in 7 days, counted from sign-ups so row reaping or a restart cannot hide it (#4234).
- **api:** `lower_bound` on the `/v1/rwa` `both_bases` totals, set when any summed row's supply is a floor; the RWA page marks each floor with "≥" and names its cause, labels the price basis (pegged, global, two-hop) on Price and Market cap cells, and the CSV gains the basis columns (#4244).
- **monitoring:** ticket `stellarindex_supply_write_band_breach` when a served `total_supply` jumps more than 10x, with a runbook section; the alert count is 401 (#4246).
- **ci:** `lint-timer-staleness.sh` fails when a role installs a timer that no alert rule names; its baseline lists the grandfathered timers and can only shrink (#4246).
- **storage:** migration 0233, a BEFORE UPDATE/DELETE/TRUNCATE trigger that makes `fx_fixings` refuse mutation; its one writer is `INSERT ... ON CONFLICT DO NOTHING`. Migration 0234 corrects the `aquarius_rewards_events` table comment to 12 event kinds (#4238).

### Changed

- **api:** `DELETE /v1/account/keys/{id}` revokes exactly the calling key when no account store is configured, so a `/v1/register` key can be killed by its owner; any other target gets the same 503 whether or not it exists. An already-revoked key is not revoked again and keeps its first reason and actor. A revoke that committed in one store and failed in the other writes a failed `key.revoke` audit row on the self-service and staff routes (#4240).
- **api:** the staff account look-up returns 400 when both `email` and `slug` are given; it used to resolve one and return that account. The key-store 503 names its real causes (no Redis, or `auth_backend=postgres`). Godoc, reference text and the spec example state what the server does (#4235).
- **monitoring:** `stellarindex_config_assertions_stale` also fires when the assertions textfile was deleted or never written. `stellarindex_alertmanager_silence_active` and the dead-man's silence check share one test, a silence active at 90% of a day's five-minute samples, so expiring and recreating a silence no longer resets either. The alert count is unchanged at 400 (#4234).
- **audit:** Reflector WASM `25fc3060` is registered as audited against nine real mainnet fixtures, three per oracle contract; the deployed bytes match upstream `reflector-pulse-contract` v7.1.0. The two earlier hashes stay unregistered until they have fixtures (#4239).
- **storage:** a bare soroswap-router routing tag is upgraded once a registered wrapper resolves, and a wrapper tag is never overwritten; `tx_index` is updated whenever the stored value differs, not only when it is NULL; the ClickHouse ops auth refuses a password with an empty username (#4238).
- **deploy:** the API unit runs as `stellarindex-api` with `WorkingDirectory=/`, loads only its own env file, hides `/etc/default/stellarindex` and sets `ProtectProc=invisible`. The completeness report moves to `/var/lib/archive-completeness`, seeded once from the old path. `stellarindex` leaves group `galexie` after the API is verified. A `pg_hba.conf` change reloads Postgres instead of restarting it. `lint-systemd-hardening.sh` fails an API unit that shares the indexer's user or env file (#4233).
- **ansible:** only the first Patroni host renders a bootstrap section and replicas wait for the cluster's initialize key; initdb uses peer and `scram-sha-256`, never trust; restart handlers restart a member only while every peer is healthy, for etcd too. Prometheus and Alertmanager rolling restarts stop at an unhealthy peer and basic-auth hashes are validated with promtool. A Redis Sentinel policy change no longer restarts every node, and the HAProxy API frontend sets `X-Forwarded-Proto` from the TLS state. These roles are not applied to any host (#4237).
- **explorer:** the asset page shows a live value, a value labelled as what it is, or nothing. The sidebar keeps its baked figures and caption when the live row is stale; a failed issuer-directory lookup reads "check unavailable"; the Supply tab marks Total as a bound; the Price card renders the live price; a `price_withheld` event clears the live figure; the Markets tab matches the page asset by identity, never by ticker; the USDC chart tab no longer prints a constant "about $1.00". Where a market cap is missing the page says why in words (#4236, #4232).
- **api:** a price withheld by the gated reader stays withheld: the catalogue row overlay no longer fills `price_usd` or clears a withheld reason, though a substance verdict can still be re-judged by the row's own-market check (#4243).
- **api:** a token's read decimals have three states: known, contradicted and unavailable. A contradicting lake read withholds `price_usd`, its change, cap, FDV, history and provenance with `price_withheld_reason` `decimals_mismatch`, on detail, listings and RWA rows; whole-token prices (peg, global market) are exempt. An unavailable read never withholds a price: it serves as before, the cap stays refused and the body is marked stale and not cached. `/v1/history` and contract transfers answer 200 with decimals omitted and the stale flag when the reader is not wired; a read error stays 503. Listings derive the Stellar Asset Contract of every classic row, which is 7 decimals by protocol (#4248).
- **api:** a contract-storage or event-net supply whose latest refresh failed, or whose refresh ran past its budget, is served with `flags.stale` on `/v1/assets/{id}/supply`, the asset detail and the RWA listing, and is not cached as clean; age alone never sets it. A storage level that fails the contract's own cross-check serves the event net as a lower bound. No served number changes (#4249).
- **api:** `api.register_disabled` refuses passkey sign-up and a first sign-in by email link or code, as it does `/v1/register`. `api.register_max_per_hour` is one budget across all of them, spent when the account is created (after the passkey attestation verifies or the email is proven); beginning a passkey ceremony spends nothing and existing users sign in as before (#4249).
- **api:** failed SAC events probes and Postgres tail reads on the asset filter, asset movements and holders routes report `degraded` (or the existing 503) instead of a clean answer (#4242).
- **ops:** `ch-rebuild` refuses `-sdex-gaps` or `-sdex-reconcile` without `-sdex`, and the two together; `-write` refuses a range the lake does not hold contiguously or a `-to` above the tip. The cap67 movements, entry history, entry index and lending state derive jobs refuse a `-write` whose `-from` is above the resume point (first runs need `-floor-ledger`); dry runs over any window still work and say when a write would be refused (#4242).
- **monitoring:** rules install by a root-owned directory swap: the rule set is built in a root-only staging directory (`root:prometheus`, 0750/0640) and renamed in, a failed install or reload restores the previous set, and a symlinked destination is refused. The stale-timer alerts cover `optype-rollup`, `asset-registry-backfill` and the holders-rollup timer, with absent arms for timers the role always enables (#4246, #4242).
- **explorer:** the sources page labels a pair's assets by code and issuer; the RWA page shows a shared ISIN and an overdue hand-vetted binding (no longer read as reviewed by hand), never shows a small non-zero premium as zero, and leaves the yield-bearing wrapper out of the Stablecoins and Combined tiles (#4242, #4244).
- **docs:** fifteen security and methodology documents (getting started, repo prep, HA plan, ADR status notes, supply and price methodology, lake reads) now state what key revocation, Redis, the origin lock and the supply caches do; no code changed (#4247).
- **ci:** the lake-dedup, baseline-growth, i128-sum, migration, migration-compat and imports lints each catch more statement forms; lake-dedup baseline entries carry a read count and a fingerprint of the excused query (#4245, #4251). Derive integration tests start their first run at `-floor-ledger`, and `internal/ops/chops` changes run the integration shards (#4250).

### Fixed

- **api:** a key can revoke itself under the postgres auth backend (#4240).
- **api:** the SEP-1 supply parser accepts only a plain decimal of bounded length whose result fits i128; a string such as `1p9999999` no longer costs seconds of CPU per request (#4243).
- **docs:** the migrations README records why there is no CHECK on `trades` amounts: on a hypertable with compressed chunks `ADD CONSTRAINT ... NOT VALID` still decompresses every compressed row under ACCESS EXCLUSIVE, so it is a planned maintenance operation; the rule stays in `canonical.Trade.Validate` (#4238).
