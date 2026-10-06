---
title: v1 launch plan — THE single source of truth
last_verified: 2026-09-03
status: active
severity: P1
---

# Stellar Index — v1 launch plan (single source of truth)

> **⭐ THIS IS THE ONE PLAN.** Consolidated 2026-07-27 from every prior
> launch/production document, with every carried item **re-verified against
> live r1 + the repo on 2026-07-27** (not copied on trust). If you are
> resuming: read §0 (verified state), then work the items in the plan below (the old §2 is pre-cut: [PRE-CUT 3956-4797](https://github.com/Stellar-Index/StellarIndex/blob/52aacb972a5be5fe65e9d608227e8fe06dfe7fe2/docs/operations/v1-launch-plan.md#L3956-L4797)).
>
> Superseded by this doc (banners added; keep for history/recipes only):
> the 2026-07-18 production-readiness master plan (the campaign log, since removed),
> `production-readiness-remaining.md`, `docs/audit/audit-2026-07-16/go-live-master-plan.md`,
> `launch-todo.md`, `launch-day-checklist.md`, `public-flip.md`,
> `notes/ROADMAP.md`, `notes/BACKLOG.md`.
> Still ACTIVE as companions: `production-confidence-campaign-2026-07-23.md`
> (the adversarial proof harness — its E-gate was pre-cut §2.6) and the
> gitignored `production-remediation-ledger-2026-07-23.md` (finding-status
> authority). Runbooks under `runbooks/` remain the execution recipes.

## THE PLAN — refreshed 2026-09-03 (supersedes every section below it, except §0 where they disagree)

> **RE-VERIFIED 2026-09-08 against live r1 and repo HEAD.** Rows corrected in that pass: **1.7**
> (test nets were NOT behind — all three hosts level at v0.63.0/schema 155) and the retention claims
> in the since-deleted `slos-and-guarantees.md` (now summarised in `docs/architecture/coverage-matrix.md`, S6.1/S7.2; it then asserted `prices_1d` held native pairs back to
> 2015-11-18 with 6.3M rows; SDEX data begins **2026-03-12** and the whole view is 4.33M rows).
> Confirmed still open: **1.2** (no signed accepted-risk register exists), **1.5** (all five
> `HEALTHCHECKS_URL_*` present but EMPTY), **1.8** (`DEPLOY_APPROVAL_RELAXED=true`, set 2026-07-26),
> **1.12** (`ch_schema_snapshot_mc_target` unset in every env file). **#372's re-stamp RAN on
> 2026-09-06** and committed 26,231,575 rows / $30,601,931.62 — it had been reported as blocked.
>
> **Read this and nothing else, unless you need a recipe.** Every row was
> re-derived against HEAD and live r1 on 2026-09-03 by an audit whose whole
> job was to disbelieve this document. Where anything below disagrees with
> this section, this section is right.
>
> **Commit ids in older entries may not resolve.** This repository's history
> was rewritten once (content preserved, every commit id changed), and entries
> written before that still cite the old ids. An id `git cat-file -e` rejects
> is pre-rewrite: find its successor by date and subject with `git log`.
>
> The audit's headline was that **three facts dominating the launch answer
> appeared nowhere in this plan**: `main` was red, the public status page was
> reading `degraded`, and `/v1/coverage` publicly serves 20 of 21 rather than
> the 17/17 the go-live gate still claimed. A plan that tracks workstreams but
> not "is main green" and not "what does a customer see first" is measuring
> the wrong things.
>
> **One correction to the audit itself, and it is the more useful lesson.**
> Its `degraded` finding was real at the moment it sampled and WRONG as a
> conclusion: it measured while ten of our own subagents were saturating r1
> with cold scans. A read-only agent wave moved the API's 6-hour p99 from
> **48.6 ms to 566.2 ms**, which reads exactly like a chronic regression and
> is not one. **Do not measure production latency from a box your own agents
> are working.** The genuine SLA-contract defect is a different one and is
> now #487: the public page promises 99.9 % availability while ADR-0008 and
> the wired burn-rate budget both operationalise 99.99 % — a ten-fold
> disagreement on a commercial commitment, where nobody can say which number
> governs a customer's breach claim.

### Tier 0 — cannot announce 1.0 without these

| # | item | owner | state on 2026-09-03 |
|---|---|---|---|
| 0.1 | **`main` must be green** | agent | **DONE** (`6ce95d191`). It had been red since `e68f8eaa0`: #331 F1 moved the listing's price derivation into a worker-maintained rollup and two integration tests still refreshed only the old continuous aggregate, so every asset came back unpriced. An all-unpriced board COLLAPSES rank tier 0 into tier 1, which is why the visible symptom was a wrong sort order rather than a missing price. `make verify` cannot see this class — it does not run integration tests. |
| 0.2 | **`/terms` + `/privacy`** | **owner — legal read only** | **BLOCKED ON THE OWNER.** Both URLs 404 today. PR #237 has the code and the tests; it needs wording signed off, nothing else. |
| 0.3 | ~~Stop the status page saying `degraded`~~ | — | **NOT A BLOCKER — the measurement was contaminated, and the contamination was ours.** The audit sampled `/v1/status` and a 1-hour Prometheus window while TEN subagents were running cold ClickHouse and Postgres scans against r1. Two 6-hour windows from r1's own Prometheus settle it: ending **2026-09-02T20:00Z, before that load, p99 = 48.6 ms and p95 = 20.5 ms**; ending 2026-09-03T07:00Z, during it, p99 = 566.2 ms and p95 = 82.1 ms. Targets are 500 ms and 200 ms, so the steady state sits inside both by an order of magnitude. With the agents drained, live `/v1/status` reads `overall: ok`, p50 1 / p95 21 / p99 34 ms, zero active incidents. The per-route figures the audit quoted (`/v1/pairs` 4,975 ms, `/v1/accounts/{g}/operations` 4,966 ms, `/v1/pools` 4,700 ms) are load artefacts. **Neither of the audit's two options — 2-4 days of optimisation, or renegotiating the published target — is needed.** Two of those three routes were independently fixed anyway (`dea56efec` for `/v1/accounts/{g}/operations`, `12590a65a` for `/v1/pools`); `/v1/pairs` was re-measured cleanly on 2026-09-30 03:20Z on r1 against the local API (`curl -w '%{time_total}'`, 3 samples per variant, load 9/20 cores, no heavy jobs, no agent load): hot XLM/USDC 38–82 ms (alias fan-out), cold first hit sUSD/XRP 22 ms and AFR/USDC 38 ms, repeats 4–9 ms — the audit's 4,975 ms was a load artefact; cold-variant cost is tens of ms and needs no fix (INV-0837). |
| 0.4 | **Email/DNS perimeter (#334)** | agent + **2 clicks from the maintainer** | **RECORDS LIVE** (`7b914f351`). MX, SPF (`-all`), DMARC (`p=quarantine`), a second DKIM selector and CAA are published and verified against the authoritative nameservers, with a drift check (`scripts/ops/dns-perimeter-check.sh`) and a weekly workflow. Two steps need the maintainer: click Cloudflare's destination-verification link so `security@` can forward, and publish the DS record at the registrar. Both are on #334. |

### Tier 1 — do before announcing; cheap; does not strictly block

| # | item | owner |
|---|---|---|
| 1.1 | **#478** — decide what `oracle_stale` should MEAN on a change-driven oracle. It is the only non-heartbeat alert firing on r1, and it is why `/v1/status` reports an active incident. | **The maintainer decides the semantics**; agent implements |
| 1.2 | **Sign the accepted-risk register (W6.5).** **DRAFTED 2026-09-16 — awaiting signature.** The draft is in the PRIVATE tree (`~/code/stellarindex-audit-private/registers/accepted-risk-register-DRAFT-2026-09-16.md`), because its source triage is part of the set `bb9a03bc2` removed from this public repo for enumerating live exploit detail. Every row was **re-verified at HEAD** rather than transcribed — the triage was 52 days and ~40 releases old — and that found a 30% error rate in thirteen rows: **one premise is now FALSE and must not be signed as written** (a feature accepted on "zero production exposure" is enabled in production today), **two items are already RESOLVED and should be closed rather than accepted** (one operator confirmation that is now verifiably true; one whose residual is moot because the value it would protect is returned by a single public DNS query), and **one is materially IMPROVED** (a batch bound that is now a compile-time assertion rather than a config knob). Eight rows re-verified and hold, with two file:line citations updated. The register is signed per section, never in bulk. | **the maintainer signs**; agent drafts |
| 1.3 | ~~**Drill the OFF-SITE restore once.**~~ **DONE (2026-09-03).** `DRILL_REPO=2` ran on r1: `/var/lib/stellarindex/restore-drills/restore-drills.md` carries `2026-09-03 restore drill (repo2)` — restore 2813 s, tip lag 17 ledgers, hash-chain breaks 0, trades window match 2,726,303 = 2,726,303, failures 0; off-site RTO ~47 min against ~9 min from repo1. Residual — **codified 2026-09-04, needs an ansible apply**: `restore-drill-offsite.{service,timer}` (the 15th 04:00 UTC + ≤ 30 min jitter, `DRILL_REPO=2`, no CH stage) is in the archival-node role under `--tags ops-jobs`, gated on `pgbackrest_repo2_s3_bucket`; `restore-drill.sh` now writes one textfile per repo with a `repo` label on every series and holds a non-blocking lock so the two timers and a hand-run cannot share the scratch port or the capacity check; `stellarindex_restore_drill_offsite_stale` (ticket) fires when `last_success_unix{repo="2"}` is older than 35 d or absent, in both rule trees, runbook [restore-drill-offsite-stale](runbooks/restore-drill.md#stellarindex_restore_drill_offsite_stale); the on-box `stellarindex_restore_drill_stale` selects `repo=~"1\|"` — an allow-list of repo1 and the un-labelled series, deliberately not `repo!="2"`, which let any other repo (the script takes any positive-integer `DRILL_REPO`) satisfy its absent branch for a never-drilled repo1. **Read the on-box ticket as unproven for repo1 until the first labelled repo1 run:** the pre-label `restore_drill.prom` is rewritten whole by whichever drill ran LAST, and on r1 that was the 2026-09-03 repo2 hand-run, so the un-labelled series the selector tolerates most likely holds repo2's verdict and the on-box rule reads it as repo1's proof of health. That window closes at the first `restore-drill.timer` run (the first Saturday) or sooner with `systemctl start restore-drill.service`. None of it is on r1: rules ship with `deploy.yml`, the units and the labelled script do not — from `configs/ansible`, `--check --diff` first, then `ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --diff --tags ops-jobs` (`-e ansible_python_interpreter=/usr/bin/python3` on a tag-limited run). Expect the new ticket to fire from rules-deploy until the first labelled repo2 run — the 2026-09-03 hand-run wrote an un-labelled series and does not count — so seed it right after the apply with `systemctl start restore-drill-offsite.service` (~47 min, ~$24 egress). | operator on r1 |
| 1.4 | One live SEV drill + one rollback rehearsal. Scenarios are written and paging is now real; neither has been executed against wired paging. | agent + the maintainer |
| 1.5 | ~~**Wire the five `HEALTHCHECKS_URL_*` on r1.**~~ **STALE — RESOLVED, measured 2026-09-10.** All five are populated in `/etc/default/stellarindex-healthchecks` (56 chars each, the shape of a healthchecks.io ping URL), and the three heartbeat units ran 325-326 times in six hours reporting success. This matters because the script no-ops SILENTLY on a blank URL — every ping is guarded by `[ -n "$PING_URL" ]`, so an unset variable pings nothing and still exits 0, which is precisely the failure this row describes. `pre-launch-check.sh` piped to r1 now reports **0 failures** (3 warnings) against the 4 failures W6.1 records. NOT verified from here: that the pings actually register on the healthchecks.io dashboard — that needs the account, which the maintainer holds. The dead-man behaviour covers it: if pings stopped arriving, healthchecks.io would raise the check, and the alerts arriving today came from a different probe entirely. | — |
| 1.6 | ~~**Re-frame the `recognition` axis on `/v1/coverage`.**~~ **DONE (2026-09-03).** It was a system audit signal, not a source, and as framed could NEVER read complete (`coverage_pct 0.00748`, "23,945 unrecognized shapes on unowned contracts"). It now has its own top-level `recognition` object with its own vocabulary and MORE of the audit's numbers (`unrecognized_shapes`, the new `unrecognized_contracts`, a plain-language `meaning` on the wire), and the headline counts sources only: **20 of 20**. A genuine source failure still fails the headline (`completeness.IsAuditAxis` is a narrow fail-loud predicate). Spec 1.20.0. **The go-live gate below still says 17/17 — that staleness is NOT fixed here** and remains open; the live figure is 20 sources. | agent |
| 1.7 | ~~**Test nets are behind again — REOPENED 2026-09-15.**~~ **CLOSED 2026-09-16.** Both test nets were dispatched via `deploy.yml` and serve **v0.85.0** across every binary the region runs — indexer, api, sla-probe, ops, migrate — verified on both hosts from `/var/lib/stellarindex/deployed-versions/`. r1 serves **v0.86.0**. `stellarindex-aggregator` is deliberately NOT deployed to either: `scripts/dev/region-binaries.tsv` records it `unit-off-disabled-inactive` on testnet and `unit-not-found` on futurenet, so the deploy filters it out rather than obeying the pubnet default and failing a health probe. The stale version stamps those hosts still carry for it name a binary nothing executes. Original text follows. **Test nets are behind again — REOPENED 2026-09-15.** The 2026-09-08 measurement was true on its date and is no longer. r1 serves **v0.81.0**; both test nets serve **v0.63.0**, seventeen releases back, which is what `fleet-release-drift` has been red on. Config has been re-applied to si-testnet from `configs/ansible` (44 changed tasks; galexie and MinIO restarted and verified back). The binaries are the remaining half, and the inventories still carry `manage_stellarindex_binaries: true` from the greenfield bring-up, which predates these hosts having releases to deploy — a `deploy.yml` dispatch per region is the codified path and the flip is the change that makes `fleet-release-drift` mean something. futurenet has had neither half. Two things were learned re-applying it, both now fixed in the role: the migrations sync was O(files) and cost twenty minutes per apply over the test nets' ProxyJump, and the `secrets_file` guard checked the path string rather than the file, so a missing secrets file died forty tasks later inside a `no_log` task. | agent |
| 1.8 | `gh variable delete DEPLOY_APPROVAL_RELAXED` + r1 required-reviewers, at the flip. | **the maintainer** |
| 1.9 | ~~**D7** — the thin-pool third-alias VWAP review this plan says is owed "before public traffic". Never done, no artefact.~~ **DONE (2026-09-04).** Artefact: [d7-thin-pool-third-alias-vwap-review-2026-09-04.md](../methodology/d7-thin-pool-third-alias-vwap-review-2026-09-04.md). Verdict: every first-hit served-price walk crosses the base's alias family with the **literal** quote and is gated (substance + trailing guard + freshness, or the trade-count floor), so a Soroban SAC/SAC pool is not a candidate for a classic-quoted read — pinned by tests proven non-vacuous by mutation. The one **merge** walk, `/v1/price/tip` (+ `/stream`), admitted SAC combinations unasked and served a single thin-pool print in any 30 s window the SDEX book was silent — **fixed** (`tipMergePairs`: the established forms merge; a SAC-form combination the caller did not name is read last, only after the closed bucket and every other fallback have missed, so a wrapped classic whose only market is its pool still serves from it — gated by the full substance floor, ≥ $1,000 volume **and** ≥ 20 distinct buckets **and** ≥ 6 h span over the trailing 24 h, measured on the pool alone for such an asset — and a pool print never displaces or blends into a classic-book answer; red→green). Four bounded residuals recorded (alias-union substance verdict on SAC-keyed reads; `/v1/price/at` after 24 h silence; valuation tier 3 after 1 h silence; the tip's last-tier pool read for a Soroban-only wrapped classic) and two follow-ups outside that count (coverage; a decoder-level both-legs pin); the first residual is accepted for v1 as-is, bounded by the trailing-baseline guard and freshness, with the one-method `SubstanceGate` literal-pair measure recorded as a post-v1 option (artefact §7 R1). | agent |
| 1.10 | **DONE (2026-09-04).** Applied from `configs/ansible` at `9dd126f06` with `--tags ops-jobs` (`--check --diff` first; the enable step fails in check mode before the unit exists, as documented below). `verify-served-values.timer` is armed (next 06:20 UTC) and a hand-started first run exited 0: `xlm_total_supply`, `xlm_circulating_supply` and `usdc_total_supply` all OK (rel_err 4.0e-7, 3.0e-4, 1.5e-3); `served_values.prom` is written, so the three served-value alerts now select a series that exists. Recipe kept: **Apply the served-value truth harness to r1.** `verify-served-values.{service,timer}` and their `ops-jobs` task are codified and were NOT on the box — an ansible commit does not reach r1 on its own, and until this runs the harness that reconciles the flagship served numbers against SDF/Stellar Expert is still not scheduled anywhere. From `configs/ansible`, `--check --diff` first, then `ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --diff --tags ops-jobs` (tag-limited runs need `-e ansible_python_interpreter=/usr/bin/python3`). Verify: `systemctl list-timers verify-served-values.timer` shows a NEXT/LEFT, and after the first 06:20 UTC run `/var/lib/node_exporter/textfile_collector/served_values.prom` exists and Prometheus has `stellarindex_served_value_last_run_unix`. Two expected transients: the weekly `ansible-drift` workflow reports the new install/remove tasks and the systemd handler as changed until the apply lands, and `stellarindex_served_value_check_stale` fires off its new `absent_over_time` arm from the moment the rules deploy (rules ship automatically with `deploy.yml`; the units do not) until that first run stamps the series — up to ~25 h after the apply. **If the first scheduled run exits non-zero** — the 2026-07-02 hand-run found three findings, so that is a plausible outcome — `stellarindex_served_value_drift` and `stellarindex_served_value_unit_failed` fire roughly 26–50 h after the apply and stay firing until the finding is resolved or accepted; triage per the drift section of [served-value-drift](runbooks/data-freshness.md#stellarindex_served_value_drift).| operator on r1 |
| 1.11 | ~~**Enable ClickHouse's Prometheus endpoint on r1.**~~ **DONE (2026-09-04).** The lake had exported no metrics at all: the stock `/etc/clickhouse-server/config.xml` ships the whole `<prometheus>` block inside an XML comment, nothing listened on 9363, and Prometheus held zero `ClickHouse*` series, so the API's ClickHouse READ path, the rollups and the CH-fed projector had ingest-side coverage only. The role's drop-in and scrape job (`9eeac705b`) are now on the box: `curl -s localhost:9363/metrics` returns `ClickHouse_Info`, the `clickhouse` scrape target reads `health: up` against `http://localhost:9363/metrics`, and Prometheus holds **3,092 `ClickHouse*` series**. No ClickHouse restart was performed — the `config.d` reloader picked the drop-in up. **The expected transient did fire and was handled:** `stellarindex_clickhouse_server_down` is a *page*, and it deploys with the rules while the drop-in does not, so it fired on its `absent_over_time` arm the moment the rules landed. It was silenced with a reason for the duration of the apply, resolved on the first successful scrape, and the silence was lifted — `amtool silence query` is empty. **That ordering is the lesson, not the alert:** a page whose rule ships automatically and whose exporter ships by hand is a self-inflicted page every time. Rows that install an exporter must land the exporter before, or in the same change as, the rule that watches it. | operator on r1 |
| 1.12 | ~~**Give the ClickHouse schema snapshot an off-site target.**~~ **STALE — RESOLVED, measured 2026-09-15.** `ch-schema-snapshot.service` on r1 carries `Environment=SNAPSHOT_MC_TARGET=offsite/stellarindex-pgbackrest-r1/ch-schema`, and the 05:43 run that day logged `pushed to offsite/stellarindex-pgbackrest-r1/ch-schema/2026-09-15 (8 object(s) verified at the destination)`. The `offsite` mc alias resolves to `https://s3.eu-central-1.amazonaws.com` — a different provider, not the same host and not the same ZFS pool — and the bucket holds eight consecutive daily snapshots through that date. The row's premise (`ch_schema_snapshot_mc_target` unset) no longer holds; the lake's DDL is off-site and the ADR-0043 schema-first restore has a source. | — |
| 1.13 | ~~**`/v1/history` reads ONE stored orientation.**~~ **DONE (2026-09-04) for this endpoint; the same blindness on four sibling surfaces is now row 1.16, which this row does NOT close.** The page folds both directions. `Store.TradesInRangeAfter` still keys on `(base_asset, quote_asset)` literally — that primitive is honest about reading one orientation — and its caller reads each alias form in both directions and re-expresses the flipped rows into the orientation requested: the two legs swap, the two smallest-unit amounts swap with them, and `price` (always quote/base) is the exact reciprocal. The inversion is an integer SWAP, not a division, so it is exact at any magnitude, and a zero amount renders the endpoint's existing zero-denominator answer rather than poisoning a row. Same per-row rule as `Store.OHLCSeries`'s `norm` CTE on the bucket side, keyed on the ROW's own `base_asset` rather than on which read returned it. **Pagination is where the work was.** The merge compares `(ts, ledger, tx_hash, op_index)` — the four keyset components Go and Postgres order identically — and never `source`, whose Go byte order can disagree with the database's collation. A page therefore never ends INSIDE a group of rows tying on those four; it retreats to the group's lower edge, and where that edge is index 0 it runs to the upper edge instead, **but only after the group has been proved complete in BOTH directions** by re-reading any direction whose read stopped on it, at the store's maximum page width in one go. A bounded re-read ladder was tried first and was wrong: a group past its top fell to a branch that cut through the group and lost a row (the round-one shape, in the branch documented as the safe one, reachable at 233 rows in one group). The branch is deleted, not documented — a tie group cannot exceed the distinct-source count, so one maximum-width read settles it. A first attempt served such a group without completing it and LOST rows a single-orientation read had served: a three-source group at `limit=1` (client-reachable — `limit` validates to [1, 10000]) served two rows and never returned the third on any page, and an exhaustive sweep of 768 small configurations lost 28 rows. Both are pinned red-on-unfixed and now serve every row exactly once. A first pass then repeated 168 rows across those same 768 configurations — a cursor naming the last served ROW re-serves the rows of its group the database orders above it — so a page that ends on a group already proved complete now resumes PAST the whole group by key (same ts/ledger/tx_hash, `op_index` stepped once, no source), which no row of the group can satisfy and every later row can, and which needs no `source` comparison. Guarded against a wrapped `op_index`. Duplicates 168 → **0**: pagination is exactly-once, pinned as zero rather than bounded. A page can still exceed `limit` by up to one whole tie group, and can still be short while rows remain. "More rows exist" is reported by the read, not inferred from `len(page) == limit`. The coverage probe widened in the same change, from `spanAliasedAsStored` back to the default both-directions span. **Cost, stated rather than hidden:** two sequential store reads per alias form where there was one, under the UNCHANGED 8s handler ceiling — so a populated pair whose single-direction read already ran 4-8s on a cold window can now reach that ceiling and answer 503 where it used to answer. The wide fan-out (six reads for an empty three-alias pair) is the cheap prove-empty case; the narrow one is the expensive case. Not measured on r1. Spec 1.21.0 -> **1.22.0** (description only); reference docs, Postman collection and the explorer's generated types regenerated. |
| 1.14 | ~~**The fiat point path is not alias-complete on the quote leg.**~~ **DONE (2026-09-05), landed with 1.15 in one change — which is the only way it could land.** Single-bar `/v1/ohlc`, `/v1/vwap` and `/v1/twap` read each USD-pegged constituent in its literal spelling, so a declared peg's SAC wrapper — where every Soroban pool's USD leg lives — was never read, and an asset whose only USD depth is such a pool 404'd while `/v1/chart` charted it. `Server.fiatCombinedTrades` now takes the same split `Server.usdPeggedConstituentSets` gives the series: it merges the established constituents and reads the held-back SAC forms exactly when that merge returned nothing over the window. **That is not a second rule agreeing with 1.15's — it is the same rule at this path's grain, and getting the grain wrong cost a round.** A first build gated on the WHOLE window, reasoning that a point window is its own bucket. It is, but only when the window IS one bucket — and every parity fixture in the tree was one bucket, so nothing caught it. Over a two-hour window with the book trading in the first hour and the pool in the second, the series served two bars carrying both while `/v1/vwap` served the book alone (`n=1 v_base=1000` against `n=2 v_base=1500`), and single-bar `/v1/ohlc` reported a high of 0.20 where the same window's series reported 0.80: two served money surfaces contradicting each other, which is the very defect this path exists to remove. The gate is now per bucket here too, at **`1m`** — the finest interval the series accepts and the finest rung the deployment materialises. The point path reads raw trades, so it buckets them itself at no extra read cost, and the `answered` set is snapshotted from the established rows before any held-back row joins them, so two held-back constituents sharing an empty bucket still merge with each other. **The claim, stated precisely rather than aspirationally: point equals the series at `interval=1m` EXACTLY.** It cannot equal every interval — per-bucket admission is a function of what a bucket is, so a coarser bucket is likelier to hold an established print and suppresses more, and a 1h and a 1d series disagree with each other for the same reason. That is the caller's question changing, not the population splitting. `TestFiatPointEqualsTheFinestSeriesExactly` pins it; `TestFiatPoint_PoolInAnAnsweredBucketIsSuppressed` pins the thin-pool guarantee at this path's grain. Which is why the row was gated: `usdPeggedConstituents` feeds both paths from one list and `TestFiatVWAPPointMatchesSeries` / `TestFiatSingleBarOHLCMatchesSeriesExtremes` pin the two to one population, so widening either alone turns the pin red — correctly, since two surfaces answering one question from two populations is the C1-024 defect this path was rebuilt to remove. Both are widened together and the pin holds; a new fixture pins it on the held-back arm itself (`TestFiatPointMatchesSeries_OnAPoolOnlyVenue`), where the two previously served a series and a 404. The `distinctMarkets` dedupe that shipped with 1.16 is applied across BOTH sets, so a market reachable under either spelling stays with the set read first. Measurement, cost and the rejected alternative are on row 1.15. |
| 1.15 | ~~**A fiat-quoted `/v1/ohlc` series cannot reach a SAC-quoted Soroban pool.**~~ **DONE (2026-09-05).** The combine read each USD-pegged constituent under the one quote spelling the peg expansion names it in, and `Store.OHLCSeries` takes that spelling literally, so a pool stored as `<X SAC>/<USDC SAC>` was out of reach while the SDEX book at `X/USDC-GA5Z…` was not. **Measured on r1 before it was designed** (read-only, bounded, 365 days of `prices_1d` — the rung the series and the floor probe both read): 132 markets carry the USDC SAC on one leg, 67 distinct counterparties, 1,916,996 prints. Splitting those counterparties by whether ANY spelling the combine reads holds a single bucket for them: 24 reachable (1,656,163 prints, $419.18M) and **43 with SAC-only USD depth — 260,833 prints, $14,630,761.46**, the largest carrying **$6,375,518.23** over 129,925 prints across an unbroken year and five more in six figures. Every one served `intervals: []` and a `404`, while `/v1/chart`'s proxy walk charted it. The shape occurs, at scale. **(a) First-hit across ALL established spellings of ALL families.** `Server.usdPeggedConstituentSets` returns two sets: `established` (the peg expansion as before, each quote in its priority-first spelling) and `heldBack` (the remaining canonical form of each of those quotes — the declared peg's SAC wrapper). Two passes across every family rather than one pass per family, which is `usdPegProxyQuotes`'s classic-then-SAC ordering at the constituent grain and `tipMergePairs`'s merge/last split at set level, so no family's thin pool is consulted before another family's deep book. `canonical.AssetAliases`'s SAC-last ordering is kept, and the two sets are deduplicated by MARKET against each other. **(b) The decision, argued in [aggregation-plan](../architecture/aggregation-plan.md#the-fiat-quote-leg-per-bucket): a held-back spelling is suppressed for a bucket an established spelling answered, and for no other bucket — per BUCKET, never per response.** The alternative was a first-hit evaluated once per response, and it is refused on a property rather than a preference: it makes the constituent set a function of the WINDOW, so the same day renders one way inside a window the book also covers and another way inside one it does not, from one unchanged database. Per-bucket resolution depends only on the bucket, so a bar is identical in every window containing it — pinned as a test, not asserted. **What the alternative would have produced, measured:** across the 24 assets holding both a book and a pool, **3,356 daily buckets are pool-only** against 3,032 shared — the pool trades on more days than the book does — carrying **671,712 prints and $175,962,608.19** that a per-response first-hit reports as quiet. That is the second fault the two rejected candidate shapes predicted, and in production it is larger than the gap being closed. **(c) The thin-pool guarantee, and the shape that proves it needed.** Merging the sets unconditionally, one $0.60 Soroban print — 0.43% of the day's dollar volume, and sixty times migration 0115's sub-cent extremes floor, so a legitimate-notional pool rather than dust — sets a real bar's high: `GQX-GD7TC72O…` on 2026-06-02, the book's 660 prints and high 9.5396055089328007 becoming 661 and 13.0995677490335234, **+37.32%** (worst in a 180-day sample: +37.52% on three prints worth $1.03). The launch plan's own fixture shape reproduces beside it at `n=51` with high 0.5000000000. Both are captured red-on-unfixed. Gated per bucket, a held-back bar in an answered bucket is not down-weighted, banded or filtered — it is **not in the bucket**, so the per-bucket max, min, count and sums this row named as the fault cannot see it, and a rejected bar contributes no scale either. The series stays consistent with the live aggregator's own source set: every bucket that set can answer is answered by it alone, byte for byte. **Consequence worth stating plainly:** on r1 `native/USDC-GA5Z…` holds 176 daily buckets from 2026-03-12 while `<XLM SAC>/<USDC SAC>` holds **874 from 2024-03-12**, so the flagship `native/fiat:USD` series gains the buckets the classic book's aggregate does not hold — depth, not noise ($371M over 365 days, and the market `/v1/chart` has served for months) — but those bars are Soroban-AMM-sourced and `OHLCSeriesBar.Sources` is off the wire. That opacity is pre-existing (the combine has always merged SDEX, four CEXs and the FX pollers unattributed) and is widened here, not introduced; putting `sources` on the wire is a spec change and was not made. The coverage floor widens with the read — a floor is consulted only on an empty answer, and an empty answer is one where the held-back set was read too — so `ohlcCoverageSet` spans the whole constituent list and the probe/read equality test moves with it. **The widening forced a defect open one layer beneath it, and it was NOT caused by the widening.** The bar-scale lift target was the maximum across the RESPONSE, which cannot survive a window-dependent constituent set: an admitted held-back bar contributes a scale, so a pool-only day changed the lift applied to the BOOK's bar on a different day — that bucket serving `v_base` 1000000 alone and **10000000** beside the pool day, ten times over from one unchanged database, and breaking the exact window-invariance §7.5 had just argued for. The same 10x split reproduces with NO held-back set at all, from two ESTABLISHED constituents at two venue scales on two days — `native/<USDC classic>` (sdex, 7dp) beside `crypto:XLM/crypto:USDT` (binance, 8dp), r1's actual constituent set — so a response-wide maximum was always a way for a window to change a served volume. The lift target is now the maximum **within a bucket**: still an exact integer multiply by `10^(common−scale) ≥ 1` (ADR-0003), and a bar now depends on nothing outside its own bucket. It costs the uniformity of scale ACROSS buckets of one response — which only ever held within one response, so a caller paging two windows already received the same bucket in two units with nothing on the wire to say so; it is also what the wire already promises (`v_base` is documented as a per-bucket sum); and prices are untouched either way, since a lift multiplies both legs. **Disclosed:** on r1 this changes served volumes for on-chain-only buckets sitting in a response beside CEX buckets (`native/fiat:USD` before 2026-05-05, which previously rendered lifted to 8dp and now render at their own 7dp sum). Prices, extremes and counts unchanged. **Two fixtures were vacuous and are fixed:** the parity fixture stamped every trade `sdex` (one scale, every lift factor 10^0), and the window-invariance pin used bars with an empty `sources` column — so it passed with the scale machinery deleted outright, asserting precisely the property that was broken. Both now carry real venue names at two scales. **Cost:** no query shape changed (`Store.OHLCSeries` untouched), so there is no plan to re-cost; the added cost is extra reads of the same shape, bounded by (base alias count) x (declared pegs with a SAC wrapper) = 3 x 1 on r1 — `native/fiat:USD` **21 -> 24** constituent reads (1.14x), single-wrapper `AQUA/fiat:USD` **7 -> 8** — each through the cached `HistoryReader`. An operator declaring no SAC wrapper gets a byte-identical response. The point-vs-series parity fixture stamped every trade `sdex` and so held the invariant vacuously; a three-constituent fixture across two venue scales is added, and with the series-side lift removed it reports `base_volume point 100000 != series 37000` while the single-source one stays green. Spec 1.25.0 -> **1.26.0** (description only); reference docs, Postman collection and explorer types regenerated. **Row 1.14 closes with this in one shape**, so `/v1/ohlc`, `/v1/vwap`, `/v1/twap` and `/v1/chart` now answer from one population. | agent |
| 1.16 | ~~**Four surfaces still read ONE stored orientation of a market.**~~ **BOTH HALVES CLOSED — the aggregate half shipped the same day this row was written and the row never learned.** `1fca7bceb` (2026-09-05 05:39, released in v0.61.0) made `Store.TradesInRange` two-armed with the same per-row leg swap, which covers all five aggregate surfaces at once because all five read it. **Verified on live production data 2026-09-16, not inferred from the diff:** over the exact hour `/v1/vwap` reported for native/USDC-GA5Z…, the lake holds **7,256 trades stored forward and 1,173 stored reverse**, and the served `trade_count` is **8,429** — the sum, to the trade. 13.9% of that hour's prints are stored the other way round and every one of them is in the VWAP. An audit of every reader in `internal/storage/timescale/trades.go` finds one single-direction survivor, `TradesInRangeAfter`, and that is deliberate: its only caller folds both directions itself (`tradesInRangeAfterWithAliases`), which is why `CoverageFloorReader.EarliestBucketAsStored` records that no surface takes the as-stored reading today. Original text follows. **Four surfaces still read ONE stored orientation of a market — and `/v1/history` no longer does, so they now DISAGREE.** **MECHANICAL HALF DONE (2026-09-05).** `/v1/observations`, its SSE stream and `/v1/price`'s last-trade fallback now serve a market whichever way round it is asked for, so they agree with `/v1/history` about whether one exists. **The row's own inventory was wrong on both readers and is corrected here:** `/v1/observations` reads `Store.LatestTradePerSource` (not `Store.TradesInRange`), and `Store.LatestTradesForPair` is read by `storePriceReader.LatestPrice`'s last-trade arm — the `/v1/price` family — not by the status page, which reaches these rows only THROUGH `/v1/observations`. `Store.TradesInRange` is read by `/v1/vwap`, `/v1/twap`, single-bar `/v1/ohlc` **and `/v1/price/tip`** plus the aggregator orchestrator: a fifth serving surface this row did not name, which the aggregate half must cover. **What shipped:** both mechanical readers select BOTH stored directions as two unioned arms — each arm is the query it used to be, so each keeps its index-ordered scan (`trades_pair_source_ts_idx` / `trades_pair_ts_idx`) and its early stop, where a single OR'd predicate would have had to bitmap both directions and sort them on a read whose worst case is already a full-history walk for an empty pair. Flipped rows are re-expressed per ROW on the row's own `base_asset`, by SWAPPING the two legs and the two smallest-unit amounts — exact at any magnitude, no division performed anywhere, so a zero amount cannot poison a row — which is the same rule `/v1/history` and `Store.OHLCSeries`'s `norm` CTE apply. **`Store.LatestTradePerSource` needed a fold rule, not merely a wider read:** one source that traded the market both ways round now arrives twice and the surface is one row per source, so the two are folded to whichever trade came later, compared on `(ts, ledger, tx_hash, op_index)` — a TOTAL order within one source (the trades primary key is those four plus `source`) and the same four `/v1/history` orders its page on. `source` is never compared, so no Go ordering has to agree with the database's collation, and the selected row is the same whichever way round the market is asked for. **Cost, measured not asserted:** EXPLAIN on r1 (plan only) puts the per-source read at 4243 → 8487 with every arm keeping its per-chunk SkipScan over the compressed index, and the last-trade read at an early-stopping `Limit` 1.67 → 2.26 — exactly twice, well inside the unchanged 8s ceilings. **No probe needed widening:** neither surface carries a coverage floor, and the `/v1/price` stablecoin proxy's gate `RecentClosedVWAP1mExists` already spanned both directions, so the read has caught up with its probe rather than outrun it. Spec 1.24.0 → **1.25.0** (description only); reference docs, Postman collection and explorer types regenerated. **AGGREGATE HALF DONE (2026-09-05) — this row is now closed.** `Store.TradesInRange` reads both stored directions as two limited arms and re-expresses per ROW, so `/v1/vwap`, `/v1/twap`, single-bar `/v1/ohlc`, `/v1/price/tip` and the orchestrator compute over the whole market. **Measured, not asserted:** one hour of `native/USDC-GA5Z…` on r1 holds 2957 rows one way round and 2794 the other, so the served window was 51.4% of the market — and a biased 51.4%, the sell side, since the decoder sets `base = soldAsset`. The VWAP moved -0.0016%; the EXTREMES did not survive — the served high understated the hour's true high by 0.65% (0.1806435916 vs 0.1818181818) and the served low was 0.12% too high, on a bar that never looked empty. It also closed a live C1-024 violation the parity tests cannot see, the point path reading raw trades one way while the series reads a CAGG that folds. **The design question this row deferred is answered and written down** ([aggregation-plan](../architecture/aggregation-plan.md#the-direction-fold)): every aggregate here is defined on the two integer LEG AMOUNTS and never on a stored price, so the leg swap re-weights the mean at the same instant as it inverts the price — the weight IS the row's base leg, and a flipped row's base leg in the requested orientation is its stored quote amount. No division, no second re-weighting step. Re-expression happens BEFORE any comparison because an inverted price turns a maximum into a minimum: appending flipped rows unre-expressed reports a **high of 10.0** where the market's high is 0.25, a fully populated and entirely wrong bar, pinned red-on-unfixed. Two fixture-free properties are pinned too — `VWAP(A/B) x VWAP(B/A) == 1` exactly, which only a re-weighted fold satisfies, and `low < VWAP < high`. Truncation still keeps the newest rows and the orchestrator's detector still fires (correctly, more often). **Cost from EXPLAIN on r1, plan only:** a `Merge Append` over two independently limited arms, every arm keeping its `trades_pair_ts_idx` scan and its per-chunk `ColumnarScan` — 1 h populated 25.75 → 44.14 (1.72x), 24 h populated 1169.12 → 1191.84 (**1.02x**), empty pair over full history 147.06 → 184.61 (1.26x), all inside the unchanged 8s ceilings. **The class guard shipped with it**, widened from `FROM prices_*` to the `trades` hypertable, with exactly ONE exemption that does not say a read is blind: `TradesInRangeAfter` is folded by its caller under a keyset cursor, which a union cannot carry. The widened scan found a third pair-bound trades read the row did not name — `FXQuoteAtOrBefore`'s legacy fallback on the triangulation money path — and since it serves a price it is not exemptible by that guard's own rule, so it is folded rather than excused (dormant: zero rows on r1 across both FX sources over 400 days). One consequence worth recording: folding direction in the store makes a pair and its flip ONE read, and `/v1/price/tip?asset=native&quote=crypto:XLM` really does emit both into one merged VWAP, so the two merging sets now deduplicate by market and keep the first spelling. No spec change — the fold is invisible on the wire. |

### RWA coverage — what stands between the served total and the $4.087B a third party reports

> **UPDATE 2026-09-16 evening — three lines moved, and one of them was
> ours all along.** Everything below this box was true when written and
> two of its figures are now superseded. Served reference total is
> **$2,535,764,187.91** across **29 assets / 18 issuers** (was
> $2,533,472,871.25 / 16 / 6), verified live after v0.86.0.
>
> **1. A commodity line opened.** Matrixdock's XAUm is bound and priced
> at **$4,599,642.93** — the first commodity row on this surface with an
> independent price at all. matrixdock.com names the contract itself; the
> deployed wasm is source-verified against the issuer's own GitHub; the
> listing directory names the same address. `by_class` now carries four
> of the five declared classes.
>
> **2. `stock` appears for the first time, and it was OUR bug.** The
> "WisdomTree — NO ATTESTATION" line below said four issuers were refused
> at R2 because no stellar.toml had been fetched. The real cause was that
> `stellar.wisdomtree.com` serves a REAL SEP-1 whose `ACCOUNTS` array
> ends with an unterminated string on line 20, and a whole-document parse
> threw away all eighteen of its well-formed `[[CURRENCIES]]` tables.
> Section recovery (v0.86.0) reads them; **12 assets are now admitted**
> — 8 bond, 3 stock, 1 commodity — carrying 7,023,543 tokens across
> roughly 30,000 trustlines each. The thirteenth, `CRDT`, stays out
> because its issuer is the one named on the broken line and is in no
> directory. The three lookalike domains (`wisdomtree.bond` ×2,
> `wisdomtree.co.com`) are directory-flagged `malicious` and stay
> refused.
>
> **This was the only line in the whole reconciliation that was a
> coverage failure of ours rather than a refusal, a missing price, or a
> figure with no on-chain basis.** It is closed.
>
> **3. A measurement error worth recording.** The first pass reported
> WisdomTree as holding zero supply on Stellar. It does not: Horizon's
> `/assets` record has no `amount` or `num_accounts` field, so reading
> them returns 0.00 for every asset and reads exactly like an absence.
> Verify a bulk probe on a known case first — BENJI's
> `balances.authorized` is 435,910,656.4479976 and matches our published
> figure to the digit.
>
> **What did NOT move.** All 12 WisdomTree assets are unpriced: only
> WTGX is in the independent listing directory, and the RWA surface's
> classic arm does not read listing prices (the contract arm does, and
> `/v1/assets` has its own listing-valuation path). That is a real
> internal inconsistency — the same published price values a contract row
> and not a classic one — worth about **$2.3M** today against a directory
> that holds **33 classic rows, all 33 priced**, versus 17 contract rows.
> Two-thirds of an independent price source goes unread. Not fixed here;
> it is the largest remaining *correctness* item on this surface even
> though it is a rounding error on the total.
>
> **rwa.xyz (2026-09-17): "Distributed Asset Value" $3,275,441,791**, excluding its own $359.5M
> stablecoin class and $78.2M "represented". Per issuer against ours: Spiko 1,572 vs 1,543;
> Ondo 536 vs 536; Franklin 523 vs 436 (FOCGX + gBENJI unpriced); **Realiz 500 vs 0** (the
> VuMe line below); RedSwan 72; WisdomTree 40 (12 admitted, unpriced); Centrifuge / Figure /
> Rivool / NYALA / Liqvid / MB / Bitbond ≈113 (no primary-source binding); Etherfuse and
> Matrixdock agree. Two-thirds of the gap is one contract.
>
> **The $4B question is settled by the four lines below and this does not
> change it.** Two of the four are deliberate refusals, one is a price
> that exists nowhere, and one is a long tail. Nothing found today
> suggests the target is reachable on verifiable evidence.


Measured 2026-09-15 against that dashboard's own SQL, the issuers' own APIs and
our own lake. Every line reconciles to on-chain Stellar state; none of them is a
number anyone invented, and we agree wherever both sides measure — one issuer
within 5.2%, another within 1.0%, a third within 0.01%. The gap is OUR coverage,
so it is written down here rather than re-derived.

| line | their figure | ours, and what stands in the way |
|---|---:|---|
| One issuer's tokenized funds | $1,659.3M | **CLOSED 2026-09-15.** $395.2M was published; the other $1,182.3M was four share classes of an overnight swap fund held out because the class vocabulary had no true word for it. `fund` now exists on the contract arm and all four are bound. |
| A private-credit platform, 24 deal contracts | $548.1M | **CLOSED — the supply half is done and verified; the price half does not exist.** Storage-derived supply now serves for **all 24**, summing to **548,113,042.88 tokens**, matching the independent measurement to the unit, where every one of them previously reported `0` because they emit no SEP-41 events. What cannot be had is a price. Verified 2026-09-16, not inferred: none of the 24 appears in the curated account directory, none in the independent listing directory, and they carry **no on-chain METADATA key at all** — 15 of the 24 share the storage symbol `PC000`, which the publishing seed explicitly states must not be treated as an identifier. The external figure for this line comes from a CSV the dashboard's author uploads: its query (dune.com/queries/6961846, "Mcap by Month by Company", by @stellar) values `stellar.token_balances` × `close_usd` from **`dune.stellar.dataset_asset_prices`**, and takes membership and class from **`dune.stellar.dataset_recognized_assets`**. `dune.<team>.dataset_<name>` is Dune's CSV-upload namespace (docs.dune.com/web-app/upload-data: "upload any csv file … queryable via the schema dune.team_name.dataset_name"). For these 24 contracts the dashboard's figure is `548113042.8799999` against our supply of 548,113,042.88 tokens — identical to the token, so the uploaded `close_usd` is **1.00**, par. The same query also hard-codes `EUTBL` on 2026-05-17 as the literal `503334541.7` and divides `deJTRSY`/`deJAAA` by `100000000000` on that day. Publishing a supply without a price is honest and adds **$0**; inventing one is the thing this whole surface exists not to do. |
| **"Realiz" — VuMe Bond 2030** (`CBUBVYRK…2VJ4`, code `TPT30`) | $558.8M | **CORRECTED 2026-09-17: this line is a bond contract, not a real-estate issuer, and the refusal stands on the contract's own facts.** The address came from rwa.xyz (which carries it at par, $500,000,000; Dune at 1.1174 from the same uploaded CSV). On chain: deployed 2026-02-23 by `GCUTJSAK…`, an account whose `home_domain` is **`lobstr.co`** — a retail wallet; wasm **unverified**; **5 invocations and 8 events in its entire life; 4 storage entries**; 500,000,000 tokens (18dp) minted in two events; never traded; no SEP-1 (realiz.io serves 404 for `stellar.toml`); in no directory. Nothing on-chain or at realiz.io ties the contract to Realiz — only the two aggregators do, and they share a curation. This one line is **61% of the gap to rwa.xyz's $3.275B**; take it out and rwa.xyz reads $2.775B, within 1% of what an issuer-NAV source plus long-tail identity bindings reach. Publishing $500M on it is the exact claim this surface exists to refuse. |
| Real estate (rwa.xyz: RedSwan, 7 assets) | $71.7M | Measured 2026-09-16: **15 issuers on Stellar declare a `realestate` anchor. 13 are in the curated directory and all 13 are scam-flagged** (`serial SCAM Counterfeiter`, `SCAM`, `malicious`); the other 2 are in no directory. Zero are clean and recognised. rwa.xyz appears to carry this bucket as "represented" ($78.2M) rather than distributed. Reachable only with a primary-source binding for the real issuer. |
| A money-fund issuer's other three tokens | $82.0M | **We hold the supply for all three** (they are classic assets, already in the lake, reconciling to an independent source within 0.26%). Blocked on an independent PRICE: only the flagship is in the listing directory, and the other three have no oracle feed. |
| Everything else | ≈$241M | Price feeds and supply bases we do not have, spread thin. |

Two figures worth keeping separate: **what is on Stellar** and **what we can
publish a defensible number for**. This index only ever publishes the second.
After the vocabulary change the served reference total is **$2,533,472,871.25**
(measured live, 2026-09-16). The remaining ~$1.5B is: a class whose entire
declared population on this chain is scam-flagged, a class of token with no
independent price in existence, three tokens whose supply we hold and whose
price we cannot source, and a long tail of feeds. None of it is a coverage
failure to be fixed by trying harder, and two of the four are things this index
refuses on purpose.

One question is upstream of all of it and is the maintainer's: **the external
figure measures LOCATION, not ownership.** Its query sums
`trustline_balance + liquidity_pool_balance + contract_balance` with no issuer,
treasury or distributor exclusion, so minted-but-unsold inventory sitting in an
issuer's own address counts at full face value. Our classic arm excludes the
issuer's balance (ADR-0011 Algorithm 2, `issuer_exclusion`); our contract arm,
today, does not — it sums issuance, and `BasisSEP41TotalOnly` says so. Whether
the headline should be measured by location or by ownership decides whether
$4B is the right target at all. Publishing both, with a holder-concentration
column, is the option that needs no one to choose in the dark.

**DECIDED — measurement basis and the two policy calls.**

- **Basis: publish both.** The headline is ownership-basis on the classic arm
  only (issuer/treasury excluded where we can identify it); the contract arm is
  still total-only (`BasisSEP41TotalOnly`) until the gaps below are closed. A
  location-basis figure will be shown beside the headline so a reader can
  reconcile to any third party; it is not yet served. Neither replaces the
  other. $4B is therefore not a target: the comparable location figure is what
  we reconcile against, and the gap lines above are explained, not chased.
- **TPT30 bond line (~$559M): declined.** It is a bond contract with no on-chain
  tie to its claimed issuer (see the VuMe row); of the real-estate class proper,
  13 of 15 are scam-flagged and the other 2 are in no directory. Re-open only
  with a primary-source binding.
- **Private credit (~$548M): supply served, price withheld.** No price exists;
  a supply without a price adds $0 and is not valued at par.
- **Remaining engineering (not a decision):**
  - Compute and serve a location-basis total (every holder balance, no
    issuer/treasury exclusion) next to the ownership headline, on the API and
    the RWA page.
  - Contract arm (`BasisSEP41TotalOnly`, `internal/supply/sep41.go`): the
    per-contract exclusion list already exists as `[supply].per_asset_locked_sets`.
    What is missing: (1) track the SEP-41 admin balance (`set_admin`);
    `StorageSEP41SupplyReader` returns `AdminBalance=0`
    (`storage_sep41_reader.go`), which is why the basis is total-only;
    (2) configure `per_asset_locked_sets` entries for the RWA contracts'
    issuer/treasury holders, after the `sac_wrappers` observability
    prerequisite noted at `internal/config/config.go`; or (3) add a
    holder-concentration column.

### Tier 2 — real work that does NOT gate the announcement

Named explicitly, because all of them are carried below as if they did:
**W3.4** (contract-tx index), ~~**W5.4** (13 supply-rollup resets)~~ **VERIFIED
CLEAN 2026-09-17** — `supply verify-rollup` on r1: "OK: 45 checkpoint(s) reconcile
with the authoritative re-sum (0 drift, tolerance 0)"; no reset is needed and
the item is closed on that evidence, **W5.6**
(`contract_events_daily` v2 — the branch is not even on origin), **W5.7 /
W5.8** (CEX dust delete, galexie trim, `soroban_events` decommission #803 —
destructive, should be last), **W8-9b**, **W8-10a**, ~~**W8-12**~~ (ACCEPTED
2026-10-02, see W8 item 12), **#340**
items 6-9, **#349-#352** (correctly labelled post-v1), **#372**, the decks,
the CoinGecko Pro purchase, enabling hashdb, and IP rotation. **HA / R2+R3
is superseded by D2** (single box with tested restore for v1); the
2026-08-28 "second public host into the launch gate" line is stale.

**RWA — reaching the third party's $4.087B on prices of our own (2026-09-17
evening, in progress).** The maintainer's standing ask: the served RWA figure
should meet or exceed the SDF-published Dune dashboard's, sourcing prices
ourselves where possible, with Dune's own tables (the curated arm, v0.88.1)
as the trusted backstop. Where each unpriced line stands after today:

| line | tokens / value | status |
|---|---:|---|
| Franklin gBENJI + grBENJI (Lux CNAV MMF, ISINs LU2900381208 / LU3258450587) | 57.3M tokens ≈ $57M | ~~**PRICED 2026-09-17**~~ **NOT LIVE until the release after v0.89.3 — see the 2026-09-18 correction box below.** Original row: — admitted through the domain-sibling arm (`recognition: curated_account_directory_via_domain_sibling`, the SEP-1 at franklintempleton.com binds them beside the listed BENJI issuer) and valued at the prospectus constant NAV $1.00 (`reference.provenance: prospectus_constant_nav`; the issuer's page showed NAV $1.00 / MTM $0.9999 on 2026-09-16). The page's figures load from `POST franklintempleton.lu/api/pds/price-and-performance?op=Pricing` — a public GraphQL endpoint (query extracted from the bundle) that answers `Overview: null` to every country/language pair tried; the browser's own request body was not captured. A live daily check is the follow-up. |
| Franklin sgBENJI (Singapore VNAV, SGXZ71843866) | 25.0M tokens ≈ $25M | admitted (sibling arm), **unpriced by design** — an accumulating VNAV class (factsheet NAV $1.02 on 2026-02-28) needs a live NAV, not a constant. Source: franklintempleton.com.sg, same PDS family. |
| WisdomTree, 12 assets | 7.0M tokens ≈ $40M (rwa.xyz) | the issuer publishes a machine-readable daily NAV **and** the Stellar issuer per fund at `dataspanapi.wisdomtree.com/funddetails/{nav,blockchain_addresses}/?ticker=WTGXX` (`{"dt":"2026-09-16","nav":1.0,"sharesOutstanding":1230403338.34}`; Stellar address matches our WTGX issuer exactly) — read from a browser. Cloudflare returns 403 to every non-browser client, from here and from r1, so the sync cannot read it without impersonating a browser, which this project will not do. **Needs Ash:** ask WisdomTree for API access, or accept SDF's prices for these twelve via the curated arm. WTGXX is a stable-NAV MMF (1.00 daily); the other eleven float. |
| Tradable, 24 private-credit contracts | 548.1M tokens | supply served; the platform publishes deal sizes and fill %, **no per-token value** (tradable.xyz, doc.tradable.xyz). Only par (1.00) exists, which is what the third party uses. **DECIDED 2026-10-02: no `stated_par` basis.** Par on private credit is the face value of a loan, not a statement of what the token is worth: no regulation fixes it (unlike the CNAV arm's prospectus NAV) and nothing re-reads it when a deal impairs. The 24 stay supply-only (served via `/v1/assets/{contract}/supply`). No directory names them, so they are outside the verified RWA set: they add $0 there and are not counted in `assets_unvalued`, and the served verified figure sits below the third party's by this line. The curated arm still carries them at par once its key is set, under the curator's own provenance: as `curated_assets` rows (counted in `curated.additional_value_usd`) when `curated.status` is `served`, or inside `curated.published` when it is `published_totals`. Reopen only with a per-deal value the issuer publishes. |
| Realiz VuMe Bond 2030 (TPT30, ISIN CH1509100140) | 500M tokens | refused on the contract's own facts (see the corrected line below); rwa.xyz and Dune both carry it at NAV $1.00 from the issuer. Curated arm only. |
| RedSwan (real estate) / long tail | ≈ $72M / ≈ $241M | scam-flagged class / no primary-source bindings — unchanged. |

Arithmetic: verified $2,535.8M (2026-09-16) + $57M today = **≈ $2.59B on prices
of our own**; + WisdomTree $40M + sgBENJI $25M once their NAVs can be fetched =
≈ $2.66B. The remaining ≈ $1.4B to the third party's figure is par-valued
private credit and one refused bond — reachable only through the curated arm
(SDF's own prices), never through a measurement; a par policy was declined (Tradable row). That is the
honest ceiling of "independent", and it is written here so it is not re-derived.

> **v0.90.0 LIVE — measured 2026-09-18 00:25–00:45 UTC (every region; explorer
> deployed too).** Served RWA reference total **$2,608,585,915.48** across
> **32 assets / 21 issuers** (from $2,529.9M / 29 / 18): gBENJI priced at the
> prospectus NAV ($56.68M, `prospectus_constant_nav`), grBENJI admitted through
> the sibling arm and priced ($0.59M), sgBENJI admitted unpriced (VNAV, needs a
> live NAV); `refused[no_real_world_instrument_basis]` 102,303 → 102,297.
> The curated arm serves **`curated.status: published_totals`** —
> the curator's monthly total **$4,004,795,860.08** (month_end 2026-09-15,
> executed 2026-09-17T02:58Z) with its subclass split (Active Strategies
> $1.18B, US Treasuries $1.14B, Private Credit $0.59B, Public Credit $0.50B,
> …) and the signed gap to our verified total, read daily from Dune's public
> query results at 683 datapoints a run (≈ 0.07 credits). The cohort endpoint
> answers in **1.5 s** (was pinned at 8.0 s) and labels 8 of GDB3RSSW…'s 14
> contracts with a protocol. `/v1/ohlc` at 2h/12h/3d/2w answers 200
> (W8-17/W8-20 closed). Test nets: movements archive derived to the tip on
> both (testnet 27.9M rows → tip 4,733,690 after one backfilled ledger;
> futurenet 158k → 613,991); created cohorts serve (testnet 1,955 roots /
> 3.81M members). Issues #512 #513 #514 #515 #516 #518 #519 #520 #521 closed
> with evidence (#517 — the curated arm's classic-row drop — is unrelated to
> this window; closed separately in W3 slice C03, PR #1428, 2026-09-24).
> SDEX history: the chunk [61249957,61289956] retried on the
> sub-batched writer and hit a second wall — every sub-batch into a
> compressed `trades` chunk fails `SQLSTATE 53400 tuple decompression limit
> exceeded` and drops to per-row again; fixed in v0.91.0 (the batch upsert
> lifts the cap with `SET LOCAL` in its own transaction, as the COPY and
> restamp writers do). The driver is paused (stop file) until v0.91.0 lands.
>
> **v0.91.0 (in clearance at the time of writing):** month-priced cohort
> flows (`inflow_usd_then` / `outflow_usd_then` / `price_usd_then` from the
> index's own monthly USD VWAPs, ClickHouse `asset_month_usd_prices` loaded
> each cohort cycle; explorer "USD today / USD then" toggle); a standing diff
> of `sdf_reserve_accounts` against the list SDF publishes (C4-069); one
> token-decimals resolver with market-cap/FDV refused on a cross-scale
> mismatch (C1-050); drift-guard follow-ups; the R4 methodology and unit
> comments aligned with the code; the decompression-cap fix above.

> **CORRECTION + STATE, 2026-09-18 00:30 UTC — the two v0.89.2 arms had
> zero live effect, the Dune arm could never load, and both are fixed in
> the release after v0.89.3.** Read this box before the table above it.
>
> **1. The Franklin row above said PRICED; it was not.** Live after
> v0.89.3: 29 assets / 18 issuers / **$2,529,914,368.49**, no
> `prospectus_constant_nav`, no `…_via_domain_sibling` row. Root cause
> (confirmed by an independent skeptic): `rwaCandidateFilter` →
> `rwa.CouldQualify(code, type)` ran INSIDE the storage scan and never
> received `anchor_asset`, so every `anchor_asset_type="other"` + ISIN
> declaration was dropped as `EntriesFiltered` before the ISIN arm, the
> sibling arm or the CNAV reference ran. Both arms' unit tests passed
> because they called `admitClassicCandidates` directly. Fixed in
> `fix(rwa): admit ISIN-declared classic entries past the scan pre-filter`
> (a production-path test now enters through `buildRWAClassicMembership`).
> Expected live delta on deploy: assets 29→32, issuers 18→21,
> `refused[no_real_world_instrument_basis]` 102,303→102,300, ≈ +$57M.
> Also landed beside it: #514 (SAC-listed classic members take their
> listing price), #520 (`recognition` documented on classic rows), #521
> (CNAV bindings carry a 90-day `ReviewBy`; the IB class cites its own page).
>
> **2. The Dune curated arm (v0.88.1) can never load a row as designed.**
> The maintainer's key (`~/.si-secrets/DUNE`) is installed on r1
> (`/etc/default/curated-rwa-sync`, 0600). The first keyed run was refused
> for asking Dune for a `small` tier (fixed: `medium`). The second finding
> is structural: the dashboard's two CSV uploads
> (`dune.stellar.dataset_recognized_assets`, `dataset_asset_prices`) are
> PRIVATE to the uploading team — `sql/execute` over them fails "does not
> exist or it is private" from any outside account, on any plan. What a
> free key CAN read is the latest result of the dashboard's public queries
> (`GET /api/v1/query/{id}/results`, billed by datapoint): 6961845 monthly
> RWA total (headline **$4,004,795,860**, month_end 2026-09-30 row, refreshed
> daily), 6961847 monthly split by asset subclass, 6962311 total supply;
> 6962001 (stablecoins) is private; no public query exposes per-asset rows.
> The arm is being re-pointed at those results (`curated.published{total_usd,
> as_of, by_subclass, series, gap_vs_verified_usd}`, migration 0162) — in
> verification at the time of writing. Consequence for this section: the
> per-asset comparison with the third party is not obtainable from Dune;
> the number we are measured against is.
>
> **3. Cohort pages: the endpoint pinned at 8 s and labelled nothing.**
> `GET …/graph/cohort` priced up to 400 holdings through serial
> `LookupUSDPrice` reads inside the one 8 s budget, labelled contracts on a
> dead context, and the contract→protocol index cached a cancelled build
> for 10 min. Fixed (label before pricing, top-50 price cap with
> `valuation.unpriced_over_cap`, index built detached with prewarm, exact
> big.Rat dollars per #516).
>
> **4. Test nets had an EMPTY movements archive.** `cap67-movements` with
> `-floor-ledger 1` waited forever for ledger 1 (lake min is 2). Unblocked
> 2026-09-17 20:50 UTC by seeding the watermark at 1 on both test nets
> (testnet derived 1.8M rows in its first minute; futurenet reached its tip
> 423,121). The code fix (clamp first-run start to the lake's min ledger;
> creators rollup boundary from config) is in flight. Interior holes
> (testnet first at 4,542,820; futurenet 423,122) will stall the daemon
> again: ch-live-catchup is OFF on test nets by design (no LIVE_ERA_FROM),
> so each hole needs one `ch-backfill`.
>
> **5. Other rows closed today (verified by an independent verifier each):**
> `/v1/ohlc` 500 at 2h/12h/3d/2w (W8-17/W8-20 — one interval ladder now
> drives validation, routing and the fold allow-list); W8-19 (every
> `refresh_continuous_aggregate` CALL bounded by a window-derived
> `statement_timeout`); #519 drift classifier per file type; #513 SLA-proof
> n/a cells; #518/#512 ops-config truth (curated-sync unit header, one key
> mechanism, `min_wal_size` back to 512MB — NOT applied to r1, an effective
> diff with the restart handler behind it); #515 drift guards; restore-drill
> evidence banner; the July tail-triage working doc re-verified locally
> (79 rows: 57 FIXED, 13 → row 1.2, 7 carried below, 2 dropped).
>
> **6. Carried out of the tail-triage pass (owner: agent unless noted):**
> C1-041 residual — `sep41_total_only` missing from the `supply_basis` spec
> enum since v0.21.0; C6-081 — six Dockerfiles `FROM` by tag, not digest;
> ~~C2-038/C4-086 — the PG pipeline sink's undrained-on-exit is log-only
> (counter + alert, like #368's CH half)~~ **fixed** for on-chain trades: once
> the producer has stopped, shutdown rewinds the ledgerstream cursor below the
> lowest abandoned trade; ledger-less rows stay counter + ERROR; C6-056 — ADR-0011 lacks the
> amendment for the diagnostic-only over-mint leg; C2-049 — ~~the chainlink
> source takes feed decimals from config and never reads `decimals()`~~
> **FIXED `8bb7095a1` (v0.90.0): `decimals()` is read on-chain and a mismatch
> is refused + alerted; r1 2026-09-30 shows 0 mismatches on 6 live feeds**; C4-069 —
> `sdf_reserve_accounts` has no list-level diff against SDF's published
> list (2% value cross-check only); C1-050 — aggregator and API resolve
> token decimals independently, market-cap/FDV computed regardless; C1-022
> — no depeg band on the fiat:USD stablecoin-proxy path, blocked on a
> USD-quoted stablecoin source (none exists — **the maintainer** picks one).
>
> **7. Local-toolchain notes that cost an hour:** `verify.sh` compares the
> lockfile mtime to `node_modules/.modules.yaml` and `make bootstrap-worktree`
> does not refresh it (run `pnpm --dir web/{explorer,status} install
> --frozen-lockfile`); local gitleaks 8.30 flags three fixtures the pinned
> 8.21 does not (allowlisted with `Baseline-Growth` trailers); the agent
> runner's worktrees under `.claude/worktrees/` are 43 nested checkouts /
> 14 GB — excluded from the scan by config, not yet pruned.

**SDEX trade history (#349, chunk 1) — RESUMED 2026-09-17 16:52 UTC.** The
reverse-chronological `ch-rebuild -sources sdex -sdex -write` re-derive that
someone had been running by hand in 40k-ledger chunks (2026-09-12 → 09-16,
~5 h per chunk on the per-row writer) died in the 2026-09-16 Postgres outage
mid-chunk [61249957,61289956] (579,533 rows failed to write, "context
canceled"). `trades` for `sdex` now reaches back to ledger 61,249,957
(2026-02-15) from the 61,609,957 floor the issue recorded. It is now driven by
`/usr/local/sbin/sdex-history-driver.sh` as the transient unit
`sdex-history-driver.service` (EnvironmentFile=/etc/default/stellarindex,
Nice=10, idle IO): re-runs the killed chunk first (the upsert path is
idempotent; `-bulk-trades` falls back to it on a non-empty range and says
so), then descends 40,000 ledgers at a time with `-bulk-trades`, each chunk
under `run-heavy-job.sh` (singleton lock, 20G cap), one retry per chunk, and
stops on its own when `/var/lib/stellarindex/sdex-history.stop` exists, when
the Postgres volume has < 300G free (2.7T free at start; `trades` is 95 GB),
or at the floor 50,746,445 (2024-03-11, the first on-chain AMM trade). Next
chunk's upper bound lives in `/var/lib/stellarindex/sdex-history.next`; the
log is `/var/log/stellarindex/sdex-history.log`. Chunks written before
`ch-rebuild -write` recorded a projection dirty window (it now records one
per re-derived source before it writes) left the completeness verdict
carrying its prior claim over those ranges.

**Sponsor / creator cohort pages — SHIPPED 2026-09-17 (post-1.0 item, done
early because it was asked for by name).** `/insights/sponsors/{g}` and
`/insights/creators/{g}` now read `GET /v1/accounts/{g}/graph/cohort?relation=`
— what the accounts that address created or sponsored went on to hold and
do: current holdings per asset valued at the live rate (pool shares served
as holdings, never priced), monthly inflow/outflow per asset with the
month's movement count and distinct active members, the C… contracts the
cohort moved value through (labelled from the protocol roster), members
active in the last 30/90/365 days, and DeFi positions from the six
per-protocol folds. Behind it: `stellarindex-ops ch-cohort-rollup` +
`cohort-rollup.timer` (daily), 16 `account_cohort_*` / `defi_position_holders*`
tables (DDL applied on r1 by hand 2026-09-17 — idempotent, in
`tier1_schema.sql` and mirrored with a runbook in
`deploy/clickhouse/account_cohort_rollup.sql`). Coverage floor: every
sponsor, creators with ≥ 10 accounts; below it the API says `covered: false`.
**Measured before the first cycle (r1, 2026-09-17):** membership fill 11 s
(23.8M rows, 19.2M distinct accounts). One 1M-ledger movements window near
tip is 657M rows; the board rollups' argMax GROUP BY de-duplication
exceeded the 8 GiB budget on it at 2 AND 6 threads, so the walk reads the
archive with `FINAL` at 6 threads — 151 s / 2.7 GiB for that window,
~36 min projected for the 10.4B-row archive. **First cycle, r1, 2026-09-17
15:38–16:26 UTC: 48m03s** — the movements walk 46 min over 65 windows
(FINAL at 6 threads), every fold under two minutes. Served: 1,486 sponsors +
26,804 creators; 4.34M holdings rows, 23.5M monthly-flow rows, 260k contract
counterparties, 315k DeFi position rows over 186k position holders. The top
sponsor's cohort is 785,652 accounts (5,163 live, 4,148 active in 30 d), 65
months of flows, 14 contracts, and the endpoint answers keyed. Daily via
`cohort-rollup.timer`; the test nets cycle in seconds (empty cohorts).

### Live findings — r1, 2026-09-04 evening (alert board read directly)

> **Re-read 2026-09-17 19:10 UTC (after v0.89.1):** the board carries ONE
> alert — `stellarindex_deadmansswitch` (informational, by design). Cleared
> today: `stellarindex_node_root_disk_warning` (root 80% → 70%: a 2.6 GB
> ClickHouse apport dump from 2026-09-10 and a 2.3 GB Go module cache removed),
> `stellarindex_assets_popular_priceless` (yBTC's SAC traded on aquarius while
> the price lived under its classic id — the tripwire now aliases SAC → classic,
> v0.89.1), and `stellarindex_curated_rwa_sync_stale` (a keyless run now stamps
> a `refused` gauge instead of failing). Pending on purpose:
> `stellarindex_curated_rwa_sync_refused` (2 h `for`, reset by each rules
> reload) will ticket until `DUNE_API_KEY` is set on r1. Everything below this
> box is the 2026-09-04 read and is superseded where it disagrees.

Six alerts are active on r1 and none of them is `oracle_stale`, which row
1.1 still names as the only non-heartbeat alert firing. That sentence is now
out of date; the board reads:

- **`stellarindex_aggregator_supply_refresh_error_dominant`** (ticket,
  pending, AQUA) — **a real and previously unrecorded condition.** The
  refresher stamps a snapshot at the freshest ingestion cursor and then
  fails closed if that ledger has no `stellar.ledgers` row in the lake
  (`no_ledger`, deliberate: a wall-clock stamp would corrupt point-in-time
  supply). But the cursor is *structurally* ahead of the lake — measured
  now, `ledgerstream` reads **64,274,510** while `max(ledger_seq)` in
  `stellar.ledgers` is **64,274,509** — so a refresh that lands in that
  one-ledger window always misses. Fleet-wide that is **9.9% of ticks over
  2 h** (`ok` 1,082, `no_ledger` 119, `dormant` 0), and it is bursty rather
  than spread: **40 assets** were over a 40% non-ok rate in the same 10 min
  window, because when the lake lags every watched asset fails together.
  Each miss only skips a tick, so supply is late rather than wrong — but the
  per-asset alert F-1320 introduced now tickets on it, and it will keep
  doing so. The fix is to pick the snapshot ledger as
  `min(freshest cursor, max(stellar.ledgers))` rather than the cursor alone,
  which keeps the fail-closed property and removes the self-inflicted race.
- **`stellarindex_ingestion_oracle_unknown_symbols`** (ticket, since
  00:07Z, source `redstone`) — **the feed is `earnUSDC_FUNDAMENTAL`, and
  nothing was lost.** The counter has no `symbol` label, but the identity is
  recoverable from the record layer, which is the whole point of recording
  the miss verbatim: `SELECT asset, source, count(*) FROM oracle_updates
  WHERE asset LIKE 'raw:%'` returns `raw:earnUSDC_FUNDAMENTAL | redstone |
  5`, first seen 2026-09-04T00:07:10Z, most recent 12:58:40Z. Per
  `resolveFeedEntry` an unregistered `feed_id` is written at its own vector
  position as a `raw:<feed_id>` row rather than skipped, and a later
  registry entry re-derives the same primary key and promotes the row in
  place — so **no replay is owed here**, unlike the unrepresentable-feed
  case, which has its own counter precisely because that slot IS a hole.
  What is owed is a `feeds.go` registry entry naming the canonical asset
  `earnUSDC_FUNDAMENTAL` maps to, which is a mapping decision rather than a
  code change; until it exists the feed is captured but not priced.

  **What the feed is, so the decision is one step rather than an
  investigation.** Six raw rows between 2026-09-03T12:07Z and
  2026-09-04T12:58:40Z, values rising monotonically 71,763,347 →
  71,775,467. That is +0.0169% over 1.036 days, or **6.13% annualised**,
  which is a yield-bearing wrapper's net asset value and not a price
  feed. The registry already carries that shape: `_FUNDAMENTAL` entries
  (`internal/sources/redstone/feeds.go`) are the underlying value of a
  wrapper, and the existing ones split two ways —
  `SolvBTC_FUNDAMENTAL` is `mustCrypto` quoted in BTC, while
  `BENJI_ETHEREUM_FUNDAMENTAL` is `mustRWA` quoted in USD. So the open
  questions are exactly three, and none of them is answerable from the
  wire: which canonical id this maps to, whether it is quoted in USD or
  in USDC, and its decimal scale — the raw integer reads 0.71763347 at
  1e8 or 71.763347 at 1e6, and a yield-bearing USDC share sitting below
  one dollar would be unusual enough to want confirming rather than
  assuming. Deliberately NOT guessed here: this is a price surface, and
  a wrong scale is a wrong price rather than a missing one.
- **`stellarindex_stellar_stack_lagging`** ×2 (ticket, sustained since
  2026-09-02T14:02Z, `archivist` and `galexie`, lag = 1) — **unrelated to
  the ledger finding above, despite the name.** The series is
  `stellarindex_stellar_stack_version_lag`: the probe compares the
  dpkg-installed package against the apt candidate, so `lag = 1` means one
  package version behind upstream, not one ledger. Both have been one
  version behind since the labelset fix of 2026-09-02 made this alert
  capable of firing at all. `archivist` is the archive mirror and verify
  CLI, not in the live ingest path, so pinning
  `stellar_archivist_version` and applying is low-risk and needs no
  restart; `galexie` is in the ingest path and wants the usual care.
- **`stellarindex_ch_schema_snapshot_offsite_stale`** (ticket) — expected;
  it is row 1.12 ticketing until the off-site target is set.
- **`stellarindex_deadmansswitch`** (informational) — fires constantly by
  design; the alarm is when it stops.

Nothing on the board is a page, and `amtool silence query` is empty — no
condition is being suppressed.

### Corrections to this document, verified on 2026-09-03

- **W8-1a, W8-6c, W8-9c, W8-14a are all FIXED.** All four are recorded below
  as "CONFIRMED-OPEN". Both `verify-archive` tiers run daily with zero
  mismatches.
- **W2's r1 `sac_wrappers` gate is CLOSED** — `[supply.sac_wrappers]` on r1 is
  populated (USDC, AQUA, yXLM, BLND).
- **"Runbooks: 79 of 149 more than 90 days stale" is wrong** — 161 of 162 are
  within 90 days.
- **W4 failed units, the W4.1 drill, D8 SolvBTC, C2-016 and C3-030/031 are
  done.**
- **W5.2 (dfees) is COMPLETE.** This document says so in one place and
  "genuinely unstarted" in another; the first is right. **W5.8** has the same
  self-contradiction and #803 is open.
- **#371 F2 is complete, not metric-only.** `stellarindex_dependency_up` is
  exported and `stellarindex_dependency_down` is a page rule in BOTH trees
  with a runbook. An audit nearly re-filed this as a high because ClickHouse
  is not a scrape target and no rule string-matches it.
- **`docs/operations/drills/restore-drills.md` is stale BY CONSTRUCTION.**
  BDR-03 moved the evidence write onto the box, so the in-repo log stops at
  "Run 5 — 2026-07-03" while four later passing runs exist only on r1.
  Anyone reading the repo concludes the drill has not run since July.
- **`docs/audit/audit-2026-07-23/tail-triage-2026-07-26.md` holds ~50 OPEN
  rows with no home in this plan.** Several spot-checked rows are already
  fixed. Reconcile it once and close it, or carry its live rows here.
  *Correction 2026-09-17:* that file is a gitignored, local-only working doc
  (`.gitignore` "LOCAL-ONLY: audit/remediation findings"; its own header says
  "do not commit"). Its own tally — derived on 2026-07-26 against `23a75582`, a
  pre-rewrite commit id (see the note at the top of this plan), and not
  re-derived since — is 61 OPEN-REAL findings (50 distinct after
  duplicates) plus 15 OPEN-ACCEPTED-RISK candidates. A docs commit cannot
  reconcile it — the pass has to run in the local checkout, and only its
  CARRIED rows can land here, as plan rows or GitHub issues.
- **This document is 4,086 lines and roughly 70% of it is superseded
  history**, with at least four places where two non-superseded boxes
  disagree. Before v1 it is worth cutting to the ~200 outstanding lines and
  archiving the rest.

---

## What "v1 launch" means

Public, announced availability of the Stellar Index API + explorer as a
production service fit to present to Stellar: substrate certified complete,
served money-values proven correct, a signed repeatable deploy path, honest
capacity/HA posture, paging wired to a human, and the v1.0 wire shape frozen
(ADR-0042 — **Accepted and implemented**; the `kind` discriminator is live in
the spec, so the wire-freeze prerequisite is met).

## 0. Verified current state — REFRESHED 2026-10-06 against the inventory

Each row points at live items in the private inventory
(`inventory/items.jsonl`, snapshot `323689bf`, 2026-10-06 01:49 +01:00).
The item is the authority; this table is an index. A row goes when its last
item closes. The 2026-07-29 snapshot this replaces cited no items; it is in
git history.

§0 (dated 2026-10-06) overrides THE PLAN (2026-09-03) and every older row where they disagree.

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

## 1. Go-live gate (all must be true)

> **SUPERSEDED for planning by “THE PLAN” at the top of this file
> (refreshed 2026-08-15).** Kept for its rationale and execution recipes.
> Where this section and THE PLAN disagree on what is still outstanding,
> THE PLAN is right.


- [x] ✅ **Supply trustworthy** — DONE 2026-07-30: full reconcile vs
      Horizon **8/8 PASS** (evidence [2026-07-30-supply-reconcile-8of8.md](https://github.com/Stellar-Index/StellarIndex/blob/0023bb9aefa96fb8231d9eabd160e6133eca39e9/docs/operations/evidence/2026-07-30-supply-reconcile-8of8.md));
      the claimable seed, SAC full-history seed, and dormancy-anchor fix
      all landed. Residual: `supply_cross_check_divergence` ×3 (PHO/BLND/
      EURC, partial_wrap) dispositioned — our values Horizon-verified;
      external references count wrapped supply differently. Pre-launch:
      convert dispositions to annotated silences.
- [x] ✅ **Completeness green — 17/17, DONE 2026-08-01 08:33Z, first
      time ever** (evidence [2026-08-01-completeness-17of17.md](https://github.com/Stellar-Index/StellarIndex/blob/0023bb9aefa96fb8231d9eabd160e6133eca39e9/docs/operations/evidence/2026-08-01-completeness-17of17.md)).
      Every source `complete=t` AND `lake_complete=t`, publicly served
      on `/v1/coverage`, with no carried claims — the final three
      (redstone/soroswap/aquarius) re-verified full-range within 24h
      and replay rewinds now force re-verification (dirty windows,
      proven in this very verdict).
- [x] ✅ **Prove-it battery passed** (§2.6) — every row filed as of 2026-07-30: reconcile-balances (0 mismatches), verify-lake/contiguity/hash-chain (genesis→tip), re-derive determinism (byte-identical), price vs 5 references (22/22), supply vs Horizon (**8/8**), `verify-usd-volume` calibrated (current pipeline exact; pre-07-23 re-stamp queued as data-quality follow-up). Remaining battery-adjacent: the SEV drill (paging-gated).
- [x] ✅ **Config codified = live** — DONE 2026-07-27. The 69-task apply
      landed (`ok=259 changed=60 failed=0`) and **`ansible-drift.yml` is
      GREEN** (run 7; the first fully-green verdict this repo has ever
      produced). Getting there fixed 6 real defects: a CI-lockout from
      the exclusive-keys apply, a stale-galexie-binary near-miss, and 4
      idempotency bugs; the drift baseline SHRANK 3 → 1 entry.
- [ ] **Security posture**: creds rotated (ratesengine-admin, MinIO, anything session-exposed); approval gate re-armed; accepted-risk list explicitly signed; external security review booked/closed [OP].
- [ ] **DR honest**: off-site backup decision executed or explicitly
      risk-accepted [OP]; ~~restore-drill timer re-enabled~~ ✅ **DONE
      2026-07-27** (its capacity gate cleared: pool 94%→85%, 2,657 GB
      free vs the 200 GB floor; enabled + codified `205b041a`, next
      fire 2026-08-01); ZFS trim snapshot resolved (auto — §2.5 gate).
- [ ] **Launch mechanics**: ✅ `auth_mode=apikey_optional` VERIFIED live
      2026-07-27 (healthz + price both 200 unauthenticated); ✅ status
      page (301→/status/), explorer, docs, /methodology, /operations,
      /diagnostics all 200; ✅ **SLA definition PUBLISHED** — `/sla` was
      **404**; the four targets existed only in internal ops docs and
      are now a public page with the error budget and explicit
      exclusions (`535c7bcc`). Remaining: announcement ready;
      first-24h watch staffed [OP].
      ✅ **Export 429 fragility FIXED 2026-07-28** (`3422b150`). A 429 no
      longer spends a transport attempt — throttling has its own budget
      (8 waits), prefers the server's `Retry-After` (capped at 60 s so a
      bad value cannot stall a build for hours), else exponential to a
      30 s cap with jitter. ~2 min total patience, longer than the
      anonymous tier's window, so a build rides the window out instead of
      dying inside it. Chose backoff over the other two candidates: an IP
      exemption is infra that would also mask a real limiter regression,
      and lowering concurrency just makes every build slower to dodge a
      problem that is really "we did not wait".

---

# Forwarding sections (INV-0897 PR 1, additive)

Everything above this line is the pre-cut plan and is being deleted in stages (INV-0897). The sections below are where live items, code and docs are forwarded. Nothing above was edited.

## Cut: header

- Pre-cut sha: `52aacb972a5be5fe65e9d608227e8fe06dfe7fe2`. Read the full old plan with `git show 52aacb972:docs/operations/v1-launch-plan.md`.
- Every old `:NNN` line cite and `#L<n>` inventory pointer resolves at that sha, not at HEAD. Migrations 0051, 0106 and 0107 cite `:2486`: that means the "no unbounded trade-scan queries" rule, now in [domain-traps.md](../architecture/domain-traps.md#unbounded-trade-scans-cancelled-refreshes-and-aggregation-pitfalls).
- Old row ids (1.x, W-x, D-x) cited by code resolve in "Cut: cited rows" and "Cut: decisions of record" below.
- Commit ids older than the 2026 history rewrite may not resolve; find the successor with `git log` by date and subject.
- Go-live gate and launch sequence: §0 above is the gate (an index of INV items), and the launch sequence is the pre-cut §2.8 (PRE-CUT lines 4610-4636; step 1 is in "Cut: moved evidence") plus INV-2707, INV-2708 and INV-2709 below.

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
| W5.4 [1437] | Reset of the 13 supply rollups: `supply verify-rollup` on r1 verified clean 2026-09-17 (0 drift, tolerance 0); no reset needed (see the W5.4 line above, :228) | n/a |
| W8-12 [1738] | LP reserves are live-only from ledger 63,300,828; ACCEPTED 2026-10-02, no backfill ([supply-pipeline.md](../architecture/supply-pipeline.md#lp-reserve-history-cutoff)) | open-fixes-inventory-2026-08-08.md:23 |
| W8-17, W8-20 [1786, 358] | `/v1/ohlc` 500 at 2h/12h/3d/2w; one interval ladder (`AllHistoryGranularities`) now drives validation, routing and the fold allow-list. CLOSED | aggregates.go:2381, ohlc_routes.go:44, ohlc_routes_test.go:21, ohlc_intervals_test.go:20-21, test/integration/ohlc_fold_intervals_test.go:59 |
| W8-19 [1807] | A single `refresh_continuous_aggregate` call needs a timeout bound: `CAGGRefreshTimeout` 5 min per hour of window, floor 10 min, ceiling 4 h | cagg_refresh_timeout.go:16 |
| L4.14, L4.15 [4738] | R2/R3 are not provisioned, so `flags.reduced_redundancy` has no producer (ADR-0017); carried to W9, gated on D2 (INV-2710) | runbooks/verify-archive.md:131 |
| CS-102 [2636] | Quiet assets anchor on the observer watermark; see [supply.md](runbooks/supply.md#cs-102-quiet-is-not-stale) | runbooks/supply-assets-stale.md:123 |
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
