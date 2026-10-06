---
title: v1 launch plan — outstanding work
last_verified: 2026-10-06
status: active
severity: P1
---

# Stellar Index — v1 launch plan (outstanding work only)

> This is the one launch plan. It holds only what is still outstanding: the
> go-live gate (§0), the launch sequence (§1), open and deferred work, decisions
> of record, and the rows that code and docs cite. The inventory
> (`inventory/items.jsonl` in the private tree) is the authority for every item.
> Closed work, loop logs and old evidence were cut under INV-0897 and stay
> reachable with `git show 52aacb972a5be5fe65e9d608227e8fe06dfe7fe2:docs/operations/v1-launch-plan.md`
> (called PRE-CUT below).
>
> Companions still active: `production-confidence-campaign-2026-07-23.md` (the
> adversarial proof harness) and the gitignored
> `production-remediation-ledger-2026-07-23.md` (finding-status authority).
> Runbooks under `runbooks/` are the execution recipes. Commit ids older than
> the 2026 history rewrite may not resolve; find the successor with `git log`
> by date and subject.

## What "v1 launch" means

Public, announced availability of the Stellar Index API + explorer as a
production service fit to present to Stellar: substrate certified complete,
served money-values proven correct, a signed repeatable deploy path, honest
capacity/HA posture, paging wired to a human, and the v1.0 wire shape frozen
(ADR-0042 — **Accepted and implemented**; the `kind` discriminator is live in
the spec, so the wire-freeze prerequisite is met).


## 0. Go-live gate: verified current state, 2026-10-06

Each row points at live items in the private inventory
(`inventory/items.jsonl`, snapshot `323689bf`, 2026-10-06 01:49 +01:00).
The item is the authority; this table is an index. A row goes when its last
item closes. The 2026-07-29 snapshot this replaces cited no items; it is in
git history.


