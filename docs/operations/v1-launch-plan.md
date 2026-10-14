---
title: v1 launch plan — outstanding work
last_verified: 2026-10-06
status: active
severity: P1
---

# Stellar Index — v1 launch plan (outstanding work only)

> The one launch plan: the go-live gate (§0), the launch sequence (§1), open
> and deferred work, decisions of record, and the rows code and docs cite. The
> inventory (`inventory/items.jsonl`, private tree) is the authority for every
> item. Closed work and old evidence were cut under INV-0897 and stay at
> `git show 52aacb972a5be5fe65e9d608227e8fe06dfe7fe2:docs/operations/v1-launch-plan.md`
> (PRE-CUT below). Companions: `production-confidence-campaign-2026-07-23.md`
> (proof harness) and the gitignored `production-remediation-ledger-2026-07-23.md`
> (finding-status authority). Runbooks under `runbooks/` are the recipes.

## What "v1 launch" means

Public availability of the API + explorer as a production service: certified-complete substrate, proven money values, a signed repeatable deploy path, honest capacity/HA posture, paging wired to a human, and the frozen v1.0 wire shape (ADR-0042).

## 0. Go-live gate: verified current state, 2026-10-06

Rows index live inventory items (snapshot `323689bf`); the item is the authority. A row goes when its last item closes.

