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

## [v0.128.0] - 2026-11-21

Tested against Stellar protocol 27. No migrations, no `pkg/*` change. Behaviour changes for API clients: `/v1/oracle/prices` on a frozen pair returns the single held value, or 503 when nothing is held (#4227); `/v1/assets/{id}/supply` answers 502 when the supply read fails, where a token with no flows got 404 or an empty answer, and serves a supply whose completeness check could not run as a lower bound (#4228). Behaviour change for operators: a bounded ledger walk by the ops tools over a range with a real hole below the datastore tip now exits non-zero where it used to exit 0 (#4229). Host payload changed: job locks move to `/run/lock/stellarindex/`, and the public API unit runs with `ProtectSystem=strict` and no writable paths; both are applied by the next host role run (#4230). On a host that already holds the `movements_by_asset_backfill` marker, run Step 3 of `deploy/clickhouse/movements_by_asset.sql` to verify it, and on a mismatch delete the marker and re-run Step 2 (#4229).

### Added

- **api:** `/v1/oracle/prices` answers carry `frozen_checked`; a frozen pair returns the held value flagged `frozen`, `degraded`, `stale` and `single_source` (#4227).
- **api:** `circulating_supply_lower_bound` on `/v1/assets/{id}/supply` and the RWA row, `market_cap_lower_bound` on the RWA row and the asset detail, and `lower_bound` on the RWA totals, set when the storage completeness check could not run (#4228).
- **ci:** `lint-numeric-mask` fails on any `to_char` numeric mask in Go or SQL (#4225).

### Changed

- **api:** `/v1/oracle/prices` goes through the same freeze check as `lastprice`: a frozen pair returns the single held value, or 503 when nothing is held (#4227).
- **api:** `/v1/assets/{id}/supply` answers 502 on a failed storage read whatever the flow count; the event-net supply is served as a lower bound when the check cannot run (undecodable balance entries, more entries than the cap, no instance entry), and only a Stellar asset contract is a real decline (#4228).
- **api:** when the contract supply arm refuses, is cut short or applies its own reading, the generic fill's supply, basis, cap and flags are cleared; an observer SEP-41 supply is kept when the arm merely has no reading, and its cap is recomputed through the dust and turnover guards (#4228).
- **api:** RWA views serve a cut-short curated valuation as lower bounds and say how many rows were not valued; a failed published or directory read, or a membership set that was never built, marks the view degraded (#4228).
- **api:** fiat-quoted `ohlc`, `vwap` and `twap` carry a `truncated` flag set by whichever read hit the cap, and every book is cut to the capped book's span; `thin_market` is measured over the peg books that put a trade into the answer, and a book that could not be measured never clears it (#4227).
- **api:** a chart whose grain is as wide as its window, including 1mo, reads back to the last closed bucket and the series is labelled `starts_before_window`; a protocols bespoke block's staleness and `as_of` come from the same cache read as the block (#4227).
- **api:** the seven change-% expressions render with `ROUND((expr)::numeric, 2)::text`, so a value over nine digits no longer prints as `#########.##`; values that fit the old mask are unchanged (#4225).
- **aggregate:** an escalated freeze (ADR-0019) ends only by operator action: a ladder whose hold lapsed while the aggregator was down stays live on restart reads, the recovery sweep, the marker rebuild, carry-forward and sibling checks, and a sibling window's release no longer clears it (#4226).
- **aggregate:** when the last frozen window auto-releases with its marker present, `ReleaseWindow` closes the open `freeze_events` row, so a re-fire opens a new row and sends its webhook; a close with no open row is success (#4226).
- **ingest:** a bounded ledger walk fails on a ledger missing at or below the datastore tip, on a delivered ledger whose sequence differs from the requested one, and returns the retryable `ErrLedgerArrivedDuringWalk` where it used to exit 0 with the range unread; the trailing-missing tolerance applies only to the distance from `-to`; the live path is unchanged (#4229).
- **storage:** when two upstream directory files name one address, the last copy's name, domain and tags win and only scam-class tags carry over from earlier copies; a listing directory sync refuses a snapshot whose prunes plus re-binds exceed the churn ceiling (20%, floor 5) unless `-accept-churn` is given (#4225).
- **lake:** `lake-dedup-driver` aborts when a merge removed fewer rows than the probe counted, sets `optimize_throw_if_noop` and skips partitions with a part written in the last hour; the `movements_by_asset` backfill steps stop on any failed or unmarked partition and insert the completeness marker only after verification (#4229).
- **deploy:** the restore-drill and galexie-archive-fill locks live in `/run/lock/stellarindex/` as root:root 0600, created at boot by tmpfiles.d, opened read-only and owner-checked; the heavy-job wrapper no longer honours old-path per-job locks (#4230).
- **deploy:** the public API unit runs with `ProtectSystem=strict` and no `ReadWritePaths`, hides `/run/lock` and `/etc/default/galexie*`, and sets `TasksMax`, `SystemCallFilter` and memory limits in both copies; `lint-systemd-hardening.sh` enforces it (#4230).
- **deploy:** stellar-core auto-upgrade reads the network tip only from the root-owned textfile and skips when it is missing or foreign; the MinIO scrape token is minted only when the prometheus user and `/etc/prometheus` exist (#4230).

### Fixed

- **aggregate:** a freeze row held open by a ladder logs at INFO once per hold, then DEBUG (#4226).
- **ingest:** probe errors in a bounded ledger walk name the step that failed (#4229).

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