| Gate | Items (status) | State |
|---|---|---|
| Launch cutover | INV-1201 (blocked), INV-0873 (open), INV-0839 (blocked, Ash) | Cutover waits on Ash's GO, a launch date and CDN provisioning. Announcement copy and the first-24h watch need Ash. Accepted-risk register sign-off deferred by Ash 2026-10-03 |
| Security review | INV-0871 (blocked, Ash), INV-0696 (blocked) | External review deferred by Ash to after the v1.0 launch. The private report's child findings are still open |
| Credential rotation | INV-0744, INV-0919 (Ash), INV-1178 (Ash), INV-1479, INV-1516, INV-2091 (all blocked) | One batch after inventory closure (Ash's 2026-09-30 rule). INV-1479 also waits on the verified B2 backup |
| r1 security defects | INV-0802 (in-progress), INV-1184 (blocked) | ClickHouse `default` user is unauthenticated; the ops users landed in #2380, operator steps remain. pgBackRest repo1 is `cipher-type=none`; the live re-encrypt is pending |
| Deploy approval gate | INV-0695 (blocked) | `DEPLOY_APPROVAL_RELAXED` is still set; deleting it is an r1 step |
| Paging + SEV drill | INV-0841 (blocked), INV-2615 (open) | Drill waits on inventory closure and 0 r1 alerts. Healthchecks check `stellarindex-futurenet-indexer` is not up |
| r1 alerts firing | INV-2548 (open), INV-2675 (open) | `stellarindex_zfs_pool_fill_90pct_within_7d`; `stellarindex_verify_archive_run_stale` |
| Capacity + backup | INV-2178 (blocked), INV-2524 (open), INV-0986 (blocked) | r1 pool reaches 85% around 2026-11-07 at 0.61 TiB/month. First full CH lake backup to B2 failed after 16 h on a 14 GiB limit. Off-site secrets backup waits on Ash's age/gpg choice |
| Config drift | INV-1911 (blocked), INV-0798 (blocked) | Scheduled `ansible-drift` is red; needs `redis_password` in the r1 vault |
| Lake entry ordering | INV-2567 (open), INV-1313 (blocked) | `ledger_entry_changes` rows extracted before `c4ab63e45` carry `intra_ledger_seq=0`; the reproject waits on the Go-walk re-derive |
| Eviction + restores | INV-2532 (in-progress), INV-2157 (open), INV-2159 (blocked), INV-2533 (open) | Lake writes persistent evictions as removed. Restored entries for ledgers 58,762,517–64,673,495 need re-extract. Lumen conservation residual +119,100,885,352 stroops (11,910 XLM) at ledger 64,767,268 |
| Explorer performance | INV-0596 (blocked), INV-0675 (in-progress) | Sub-second cold contract pages and the p95/p99 target both wait on a clean-window r1 re-measure |
| Oracle data | INV-2590 (in-progress) | RedStone Invert feeds store the reciprocal while `price_raw` claims the raw value; Fix merged in #2420; waits on deploy plus the redstone projected-rebuild on r1 |
| Catalogue pricing | INV-0202 (blocked, critical) | The XLM→USD catalogue anchor is keyed `base='native'` with a dead `fiat:USD` arm; needs a plan and adversarial review |
| Dashboard auth | INV-0768 (open, high; Ash disclosure decision) | Unauthenticated dashboard sign-in lockout; disclosure and remediation sequencing are Ash's call |
| SEV comms | INV-0145 (blocked, high, Ash) | Every SEV comms path depends on the live API; needs an out-of-band channel Ash chooses |

## 1. Launch sequence and gate rationale

Done and kept as evidence (details at PRE-CUT):

- Supply trustworthy: Horizon reconcile 8/8 PASS (2026-07-30, [evidence](https://github.com/Stellar-Index/StellarIndex/blob/0023bb9aefa96fb8231d9eabd160e6133eca39e9/docs/operations/evidence/2026-07-30-supply-reconcile-8of8.md)).
- Completeness green on every source, served on `/v1/coverage` (2026-08-01, [evidence](https://github.com/Stellar-Index/StellarIndex/blob/0023bb9aefa96fb8231d9eabd160e6133eca39e9/docs/operations/evidence/2026-08-01-completeness-17of17.md)). The count is now 20 sources.
- Prove-it battery passed; config codified equals live (`ansible-drift.yml` green 2026-07-27); `auth_mode=apikey_optional` and `/sla` published.

Still open, in order. Each step needs its inventory item closed first:

1. Migrations: run `stellarindex-migrate up` and confirm `schema_migrations.version` equals the highest file under `migrations/` and is not dirty, before tagging and swapping the binary (see "Cut: moved evidence").
2. `sip_` SLA-probe smoke and outside-internet `make smoke` 13/13 (INV-2707).
3. Refresh `post-launch-queries.md`, the Grafana launch-watch board and the F-0100 PromQL check (INV-2708).
4. Cutover on Ash's GO; first-24h watch staffed; every dashboard user re-logs in after migration 0143 (INV-2709, INV-0873).

Security posture, DR honesty and launch mechanics rows are in the §0 table above.

---

# Cut work: forwarding sections

Live items, code and docs forward here. Old `:NNN` line cites and `#L<n>` inventory pointers resolve at PRE-CUT, not at HEAD.

## Cut: header

- Pre-cut sha: `52aacb972a5be5fe65e9d608227e8fe06dfe7fe2` (PRE-CUT). Read the full old plan with `git show 52aacb972:docs/operations/v1-launch-plan.md`.
- Every old `:NNN` line cite and `#L<n>` inventory pointer resolves at that sha, not at HEAD. Migrations 0051, 0106 and 0107 cite `:2486`: that means the "no unbounded trade-scan queries" rule, now in [domain-traps.md](../architecture/domain-traps.md#unbounded-trade-scans-cancelled-refreshes-and-aggregation-pitfalls).
- Old row ids (1.x, W-x, D-x) cited by code resolve in "Cut: cited rows" and "Cut: decisions of record" below.
- Commit ids older than the 2026 history rewrite may not resolve; find the successor with `git log` by date and subject.
- Go-live gate is §0 (an index of INV items); the launch sequence is §1 (pre-cut §2.8 is PRE-CUT lines 4610-4636) plus INV-2707, INV-2708 and INV-2709 below.

## Cut: open work not in §0

One line per item; the INV item is the authority.

| INV | Item |
|---|---|
| INV-2690 | W8-15 CI/test gaps residue: goleak, revocation drift, TWAP coverage, lint-metric-refs comments |
| INV-2691 | W8-16 CS-068 live-ingest half: late trades never re-materialise in `prices_1m` |
| INV-2692 | `sep41_supply_events` never vacuumed (42/130 chunks, 276M rows) |
| INV-2693 | Orphan k6 API key and ephemeral k6 dirs on r1 |
| INV-2694 | Pin `stellar_archivist_version` for `stellar_stack_lagging` |
| INV-2695 | Pending r1 applies: CH drop-guard #286, `ops_batch`, ZFS snapshots |
| INV-2696 | Ops-CLI write-gate unification (about 15 money-path defaults) |
| INV-2697 | A failed classic op still lands a public participant row (no tx-success filter) |
| INV-2698 | W5.7 CEX dust DELETE (destructive, last) |
| INV-2699 | Update blog and methodology copy: the per-token oracle layer has shipped |
| INV-2700 | Top-50 prices check (CoinGecko key) |
| INV-2701 | `verify-usd-volume` estimated-tier alert at about $10M/day |
| INV-2702 | migrations-sync step may re-orphan the migrations dir uid |
| INV-2703 | Runbook lore: unattended-upgrades libc6 bounces Postgres about 06:24 and kills heavy jobs |
| INV-2704 | Root disk at 80%; durable fix is operator item #64 |
| INV-2705 | `WithMaxDormantComponentLedgers` calibration |
| INV-2706 | Distinct RedStone registry-stale signal |
| INV-2707 | Launch sequence step 2: `sip_` SLA-probe smoke and outside-internet `make smoke` 13/13 |
| INV-2708 | Launch sequence step 3: refresh `post-launch-queries.md`, Grafana launch-watch board, F-0100 PromQL check |
| INV-2709 | Forced re-login of every dashboard user after cutover (migration 0143) |
| INV-2710 | L5.8 and L4.15-L4.17 carried to W9 (gated on D2) |
| INV-1046, INV-1048, INV-0978 | Patroni cluster playbooks and replica-promote decision for single-node r1 |
| INV-0934 | Narrow `allowed_ssh_cidrs` (C6-041); IP rotation is its stable-admin-range precondition |
| INV-0596 | W3.3 cold-read residue: census, account family, holders, refresh gate |
| INV-0923 | DeFindex no-event vaults and the vault registry |
| INV-2157, INV-1313 | Re-extract and re-derive lake entry changes after the eviction phase (INV-2156 shipped) |
| INV-0846, INV-0848, INV-0849, INV-0874 | RWA open items; evidence moved to [rwa-coverage-reconciliation.md](../methodology/rwa-coverage-reconciliation.md#moved-from-the-launch-plan-what-stands-between-the-served-total-and-the-4087b-a-third-party-reports) |
| INV-0856, INV-1813 | SDEX history backfill driver; test-net lake writer can skip the in-flight ledger |
| INV-0885, INV-0887, INV-0894, INV-0902, INV-2140 | Sub-$100M SDEX price gating; per-view CAGG refresh lock; residual DeFi decoders; per-account deep trade history; `movements_by_asset` view |
| INV-0695, INV-0839, INV-0840, INV-0841, INV-0871, INV-0873, INV-0877, INV-1009 | Gate and launch rows (§0): deploy approval flag, accepted-risk register, restore drill units, SEV drill, security review, announcement, W7.2 visuals pass, HAProxy phase 1 |

## Cut: deferred post-v1

- HA, R2/R3 and ClickHouse HA (INV-0890, INV-1048, INV-1070, INV-1101, INV-1389). D2 accepts a single box per region with a tested restore; multi-region (ADR-0050) is post-v1 with the reasoning in `docs/architecture/ha-plan.md` §10.
- R1 is NOT hardware-upgradeable. Never propose drives.
- INV-2851: oracle-reconcile window netting (F6 = C2-16).
- INV-2852: oracle-reconcile fail-opens (F8-F10).
- INV-2853: caller-side supply close-timestamp fix (M4).
- INV-2854: `/assets/native` advertises 5,553 markets but lists 100.
- INV-2855: API auth and rate-limit review.
- INV-2856: dependency-advisory review cadence.
- INV-2857, INV-2858: operator decisions on peg-set thresholds and served-tier retention (Ash).
- INV-2859: instrument the Redis read-through caches (`cache_ops_total` covers only in-memory caches).
- INV-2860: projector-replay to the served tier to close the sep41 gap (register D4).
- INV-2861: HA blockers F-001..F-010 (HA itself stays post-v1, above).
- W9: DeFindex vault registry and unproven emitters (INV-0923), residual DeFi decoders (INV-0894), L4.14-L4.17 and L5.8 (INV-2710).
- CH Phase 8 `soroban_events` decommission (#803): destructive, last.
- Credential rotation is one batch after inventory closure (Ash, 2026-09-30). The MinIO root exposure of 2026-07-25 is closed: root is `stellarindex-admin`, the old key is rejected, and moving services off root remains hygiene (verified 2026-09-28).
- Intra-ledger-seq historical backfill: INV-1023 (blocked; its `blocked_on` names this plan).

## Cut: decisions of record

Copied from the 2026-08-29 table (old lines 1545-1570); text is condensed, the full rows are at the pre-cut sha.

| # | Decision |
|---|---|
| D1 | RATIFIED. Anomaly-freeze is implemented by shipping composite-reference corroboration (#288), not by editing the alert. Live in v0.50.0 on r1 since 2026-08-29. Measured 2026-09-30: 575 freeze ticks over 14 d were 10 freezes, median hold 31 min. Verify with `increase(stellarindex_anomaly_freeze_engaged_total[24h])` plus `stellarindex_aggregator_composite_freeze_suppressed_total > 0` (the second proves the mechanism engaged rather than the market being calm). |
| D2 | ACCEPTED-RISK plus a tested restore at v1. Single box per region; multi-region (ADR-0050) deferred post-v1. |
| D3 | SIGNED OFF. ClickHouse posture is ADR-0043 §2.1 schema-and-state snapshot plus re-derive, plus rolling ZFS snapshots (live 2026-08-29). Do NOT resurrect full-lake copies. |
| D4 | BUILD ALL THREE: order-book depth (#337), DEX TVL (#338), per-token oracle pages (#336). No retraction of site copy. This overrides any "[DECIDE build-or-drop]" text. |
| D5 | ACCEPTED. The retention contract is "we retain everything we index", not a set of windows. `trades` holds 2018-07-01 to now (738,248,187 rows on 2026-08-29). Migration 0031 removed retention from `trades`, `prices_1m`, `prices_15m`; 0040 from `oracle_updates`. Amended 2026-09-26 (#1168): the authoritative list of retention policies is `TestRetentionPolicies_AreExactlyTheDeclaredSet`; Go-side age deletes are in `TestGoAgePruners_AreExactlyTheDeclaredSet`; the MEV pruner was removed, so `mev_events` is retained. The limits worth telling customers are coverage: on-chain SDEX trades begin 2026-03-12 (#349); CEX series begin 2018-07-01 (Kraken) and 2026-05-05 (Binance, Coinbase, Bitstamp). |
| D6 | ACCEPTED as documented-unfillable: genesis edge [2 to 287,404]; recover via op-replay if ever needed. |
| D7 | Not a decision but owed work: the third-alias thin-pool VWAP review. DONE 2026-09-04, see row 1.9. |
| D8 | OVERRIDDEN to FIX FOR v1: `*_FUNDAMENTAL` RedStone feeds publish a NAV ratio in BTC but were registered `quote=fiat:USD`. Contained (`IncludeInVWAP=false`). Fix landed: 961cc8a97 (#348) quotes the SolvBTC NAV feeds in their reserve asset. |
| D9 | DROPPED. Stripe C3-081 reconcile closed as a formal DROP citing ADR-0049. |
| D10 | Privacy review reduced to a sign-off; PR #237 was closed unmerged, terms and privacy are live (INV-0150). Amended 2026-09-28 (#346): erasure and export were built in GH #809 (`internal/accounterasure`, migration 0188), superseding the PRV-1 drop; procedures in `runbooks/account-erasure.md`; identity checks and the other rights requests in `runbooks/privacy-rights-requests.md`. |
| DR | SIGNED OFF. Off-site posture: pgBackRest repo2 plus rolling ZFS snapshots; the nightly job writes every configured repo. Superseded by the B2-only off-site decision of 2026-10-02 (INV-1475, INV-1181 done). |
| W6.1 | CLOSED. Paging wired and proven: Discord (pages and alerts) and a Healthchecks.io dead-man. Caveat: whether an alertmanager receiver holds a real URL is NOT covered by the 0-failures `pre-launch-check.sh` measurement. |
| W8-13 | Decided: `stellar.trades_by_account` in ClickHouse for per-account trade history (old line 4745; INV-0902 tracks the open storage-design question). NOT built. Rejected: taker/maker in `trades`' `compress_segmentby` (recompresses every chunk, splits the pair-read segments) and keeping the bounded horizon as the v1 contract. Interim: `/trades` floors at the uncompressed horizon and its `note` says so. The build is its own slice with a plan review (PRE-CUT 1752-1760). |
| RWA basis | See INV-1036 and the RWA section in [rwa-coverage-reconciliation.md](../methodology/rwa-coverage-reconciliation.md). |

## Cut: cited rows

Code, workflows and docs cite these ids. Full text is at the pre-cut sha (old line in brackets).

| Id | Gist | Cited by |
|---|---|---|
| 1.4 [81] | One live SEV drill and one rollback rehearsal against wired paging; not yet run (INV-0841) | postmortems/2026-09-16-r1-pg-wal-fills-root.md:61,75 |
| 1.7 [84] | Test nets behind r1: CLOSED 2026-09-16. Both served v0.85.0 on every binary the region runs. `stellarindex-aggregator` is deliberately not deployed there (`scripts/dev/region-binaries.tsv`). The fleet-release-drift tripwire means something only when test nets deploy via `deploy.yml` | ci.yml:1490, deploy.yml:1589, fleet-release-drift.yml:3, check-fleet-release-drift.sh:3 (test :14, :285) |
| 1.9 [86] | D7 thin-pool third-alias VWAP review DONE 2026-09-04; `/v1/price/tip` merge walk fixed (`tipMergePairs`). Artefact: [d7 review](../methodology/d7-thin-pool-third-alias-vwap-review-2026-09-04.md) | d7 review |
| 1.12 [89] | ClickHouse schema snapshot has an off-site target (resolved 2026-09-15; `SNAPSHOT_MC_TARGET=offsite/stellarindex-pgbackrest-r1/ch-schema`, 8 objects verified) | 18-pgbackrest-backup.yml (4 lines), ch-schema-restore.md, audit-remediation-operator-actions.md:81 |
| 1.14 [91] | Single-bar `/v1/ohlc`, `/v1/vwap`, `/v1/twap` read the SAC forms of USD-pegged constituents; gate per bucket at 1m (DONE 2026-09-05, with 1.15) | internal/api/v1 (coverage_floor, ohlc_fiat_combine, fiat_series_sac_reach_test, market_dedupe_internal_test) |
| 1.15 [92] | Fiat-quoted `/v1/ohlc` series reaches SAC-quoted Soroban pools. r1 measure: 43 counterparties with SAC-only USD depth, 260,833 prints, $14,630,761.46 (DONE 2026-09-05) | same files; timescale pair_direction_guard and trades_direction tests |
| 1.16 [93] | Both stored orientations of a market are read; `1fca7bceb` (v0.61.0) made `Store.TradesInRange` two-armed. Verified 2026-09-16: 7,256 forward + 1,173 reverse = 8,429 served `trade_count` | same files |
| W1.1 [1107] | `/v1/status` incidents: a failed Prometheus query must not serialise as zero counts; `87e5b1aa` (#73) | internal/api/v1/status.go:63 |
| W3.1, W3.2 [1191, 1200] | Contract pages cold: 23/25 breaching to 6/25. W3.2 page-type audit harness is `scripts/ops/contract-page-audit.py --type` | scripts/ops/contract-page-audit.py:19 |
| W5.3 [1425] | Pre-07-23 usd-volume restamp verified a NO-OP 2026-08-30; tool `usd-volume-restamp` (#251). The #372 restamp ran 2026-09-06 (26,231,575 rows, $30,601,931.62) | usd_volume_restamp.go:24, usd_volume_restamp_test.go:16 |
| W5.4 [1437] | Reset of the 13 supply rollups: `supply verify-rollup` on r1 verified clean 2026-09-17 (0 drift, tolerance 0); no reset needed | n/a |
| W8-12 [1738] | LP reserves are live-only from ledger 63,300,828; ACCEPTED 2026-10-02, no backfill ([supply-pipeline.md](../architecture/supply-pipeline.md#lp-reserve-history-cutoff)) | open-fixes-inventory-2026-08-08.md:23 |
| W8-17, W8-20 [1786, 358] | `/v1/ohlc` 500 at 2h/12h/3d/2w; one interval ladder (`AllHistoryGranularities`) now drives validation, routing and the fold allow-list. CLOSED | aggregates.go:2381, ohlc_routes.go:44, ohlc_routes_test.go:21, ohlc_intervals_test.go:20-21, test/integration/ohlc_fold_intervals_test.go:59 |
| W8-19 [1807] | A single `refresh_continuous_aggregate` call needs a timeout bound: `CAGGRefreshTimeout` 5 min per hour of window, floor 10 min, ceiling 4 h | cagg_refresh_timeout.go:16 |
| L4.14, L4.15 [4738] | R2/R3 are not provisioned, so `flags.reduced_redundancy` has no producer (ADR-0017); carried to W9, gated on D2 (INV-2710) | runbooks/verify-archive.md:131 |
| CS-102 [2636] | Quiet assets anchor on the observer watermark; see [supply.md](runbooks/supply.md#cs-102-quiet-is-not-stale) | runbooks/supply.md (CS-102 section) |
| D1, D5, DR gate | See "Cut: decisions of record". The DR gate half "restore-drill timer re-enabled" is INV-0840 | freeze-lifecycle.yml:52 (both copies), retention_policy_test.go:644, 18-pgbackrest-backup.yml:698 |
| k6 loop entry 2026-07-30 ~14:40Z [2176] | k6-weekly failed silently after the org migration dropped its secrets; load key restored. The suite's prod-host guard refuses `api.stellarindex.io`, so AC2 evidence (p95 54.4 ms) was captured on r1 against localhost. Orphan key and dirs: INV-2693 | .github/workflows/k6-weekly.yml:149 |

## Cut: moved evidence

| Lesson | Now lives in | Old lines |
|---|---|---|
| No unbounded trade-scan queries; `XX000` vs `57014`; argMax-tuple; #475 EPIPE under pipefail; PHO +157% seed cause | [domain-traps.md](../architecture/domain-traps.md#unbounded-trade-scans-cancelled-refreshes-and-aggregation-pitfalls) | 3279, 1834, 4100, 728, 3440 |
| Agent load contaminates p99 (48.6 ms to 566.2 ms); INV-0675 | [engineering-standards.md](../engineering-standards.md#do-not-measure-production-latency-from-a-box-your-own-agents-are-working) | 25-63 |
| sep41 zero-writer hole | [ingest-pipeline.md](../architecture/ingest-pipeline.md#the-sep41-zero-writer-hole-2026-07-13-to-2026-07-27) | 3822 |
| Restore-drill `PrivateTmp` / `NoNewPrivileges` blockers | [restore-drill.md](runbooks/restore-drill.md#why-the-scheduled-drill-never-ran-until-2026-09) | 1270 |
| CS-102 | [supply.md](runbooks/supply.md#cs-102-quiet-is-not-stale) | 2636, 2823 |
| CCTP projection started after first event | [cctp.md](../protocols/cctp.md#projected-history-started-after-the-contracts-first-event-found-2026-07-30) | 2142 |
| RWA coverage evidence ($2,535,764,187.91, 29 assets, 18 issuers; real estate 13 of 15 scam-flagged; money-fund issuer's other 3 tokens $82.0M) | [rwa-coverage-reconciliation.md](../methodology/rwa-coverage-reconciliation.md) | 95-224 |
| Export-429 backoff design (own 8-wait budget, honour `Retry-After` capped 60 s, else exponential to 30 s with jitter; `3422b150`), #336 impersonator oracle-row root cause (gated on the verified catalogue), D3 rebuild recipe (`scripts/ops/d3-lecur-v2-rebuild.sh`), Galexie stale-binary near-miss | Pre-cut sha only; no better home | 3945, 736, 4017-4100, 3804 |
| A live projector holds its cursor in memory: a SQL cursor fast-forward needs stop, UPDATE, start, or the row is clobbered | Pre-cut sha only; no better home | 2715-2717 |
| 400-VU mixed stress lands p95 ~5 s / 4.4% failures across two runs: the single-box ceiling beyond rated load (HA capacity datum, INV-1101) | Pre-cut sha only; no better home | 1947-1951 |
| Soroban TTL join recipe: `SHA256(base64Decode(cd.key_xdr)) = substring(base64Decode(ttl.key_xdr),5,32)`, `live_until = reinterpretAsUInt32(reverse(substring(base64Decode(ttl.entry_xdr),41,4)))`; as of 2026-07-28 `ledger_entries_current` served archived contract_data to every current-state reader (AQUA SAC kept 1,663 of 2,420 entries under the TTL filter); readers now resolve liveness via `internal/storage/clickhouse/ttl_liveness.go` | Pre-cut sha only; no better home | 2914-2954, 3398-3439 |
| Ordinal re-derive: `-parallel 4` with default `-flush-every 500` OOM-killed at the 20 G cap in 22 s; `-parallel 3 -flush-every 100` held 6.7 GB; `ch-backfill` has no resume so run ~110k-ledger chunks (`scripts/ops/ordinal-rederive-chunks.sh`); `d2-ordinal-reproject.sh` is retired and refuses to run (REPLACE PARTITION dropped rows ingested after the snapshot) | Measurements also in the `ordinal-rederive-chunks.sh` header | 3652-3669 |
| As of 2026-07-28 the `run-heavy-job.sh` flock was per-job-name, so a scheduled timer (`archive-completeness`) ran beside a manual heavy job (two scopes reserved 40 G of 188 G); fixed by a68a34e52 (host-wide heavy-job lock, `run-heavy-job-test.sh` case 12) | Pre-cut sha only; no better home | 3750-3760 |
| Launch step 1 (pre-cut §2.8): run `stellarindex-migrate up` and confirm `schema_migrations.version` equals the highest file under `migrations/`, not dirty, before tagging and swapping the binary. Deliberately version-agnostic: the head number lives only in `migrations/` and `v1.ExpectedSchemaVersion` (CI test `TestExpectedSchemaVersionMatchesMigrationsHead`). `/readyz` fail-closes (503 drain) only when applied < expected, a floor not `==`; `deploy-binary.yml` already runs `migrate up` before any swap | Pre-cut sha only; no better home | 4610-4635 |
| Claimable-balance seed dry-run, 2026-07-27 (3 h 50 min): 3,605,321 live claimable balances across 30,748 classic assets, peak ~12.4 GB inside the 20 GB heavy-job cap, so every classic asset with pre-63.3M claimable balances was understated, not only AQUA (86.70B served vs 99.92B Horizon, -13.2%; all other components reconciled to +0.61%). Airdrop-era ledgers (about 40.48M) mint millions of balances in a few thousand ledgers, so the window floor is 256 and the width recovers after sustained success (`9226f324`). Seeded as `supply seed-claimable-balances` (`120bf7c3`); AQUA reconcile 8/8 PASS 2026-07-30 | Pre-cut sha only; no better home | 4380-4492 |
| `galexie-soak-check` gate executed 2026-07-28 19:17Z (10 PASS / 0 FAIL): `data/minio@pre-trim-2026-07-26` destroyed, timer removed. Cold-tier rollback is rehydrate-only from then on | Pre-cut sha only; no better home | 4509-4543 |
| Config apply with `--tags` can fire a restart handler that `--check` does not show: 2026-08-29 06:04Z `--tags users,minio,galexie` restarted galexie (about 11 min export delay, no data loss); fixed by #307 (restart only on effective input change, fail-closed probe, `force_handlers`). Tag-limited runs need `-e ansible_python_interpreter=/usr/bin/python3` | Pre-cut sha only; no better home | 4772-4773 |
| Testnet galexie backfill auto-restarted by systemd re-ran from `--start 2` after a `MemoryMax=5G` OOM (2026-08-29, ledger 2,254,083): skip-existing-files is not skip-existing-work. Relaunched from 2,254,080 with `MemoryMax=8G` and `EnvironmentFile=/etc/default/galexie-backfill`. The resume-point wrapper and counter-going-backwards alert were follow-ups never filed | Pre-cut sha only; no better home | 4776-4777 |
| W5.8 (INV-1245): before any TRUNCATE or DROP of Postgres `soroban_events`, re-grep every live reader: `StreamSorobanEvents\|FirstSorobanEventLedger\|MaxSorobanEventLedger\|FindSorobanEventsLedgerGaps\|DistinctSorobanTopicSamples\|ReDeriveOutputCountsByKind\(`. A TRUNCATE leaves the table, so readers see empty as "nothing happened" instead of failing. `StreamSorobanEvents` is still referenced in `internal/projector/projector.go` (5 matches at HEAD, call at :1314) | Pre-cut sha only; no better home | 1446-1460 |