| Gate | Items (status) | State |
|---|---|---|
| Launch cutover | INV-1201 (blocked), INV-0873 (open), INV-0839 (blocked, Ash) | Waits on Ash's GO, a launch date and CDN provisioning; announcement copy and first-24h watch need Ash; accepted-risk sign-off deferred |
| Security review | INV-0871 (blocked, Ash), INV-0696 (blocked) | External review deferred to after launch; the private report's child findings are open |
| Credential rotation | INV-0744, INV-0919 (Ash), INV-1178 (Ash), INV-1479, INV-1516, INV-2091 (all blocked) | One batch after inventory closure; INV-1479 also waits on the verified B2 backup |
| r1 security defects | INV-0802 (in-progress), INV-1184 (blocked) | ClickHouse `default` user unauthenticated (ops users in #2380, operator steps remain); pgBackRest repo1 is `cipher-type=none`, live re-encrypt pending |
| Deploy approval gate | INV-0695 (blocked) | `DEPLOY_APPROVAL_RELAXED` is still set; deleting it is an r1 step |
| Paging + SEV drill | INV-0841 (blocked), INV-2615 (open) | Drill waits on inventory closure and 0 r1 alerts; Healthchecks check `stellarindex-futurenet-indexer` is down |
| r1 alerts firing | INV-2548 (open), INV-2675 (open) | `stellarindex_zfs_pool_fill_90pct_within_7d`; `stellarindex_verify_archive_run_stale` |
| Capacity + backup | INV-2178 (blocked), INV-2524 (open), INV-0986 (blocked) | r1 pool reaches 85% around 2026-11-07 (0.61 TiB/month); first full CH lake backup to B2 failed after 16 h on a 14 GiB limit; off-site secrets backup waits on Ash's age/gpg choice |
| Config drift | INV-1911 (blocked), INV-0798 (blocked) | Scheduled `ansible-drift` is red; needs `redis_password` in the r1 vault |
| Lake entry ordering | INV-2567 (open), INV-1313 (blocked) | `ledger_entry_changes` rows from before `c4ab63e45` carry `intra_ledger_seq=0`; reproject waits on the Go-walk re-derive |
| Eviction + restores | INV-2532 (in-progress), INV-2157 (open), INV-2159 (blocked), INV-2533 (open) | Lake writes persistent evictions as removed; restored entries for ledgers 58,762,517–64,673,495 need re-extract; lumen conservation residual +119,100,885,352 stroops at ledger 64,767,268 |
| Explorer performance | INV-0596 (blocked), INV-0675 (in-progress) | Sub-second cold contract pages and the p95/p99 target both wait on a clean-window r1 re-measure |
| Oracle data | INV-2590 (in-progress) | RedStone Invert feeds store the reciprocal while `price_raw` claims the raw value; fix merged (#2420), waits on deploy plus redstone projected-rebuild on r1 |
| Catalogue pricing | INV-0202 (blocked, critical) | The XLM→USD catalogue anchor is keyed `base='native'` with a dead `fiat:USD` arm; needs a plan and adversarial review |
| Dashboard auth | INV-0768 (open, high; Ash disclosure decision) | Unauthenticated dashboard sign-in lockout; disclosure sequencing is Ash's call |
| SEV comms | INV-0145 (blocked, high, Ash) | Every SEV comms path depends on the live API; needs an out-of-band channel |

## 1. Launch sequence and gate rationale

Done: supply reconcile 8/8 ([evidence](https://github.com/Stellar-Index/StellarIndex/blob/0023bb9aefa96fb8231d9eabd160e6133eca39e9/docs/operations/evidence/2026-07-30-supply-reconcile-8of8.md)); completeness green on all 20 sources ([evidence](https://github.com/Stellar-Index/StellarIndex/blob/0023bb9aefa96fb8231d9eabd160e6133eca39e9/docs/operations/evidence/2026-08-01-completeness-17of17.md)); prove-it battery; config codified equals live; `auth_mode=apikey_optional`; `/sla` published.

Still open, in order.

1. Migrations: run `stellarindex-migrate up` and confirm `schema_migrations.version` equals the highest file under `migrations/` and is not dirty, before tagging and swapping the binary (see "Cut: moved evidence").
2. `sip_` SLA-probe smoke and outside-internet `make smoke` 13/13 (INV-2707).
3. Refresh `post-launch-queries.md`, the Grafana launch-watch board and the F-0100 PromQL check (INV-2708).
4. Cutover on Ash's GO; first-24h watch staffed; every dashboard user re-logs in after migration 0143 (INV-2709, INV-0873).

---

# Cut work: forwarding sections

Live items, code and docs forward here. Old `:NNN` line cites and `#L<n>` inventory pointers resolve at PRE-CUT (`52aacb972a5be5fe65e9d608227e8fe06dfe7fe2`), not HEAD; commit ids older than the 2026 history rewrite may not resolve (find the successor with `git log`).

## Cut: header

- Migrations 0051, 0106 and 0107 cite `:2486`: that means the "no unbounded trade-scan queries" rule, now in [domain-traps.md](../architecture/domain-traps.md#unbounded-trade-scans-cancelled-refreshes-and-aggregation-pitfalls).
- Old row ids (1.x, W-x, D-x) cited by code resolve in "Cut: cited rows" and "Cut: decisions of record".
- Go-live gate is §0; the launch sequence is §1 (pre-cut §2.8, PRE-CUT lines 4610-4636) plus INV-2707, INV-2708 and INV-2709 below.

## Cut: open work not in §0

One line per item; the INV item is the authority.

| INV | Item |
|---|---|
| INV-2690 | W8-15 CI/test gap residue |
| INV-2691 | W8-16 CS-068: late trades never re-materialise in `prices_1m` |
| INV-2692 | `sep41_supply_events` never vacuumed (42/130 chunks, 276M rows) |
| INV-2693 | Orphan k6 API key and ephemeral k6 dirs on r1 |
| INV-2694 | Pin `stellar_archivist_version` for `stellar_stack_lagging` |
| INV-2695 | Pending r1 applies: CH drop-guard #286, `ops_batch`, ZFS snapshots |
| INV-2696 | Ops-CLI write-gate unification (~15 money-path defaults) |
| INV-2697 | Failed classic op still lands a public participant row |
| INV-2698 | W5.7 CEX dust DELETE (destructive, last) |
| INV-2699 | Update blog/methodology copy: per-token oracle layer shipped |
| INV-2700 | Top-50 prices check (CoinGecko key) |
| INV-2701 | `verify-usd-volume` estimated-tier alert at about $10M/day |
| INV-2702 | migrations-sync step may re-orphan the migrations dir uid |
| INV-2703 | Runbook lore: unattended-upgrades libc6 bounces Postgres ~06:24, kills heavy jobs |
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

- HA, R2/R3 and ClickHouse HA (INV-0890, INV-1048, INV-1070, INV-1101, INV-1389). D2 accepts a single box per region with a tested restore; multi-region (ADR-0050) is post-v1 (`docs/architecture/ha-plan.md` §10).
- R1 is NOT hardware-upgradeable; never propose drives.
- INV-2851: oracle-reconcile window netting (F6 = C2-16).
- INV-2852: oracle-reconcile fail-opens (F8-F10).
- INV-2853: caller-side supply close-timestamp fix (M4).
- INV-2854: `/assets/native` advertises 5,553 markets but lists 100.
- INV-2855: API auth and rate-limit review.
- INV-2844: SEP-1 `stellar.toml` is not verified against its `SIGNING_KEY`; metadata is served as the domain claims it (`internal/metadata/doc.go`).
- INV-2856: dependency-advisory review cadence; cadence is in [SECURITY.md](../../SECURITY.md#dependency-advisories), standing review stays post-v1.
- INV-2803: L7.3 cross-oracle divergence production coverage.
- INV-2804: L7.6 `usd_volume` for pure-SEP-41 stables (currently an empty set); see [usd-volume-coverage-plan.md](usd-volume-coverage-plan.md#pure-soroban-sep-41-tokens-no-usd-pegged-quote-at-all).
- INV-2857, INV-2858: operator decisions on peg-set thresholds and served-tier retention (Ash).
- INV-2859: instrument the Redis read-through caches (`cache_ops_total` covers only in-memory caches).
- INV-2860: projector-replay to the served tier to close the sep41 gap (register D4).
- INV-2861: HA blockers F-001..F-010 (HA itself stays post-v1, above).
- W9: DeFindex vault registry and unproven emitters (INV-0923), residual DeFi decoders (INV-0894), L4.14-L4.17 and L5.8 (INV-2710).
- CH Phase 8 `soroban_events` decommission (#803): destructive, last.
- Credential rotation is one batch after inventory closure. The MinIO root exposure is closed (root is `stellarindex-admin`, old key rejected); moving services off root remains hygiene.
- Intra-ledger-seq historical backfill: INV-1023 (blocked; its `blocked_on` names this plan).

## Cut: decisions of record

Condensed from old lines 1545-1570; full rows at the pre-cut sha.

| # | Decision |
|---|---|
| D1 | RATIFIED. Anomaly-freeze is fixed by composite-reference corroboration (#288), not by editing the alert. Verify with `increase(stellarindex_anomaly_freeze_engaged_total[24h])` plus `stellarindex_aggregator_composite_freeze_suppressed_total > 0` (proves the mechanism engaged, not a calm market). |
| D2 | ACCEPTED-RISK plus a tested restore at v1. Single box per region; multi-region (ADR-0050) deferred post-v1. |
| D3 | SIGNED OFF. ClickHouse posture is ADR-0043 §2.1 schema-and-state snapshot plus re-derive, plus rolling ZFS snapshots. Do NOT resurrect full-lake copies. |
| D4 | BUILD ALL THREE: order-book depth (#337), DEX TVL (#338), per-token oracle pages (#336). Overrides any "[DECIDE build-or-drop]" text. |
| D5 | ACCEPTED. The retention contract is "we retain everything we index", not a set of windows. `trades` holds 2018-07-01 to now. Migration 0031 removed retention from `trades`, `prices_1m`, `prices_15m`; 0040 from `oracle_updates`. The authoritative list of retention policies is `TestRetentionPolicies_AreExactlyTheDeclaredSet`; Go-side age deletes are in `TestGoAgePruners_AreExactlyTheDeclaredSet`; `mev_events` is retained. Coverage limits: on-chain SDEX trades begin 2026-03-12 (#349); CEX series begin 2018-07-01 (Kraken) and 2026-05-05 (Binance, Coinbase, Bitstamp). |
| D6 | ACCEPTED as documented-unfillable: genesis edge [2 to 287,404]; recover via op-replay if ever needed. |
| D7 | Not a decision but owed work: the third-alias thin-pool VWAP review. DONE, see row 1.9. |
| D8 | FIX FOR v1: `*_FUNDAMENTAL` RedStone feeds publish a NAV ratio in BTC but were registered `quote=fiat:USD`. Contained (`IncludeInVWAP=false`); fixed by 961cc8a97 (#348). |
| D9 | DROPPED. Stripe C3-081 reconcile closed per ADR-0049. |
| D10 | Privacy review reduced to a sign-off; terms and privacy are live (INV-0150). Erasure and export were built (GH #809, `internal/accounterasure`, migration 0188); procedures in `runbooks/account-erasure.md`; identity checks and the other rights requests in `runbooks/privacy-rights-requests.md`. |
| DR | SIGNED OFF. Off-site posture: pgBackRest repo2 plus rolling ZFS snapshots. Superseded by the B2-only off-site decision (INV-1475, INV-1181 done). |
| W6.1 | CLOSED. Paging wired and proven: Discord (pages and alerts) and a Healthchecks.io dead-man. Caveat: `pre-launch-check.sh` does not check that an alertmanager receiver holds a real URL. |
| W8-13 | Decided: `stellar.trades_by_account` in ClickHouse for per-account trade history (INV-0902 tracks the open storage design). NOT built. Rejected: taker/maker in `trades`' `compress_segmentby`, and the bounded horizon as the v1 contract. Interim: `/trades` floors at the uncompressed horizon and its `note` says so. PRE-CUT 1752-1760. |
| RWA basis | See INV-1036 and the RWA section in [rwa-coverage-reconciliation.md](../methodology/rwa-coverage-reconciliation.md). |

## Cut: cited rows

Code, workflows and docs cite these ids (grep the id for citers). Full text is at the pre-cut sha (old line in brackets).

| Id | Gist |
|---|---|
| 1.4 [81] | One live SEV drill and one rollback rehearsal against wired paging; not yet run (INV-0841) |
| 1.7 [84] | Test nets behind r1: CLOSED. Both serve every binary the region runs; `stellarindex-aggregator` is deliberately not deployed there (`scripts/dev/region-binaries.tsv`). |
| 1.9 [86] | D7 thin-pool third-alias VWAP review DONE. Artefact: [d7 review](../methodology/d7-thin-pool-third-alias-vwap-review-2026-09-04.md) |
| 1.12 [89] | ClickHouse schema snapshot has an off-site target (`SNAPSHOT_MC_TARGET=offsite/stellarindex-pgbackrest-r1/ch-schema`) |
| 1.14 [91] | Single-bar `/v1/ohlc`, `/v1/vwap`, `/v1/twap` read the SAC forms of USD-pegged constituents; gate per bucket at 1m (DONE 2026-09-05, with 1.15) |
| 1.15 [92] | Fiat-quoted `/v1/ohlc` series reaches SAC-quoted Soroban pools. (DONE 2026-09-05) |
| 1.16 [93] | Both stored orientations of a market are read; `1fca7bceb` (v0.61.0) made `Store.TradesInRange` two-armed. |
| W1.1 [1107] | `/v1/status` incidents: a failed Prometheus query must not serialise as zero counts; `87e5b1aa` (#73) |
| W3.1, W3.2 [1191, 1200] | Contract pages cold. W3.2 page-type audit harness is `scripts/ops/contract-page-audit.py --type` |
| W5.3 [1425] | Pre-07-23 usd-volume restamp verified a no-op; tool `usd-volume-restamp` (#251). The #372 restamp has run (26,231,575 rows) |
| W5.4 [1437] | Reset of the 13 supply rollups: `supply verify-rollup` on r1 verified clean (0 drift); no reset needed |
| W8-12 [1738] | LP reserves are live-only from ledger 63,300,828; ACCEPTED, no backfill ([supply-pipeline.md](../architecture/supply-pipeline.md#lp-reserve-history-cutoff)) |
| W8-17, W8-20 [1786, 358] | `/v1/ohlc` 500 at 2h/12h/3d/2w; one interval ladder (`AllHistoryGranularities`) now drives validation, routing and fold allow-list. CLOSED |
| W8-19 [1807] | A single `refresh_continuous_aggregate` call needs a timeout: `CAGGRefreshTimeout` 5 min per hour of window, floor 10 min, ceiling 4 h |
| L4.14, L4.15 [4738] | R2/R3 are not provisioned, so `flags.reduced_redundancy` has no producer (ADR-0017); carried to W9, gated on D2 (INV-2710) |
| CS-102 [2636] | Quiet assets anchor on the observer watermark; see [supply.md](runbooks/supply.md#cs-102-quiet-is-not-stale) |
| D1, D5, DR gate | See "Cut: decisions of record". The DR gate half "restore-drill timer re-enabled" is INV-0840 |
| k6 loop entry 2026-07-30 ~14:40Z [2176] | k6-weekly failed silently after its secrets were dropped; the suite's prod-host guard refuses `api.stellarindex.io`, so AC2 evidence ran on r1 against localhost. Orphan key and dirs: INV-2693 |

## Cut: moved evidence

| Lesson | Now lives in | Old lines |
|---|---|---|
| No unbounded trade-scan queries and related pitfalls | [domain-traps.md](../architecture/domain-traps.md#unbounded-trade-scans-cancelled-refreshes-and-aggregation-pitfalls) | 3279, 1834, 4100, 728, 3440 |
| Agent load contaminates p99; INV-0675 | [engineering-standards.md](../engineering-standards.md#do-not-measure-production-latency-from-a-box-your-own-agents-are-working) | 25-63 |
| sep41 zero-writer hole | [ingest-pipeline.md](../architecture/ingest-pipeline.md#the-sep41-zero-writer-hole-2026-07-13-to-2026-07-27) | 3822 |
| Restore-drill `PrivateTmp` / `NoNewPrivileges` blockers | [restore-drill.md](runbooks/restore-drill.md#why-the-scheduled-drill-never-ran-until-2026-09) | 1270 |
| CS-102 | [supply.md](runbooks/supply.md#cs-102-quiet-is-not-stale) | 2636, 2823 |
| CCTP projection started after first event | [cctp.md](../protocols/cctp.md#projected-history-started-after-the-contracts-first-event-found-2026-07-30) | 2142 |
| RWA coverage evidence | [rwa-coverage-reconciliation.md](../methodology/rwa-coverage-reconciliation.md) | 95-224 |
| Export-429 backoff (`3422b150`), #336 impersonator oracle-row root cause, D3 rebuild recipe (`scripts/ops/d3-lecur-v2-rebuild.sh`), Galexie stale-binary near-miss | PRE-CUT only | 3945, 736, 4017-4100, 3804 |
| A live projector holds its cursor in memory: SQL cursor fast-forward needs stop, UPDATE, start | PRE-CUT only | 2715-2717 |
| 400-VU mixed stress: p95 ~5 s / 4.4% failures, the single-box ceiling (INV-1101) | PRE-CUT only | 1947-1951 |
| Soroban TTL join (`internal/storage/clickhouse/ttl_liveness.go`) | PRE-CUT only | 2914-2954, 3398-3439 |
| Ordinal re-derive: `-parallel 3 -flush-every 100`, ~110k-ledger chunks (`scripts/ops/ordinal-rederive-chunks.sh`); `ch-backfill` has no resume | script header | 3652-3669 |
| `run-heavy-job.sh` flock is host-wide | PRE-CUT only | 3750-3760 |
| Launch step 1 (pre-cut §2.8): `stellarindex-migrate up`, confirm `schema_migrations.version` equals the highest file under `migrations/` and is not dirty, before tagging and swapping the binary | PRE-CUT only | 4610-4635 |
| Claimable-balance seed (`supply seed-claimable-balances`): peak ~12.4 GB inside the 20 GB cap | PRE-CUT only | 4380-4492 |
| Galexie soak gate, `data/minio@pre-trim-2026-07-26` destroyed; cold-tier rollback is rehydrate-only | PRE-CUT only | 4509-4543 |
| Tag-limited ansible runs: a restart handler can fire that `--check` hides (#307); pass `-e ansible_python_interpreter=/usr/bin/python3` | PRE-CUT only | 4772-4773 |
| A systemd-restarted galexie backfill re-ran from `--start 2` after an OOM: relaunch from the last ledger with `MemoryMax=8G` and `EnvironmentFile=/etc/default/galexie-backfill` | PRE-CUT only | 4776-4777 |
| W5.8 (INV-1245): before any TRUNCATE or DROP of Postgres `soroban_events`, re-grep every live reader (`StreamSorobanEvents` is still used by `internal/projector/projector.go`) | PRE-CUT only | 1446-1460 |
