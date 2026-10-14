---
title: Runbook — supply
last_verified: 2026-10-06
status: living
severity: P2
---

# Runbook — supply alerts

Supply alerts: cross-check, divergence, refresh, snapshot and verify-rollup.

## At a glance

- [`stellarindex_supply_cross_check_divergence`](#stellarindex_supply_cross_check_divergence)
- [`stellarindex_supply_cross_check_unevaluable`](#stellarindex_supply_cross_check_unevaluable)
- [`stellarindex_supply_divergence_high`](#stellarindex_supply_divergence_high)
- [`stellarindex_aggregator_supply_refresh_stalled`](#stellarindex_aggregator_supply_refresh_stalled)
- [`stellarindex_aggregator_supply_refresh_never_initialized`](#stellarindex_aggregator_supply_refresh_never_initialized)
- [`stellarindex_aggregator_supply_refresh_error_dominant`](#stellarindex_aggregator_supply_refresh_error_dominant)
- [`stellarindex_aggregator_supply_refresh_dormant_fleet`](#stellarindex_aggregator_supply_refresh_dormant_fleet)
- [`stellarindex_sep41_supply_rollup_no_cursor`](#stellarindex_sep41_supply_rollup_no_cursor)
- [`stellarindex_ch_supply_gapfill_failed`](#stellarindex_ch_supply_gapfill_failed)
- [`stellarindex_supply_snapshot_unit_failed_alert`](#stellarindex_supply_snapshot_unit_failed_alert)
- [`stellarindex_supply_snapshot_stale`](#stellarindex_supply_snapshot_stale)
- [`stellarindex_supply_snapshot_critical_stale`](#stellarindex_supply_snapshot_critical_stale)
- [`stellarindex_supply_snapshot_never_initialized`](#stellarindex_supply_snapshot_never_initialized)
- [`stellarindex_supply_snapshot_circulating_zero`](#stellarindex_supply_snapshot_circulating_zero)
- [`stellarindex_supply_verify_rollup_stale`](#stellarindex_supply_verify_rollup_stale)
- [`stellarindex_supply_verify_rollup_unit_failed_alert`](#stellarindex_supply_verify_rollup_unit_failed_alert)


## stellarindex_supply_cross_check_divergence

Severity P3 (ticket). Rules: `deploy/monitoring/rules/supply.yml` + `configs/prometheus/rules.r1/supply.yml`. Impact: the asset's `total_supply` / `circulating_supply` / `market_cap_usd` / `fdv_usd` on `/v1/assets/{id}` are wrong by the divergence until reconciled; aggregate price endpoints are unaffected.

Symptom: `stellarindex_supply_cross_check_divergence_stroops{classic_key="...",wrap_class="..."} > 1` for 5 min. `classic_key` is `CODE:ISSUER`; `wrap_class` is `partial_wrap` (default) or `full_wrap` (operator-attested via `[supply].fully_wrapped_sacs`, none configured).

What is compared ([`supply-pipeline.md`](../../architecture/supply-pipeline.md)):

- `partial_wrap`: Algorithm 3 (SEP-41 event sum, SAC `total_supply`) must not exceed Algorithm 2 (classic total). Two legs are computed by `CrossCheckSubsetBound` in `internal/supply/crosscheck.go`; only leg 2 feeds the gauge.
- Leg 1, over-mint (`sac_total > classic_total`, field `OverMintStroops`): diagnostic only, cannot page. Legitimate after a classic-side retirement (BLND) or a one-time SAC mint distributed classically (PHO).
- Leg 2, escrow-exceeds-minted (`SACWrapped > sac_total`, field `EscrowExcessStroops`): pages. Means SEP-41 event capture for that SAC is incomplete or a burn was double-counted.
- Leg 2 reports `subset_bound_checked=false` when the classic snapshot has no `sac_wrapped_stroops` (pre-migration 0117). That is UNCHECKED, not green; it clears within one refresh cycle.
- `full_wrap`: totals must be equal within 1 stroop. Under-counted `SACWrapped` (the BLND / EURC / KALE / PHO case below) passes leg 2 quietly.

### Check

```sh
# 1) Which asset is diverging.
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_supply_cross_check_divergence_stroops' \
  | awk '$NF != "0" && $NF != "1"'

# 2) Both readings + their basis.
psql -d stellarindex -c \
  "SELECT asset_key, time, total_supply::text, basis, ledger_sequence
     FROM asset_supply_history
    WHERE asset_key IN ('USDC:GA5Z...', 'CCW6...')
    ORDER BY time DESC LIMIT 4;"
```

Pairs are classic assets in `watched_classic_assets` whose `sac_wrappers` contract is also in `watched_sep41_contracts` (others log `sac_wrappers entry NOT cross-checked` at startup). An unevaluable pair has its series deleted and fires [`unevaluable`](#stellarindex_supply_cross_check_unevaluable).

### Likely causes

- `partial_wrap`, leg 2: Algorithm 3 missed mint events or double-counted a burn. Follow [`sep41-mint-recovery.md`](../sep41-mint-recovery.md).
- `partial_wrap`, SAC > classic with `escrow_excess_stroops` 0 cannot page; if seen, the metric computation regressed (check `CrossCheckSubsetBound`).
- `full_wrap`, classic > SAC: Algorithm 3 missed mints; replay the SAC's event range. Classic < SAC: Algorithm 2 missed a trustline / claimable / LP change.
- Both readings stale (aggregator stalled): see `stellarindex_aggregator_silent` in [`aggregator.md`](aggregator.md).

### Fix: dormant pool-held SAC balances (BLND / EURC / KALE / PHO)

Standing cause for these four, and a real served-supply understatement. Phoenix / Blend pool contracts hold SAC balances written before ledger ~62,000,000; the default `seed-sac-balances` reads `stellar.ledger_entries_current`, which never saw them, so Algorithm 2 is short. Algorithm 3 is correct. Do not write a new reader (detail: [`supply-pipeline.md`](../../architecture/supply-pipeline.md), "Dormant contract-held SAC balances").

1. Check whether the pair was ever fixed. A `full_history` row with `lake_verified_through` NULL was written before migration 0183 and is NOT evidence: re-seed it. With it set, `min_ledger_seen` well below 62,000,000 shows the floor was reached.

```sql
SELECT contract_id, asset_key, source, holders_seeded,
       holders_retracted, lake_verified_through,
       min_ledger_seen, max_ledger_seen, seeded_at
  FROM sac_balance_seed_provenance
 WHERE asset_key LIKE 'BLND:%' OR asset_key LIKE 'EURC:%'
    OR asset_key LIKE 'KALE:%' OR asset_key LIKE 'PHO:%'
 ORDER BY asset_key;
```

2. Re-seed from the complete append-log (`stellar.ledger_entry_changes`). It is heavy and must run under the wrapper; dry-run first.

```sh
run-heavy-job.sh supply-seed-sac-balances \
  stellarindex-ops supply seed-sac-balances \
  -config /etc/stellarindex.toml -full-history -dry-run
run-heavy-job.sh supply-seed-sac-balances \
  stellarindex-ops supply seed-sac-balances \
  -config /etc/stellarindex.toml -full-history -write
```

3. Notes on the run: add `-contracts <id>,<id>` (ids from `[supply.sac_wrappers]`) to re-seed only some wrappers (prints a `PARTIAL` banner). The walk is silent for about an hour; progress is the `ops_job="supply-seed-sac-balances"` heartbeat. Printed `sum=<stroops>` per contract should rise by the dormant holding. Removed holders are written as zero-balance tombstones (`retracted=`), which clears phantom rows without a DELETE.
4. Errors: `lake is not intact over the range it would reduce` names the first missing ledger and nothing was written; heal it (`ch-live-catchup` near tip, `ch-backfill` below) and re-run. `matched no Balance entry` names a `[supply.sac_wrappers]` id that found nothing; check it. `neither re-seeded nor retracted` means old holder rows are still served; escalate, do not re-run.
5. After the next refresher tick the gauge drops to 0. If it does not, and provenance shows a genuine `full_history` seed below the floor, treat it as fresh corruption (generic path). Record the outcome per asset in [`audit-remediation-operator-actions.md`](../audit-remediation-operator-actions.md).

### Fix: generic path

1. Find the wrong side. Exit is non-zero on out-of-tolerance, on `status: UNCHECKED` (no `sac_wrapped_stroops`; re-run after a newer classic snapshot), and on `status: MISALIGNED` (snapshots more than 1000 ledgers apart, `CrossCheckLedgerTolerance`; the stalled side named by `primary_ledger` / `counterpart_ledger` must catch up first).

```sh
stellarindex-ops supply audit USDC-GA5Z... \
    -config /etc/stellarindex.toml \
    -cross-check CCW6... \
    -history-hours 24
```

2. Replay Algorithm 3 (projected source `sep41_supply_events`). Decompress the window first ([projector-replay](projector.md#stellarindex_projector_replay_stalled) pre-flight). The command also resets the `sep41_supply_rollup` fold.

```sh
stellarindex-ops projector-replay -config /etc/stellarindex.toml \
    -source sep41_supply -from <first-diverging-ledger> -write
```

3. Confirm the fold once the cursor is at tip (under `run-heavy-job.sh` on r1): `stellarindex-ops supply verify-rollup -config /etc/stellarindex.toml -contracts <C...>`.
4. Replay Algorithm 2 legs: SAC-wrapped via `stellarindex-ops supply seed-sac-balances`, claimable via `stellarindex-ops supply seed-claimable-balances` (both `-write`, under `run-heavy-job.sh`). Trustline and LP legs have no lake re-seed; a fault there is a code defect.
5. Verify the gauge is below 2 within 10 min of the replay. If the divergence is over 0.1% of total, treat the asset's F2 fields as advisory; no in-tree override exists.

False positives: an Algorithm 2 backfill running behind Algorithm 3 (silence only the affected `classic_key`); a mint between the two reads shows a small `escrow_excess` that clears next tick.

Escalate: capture the first diverging ledger (walk `asset_supply_history` back), the replay commands and result, and which algorithm was at fault, for the postmortem.

## stellarindex_supply_cross_check_unevaluable

Severity P3. Rules: `deploy/monitoring/rules/supply.yml` + `configs/prometheus/rules.r1/supply.yml` (promtool test `deploy/monitoring/rule-tests/supply_test.yml`). Nothing served is wrong, but the classic/SAC check for a pair is not running, so [`stellarindex_supply_cross_check_divergence`](#stellarindex_supply_cross_check_divergence) cannot fire for it.

Symptom: `increase(stellarindex_supply_cross_check_total{outcome=~"missing_snapshot|read_error|misaligned"}[30m]) > 0` for over an hour, and a `classic_key` series missing from `stellarindex_supply_cross_check_divergence_stroops` (an unevaluable pair has its series deleted).

| `outcome` | Means |
| --------- | ----- |
| `missing_snapshot` | One side has no `asset_supply_history` row. |
| `read_error` | Reading a snapshot or comparing keeps failing. |
| `misaligned` | Snapshots more than 1000 ledgers (`supply.CrossCheckLedgerTolerance`) apart; one refresher stalled. |

### Check

The counter has no `classic_key` label; the pair comes from the aggregator log (`read_error` / `misaligned` at WARN, `missing_snapshot` at DEBUG).

```sh
# 1) Which pair, and why. For missing_snapshot, compare the pairs
#    registered at startup with the series the gauge still exports.
journalctl -u stellarindex-aggregator --since='2 hours ago' --no-pager \
  | grep -E 'cross-check: (classic read failed|sac read failed|snapshots misaligned|compare failed)|cross-check pairs registered'
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_supply_cross_check_divergence_stroops'

# 2) How old each side's latest snapshot is (substitute the pair's keys).
psql -d stellarindex -c \
  "SELECT asset_key, max(time) AS latest, max(ledger_sequence) AS ledger
     FROM asset_supply_history
    WHERE asset_key IN ('USDC:GA5Z...', 'CCW6...')
    GROUP BY asset_key;"
```

### Likely causes

- One side's supply refresher is not producing snapshots (`misaligned` or `missing_snapshot`).
- Newly added pair: classic asset not in `[supply].watched_classic_assets` or SAC not in `watched_sep41_contracts` (the aggregator WARNs at startup for each non-cross-checked `sac_wrappers` entry).
- Storage trouble (`read_error`), or a snapshot with nil `total_supply` (`compare failed` line).

### Fix

1. Stale side: follow [`stalled`](#stellarindex_aggregator_supply_refresh_stalled) or [`error-dominant`](#stellarindex_aggregator_supply_refresh_error_dominant) for that asset. The check recovers once both sides are within 1000 ledgers.
2. New pair: wait one refresh cadence, then check the two watch lists.
3. `read_error`: check [`pg-conns-saturated`](postgres.md#stellarindex_timescale_connections_saturated) and [`timescale-primary-down`](postgres.md#stellarindex_timescale_primary_down). For `compare failed`, re-seed that asset's snapshot; never change the tolerance.
4. When fixed, the next tick restores the series; if it reads over 1, go to [`cross-check-divergence`](#stellarindex_supply_cross_check_divergence).

Escalate: unevaluable over a day.

## stellarindex_supply_divergence_high

Severity P3. Rules: `deploy/monitoring/rules/supply.yml` + `configs/prometheus/rules.r1/supply.yml`. Our served `circulating_supply` (and derived `market_cap_usd`) differs from the external reference by more than 1%. This compares our number against the world's (Stellar Network Dashboard `dashboard.stellar.org/api/v3/lumens`, primary; CoinGecko off by default), unlike [`cross_check_divergence`](#stellarindex_supply_cross_check_divergence), which compares two of ours. Method: [`xlm-circulating-supply.md`](../../methodology/xlm-circulating-supply.md).

Symptom: `stellarindex_supply_divergence_ratio{asset="native",reference="stellar-dashboard"} > 0.01` for 1 h; `asset` and `reference` labels name the pair. The normal residual is about 0.03% (the Dashboard also subtracts the ~10.1 M XLM Fee Pool), so 1% is a real drift.

### Check

```sh
# 1) Confirm the divergence + which reference.
curl -fs http://localhost:9465/metrics \
  | grep '^stellarindex_supply_divergence_ratio'

# 2) Our served figure + basis.
curl -fs http://localhost:3000/v1/assets/native \
  | jq '{circulating_supply, supply_basis, total_supply}'

# 3) The reference's current figure (Dashboard).
curl -fs https://dashboard.stellar.org/api/v3/lumens \
  | jq '{totalSupply, sdfMandate, upgradeReserve, feePool}'
```

Dashboard circulating is `totalSupply - sdfMandate - upgradeReserve - feePool`. Ours lower: over-excluding (a reserve account was distributed from). Ours higher: under-excluding (new reserve account untracked). A moved `sdfMandate` component usually names the account.

### Likely causes

- Stale SDF-reserve exclusion list (`stellarindex_sdf_reserve_accounts`, `stellarindex_reserve_balances_stroops`); most likely.
- The reference changed methodology.
- Silent non-firing states: reference dark records `outcome="no_reference"` and holds the gauge; no served snapshot yet records `outcome="refresh_error"`; `[divergence.supply].enabled = false` (default) means no series.

### Fix

Do NOT edit our figure to make the alert stop; fix the exclusion set or record the reference change.

1. Reconcile both lists against SDF's published non-circulating set (Dashboard `lumens.js`). They live in `configs/ansible/.../roles/archival-node/defaults/main.yml`; change both in one PR (r1 config is ansible-managed, see `docs/operations/maintainer-workflow.md`).
2. Apply: `ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml --tags supply --check --diff`, then without `--check`.
3. Trigger a supply refresh (`supply.Refresher` on cadence, or the `stellarindex-ops supply snapshot` timer). A config-only account-set change needs no lake re-derive.
4. Verify the ratio drops below 0.01 within a couple of 15 min worker cycles.
5. If the reference changed methodology: confirm in its docs, update the reconciliation table in the methodology doc, and if the new residual is legitimately over 1% widen `[divergence.supply].threshold_pct` and the `0.01` literal in BOTH rule trees with a comment.

Escalate: record which account moved and the PR diff for the postmortem.

## Supply refresh alerts: shared context

Six alerts, one rule file (`configs/prometheus/rules.r1/supply-refresh.yml`, group `stellarindex.supply_refresh`, `component: supply`; multi-host twin `deploy/monitoring/rules/supply-refresh.yml`). Only `_stalled` is `severity: page` (P2); the rest are `ticket` (P3).

The aggregator goroutine (`runSupplyRefresh`, gated by `[supply] aggregator_refresh_enabled`, true on r1) ticks every `aggregator_refresh_cadence` (default 5 min) per watched asset, increments `stellarindex_aggregator_supply_refresh_total{asset_key, outcome}`, and feeds the F2 fields on `/v1/assets/{id}` via `asset_supply_history`. On failure the previous snapshot stays (correct but old). The timer path has its own alerts below. 
```sh
systemctl status stellarindex-aggregator
curl -s http://localhost:9465/metrics | awk '/^stellarindex_aggregator_supply_refresh_total\{/' | sort
sudo journalctl -u stellarindex-aggregator --since "1 hour ago" -n 200 | grep -E "supply refresh|supply-refresh"
grep -A 10 "^\[supply" /etc/stellarindex.toml
```

PromQL: `sum by (asset_key, outcome) (rate(stellarindex_aggregator_supply_refresh_total[15m]))`. `supply refresh ok` is Debug-level; look for Warn/Error lines (`supply refresh: no ledger` / `compute failed` / `insert failed` / `supply refresh: <outcome>`).

## stellarindex_aggregator_supply_refresh_stalled

P2 (`page`), `for: 5m`. Expr: `sum(changes(stellarindex_aggregator_supply_refresh_total{outcome="ok"}[30m])) == 0`. No `ok` tick anywhere in the fleet for 30 min; F2 fields go stale across all watched assets. One healthy asset means per-asset failure: [error_dominant](#stellarindex_aggregator_supply_refresh_error_dominant).

### Check

1. `systemctl status stellarindex-aggregator`: process up?
2. `curl -s http://localhost:9465/metrics | grep stellarindex_aggregator_ticks_total`: orchestrator alive (counter incrementing)?
3. The per-`asset_key` outcome listing above; if only some keys stalled, `error_dominant` also fires.
4. Recent supply-refresh Warn/Error logs.

### Likely causes

- Aggregator process down: read the crash in journald, restart.
- Orchestrator wedged (`ticks_total` also stalled): take a pprof goroutine dump if possible, restart, file a P2 bug.
- Every tick failing: go to `error_dominant`.
- A disabled refresher cannot fire this (the series is absent, see [never_initialized](#stellarindex_aggregator_supply_refresh_never_initialized)).

### Fix

1. If up but stalled and `ticks_total` is stalled too, restart `stellarindex-aggregator`.
2. If the orchestrator ticks but supply does not, confirm `aggregator_refresh_enabled = true` and look for a repeated outcome label.
3. Verify `outcome="ok"` increments within one cadence; the alert clears once any `ok` lands in the trailing 30 min.

Escalate: when the orchestrator tick counter is the stalled one, see [`aggregator.md#stellarindex_aggregator_silent`](aggregator.md#stellarindex_aggregator_silent).

## stellarindex_aggregator_supply_refresh_never_initialized

P3 (`ticket`), `for: 5m`. Expr: `absent_over_time(stellarindex_aggregator_supply_refresh_total{outcome="ok"}[36h]) == 1`. The goroutine never produced a successful tick; F2 fields on `/v1/assets/{id}` are NULL for every asset. Timer-path sibling: [supply_snapshot_never_initialized](#stellarindex_supply_snapshot_never_initialized).

### Check

```sh
journalctl -u stellarindex-aggregator -n 200 --no-pager | grep -iE 'supply.*refresh|watched_'
grep -E '\[supply\]|watched_classic_assets|watched_sep41_contracts|sdf_reserve_accounts' /etc/stellarindex.toml
sudo -u postgres psql -d stellarindex -c "SELECT * FROM asset_supply_history ORDER BY time DESC LIMIT 5;"
```

### Likely causes

- Empty `[supply].watched_*`: the refresh path is gated on a non-empty set, so the goroutine is silent. Most common: a new asset was never added to the list.
- Non-empty config but `asset_supply_history` empty: every asset failing; check the LCM reader and classic-supply observers.
- `aggregator_refresh_enabled = false` (code default).
- Aggregator just restarted: the first refresh waits on the bootstrap window; wait 5 min.

### Fix

1. Populate the watched list in `/etc/stellarindex.toml`. Keys are `watched_classic_assets` / `watched_sep41_contracts`; the short forms are rejected at load.

```toml
[supply]
watched_classic_assets = [
    "USDC-GA5ZSEJYB37JRC5AVCIA5MOP4RHTM335X2KGX3IHOJAPP5RE34K4KZVN",
    "EURC-GDHU6WRG4IEQXM5NZ4BMPKOXHW76MZM4Y2IEMFDVXBSDP6SJY4ITNPP2",
    # ... full list per docs/operations/supply-snapshot.md
]
watched_sep41_contracts = []
sdf_reserve_accounts = ["GA..."]
```

2. `systemctl restart stellarindex-aggregator`.
3. Within 5 min `outcome="ok"` increments; confirm `circulating_supply` is non-null on a watched asset (and `market_cap_usd` when it has a USD price).

Escalate: `ok` still absent after the list is populated: check `compute_error` / `no_observation` under [error_dominant](#stellarindex_aggregator_supply_refresh_error_dominant).

## stellarindex_aggregator_supply_refresh_error_dominant

P3 (`ticket`), `for: 30m`. Expr: per asset, `sum by (asset_key) (rate(..._supply_refresh_total{outcome!~"ok|dormant|missing_baseline"}[5m])) / (sum by (asset_key) (rate(..._supply_refresh_total[5m])) > 0) > 0.5`. Over 50% of one asset's ticks are non-ok; its F2 fields are stale. Counted: `no_ledger`, `no_observation`, `compute_error`, `write_error`, `stale_component`, `missing_freshness`, `static_reserve`. Excluded:

- `dormant`: accepted snapshot, not an error (see `stale_component` below, and [dormant_fleet](#stellarindex_aggregator_supply_refresh_dormant_fleet)).
- `missing_baseline`: unseeded SEP-41 genesis baseline for a SAC wrapper. Run `stellarindex-ops supply seed-sep41-genesis`; range-scoped, not corruption.

### Check

1. Dominant outcome: `curl -s http://localhost:9465/metrics | grep stellarindex_aggregator_supply_refresh_total | sort -t' ' -k2 -rn | head`.
2. Per-asset breakdown (shared commands). One `asset_key` dominating means per-asset config (watched list, locked-set member, SAC wrapper map gap).
3. `sudo journalctl -u stellarindex-aggregator --since "30 min ago" -n 200 | grep "supply refresh: " | sort | uniq -c | sort -rn | head`
4. Sanity-check `[supply]` config (shared commands).

### Likely causes by outcome

- `no_ledger`: no chain position to stamp. The aggregator clamps the `ledgerstream` cursor (`ingestion_cursors`) to the newest `stellar.ledgers` row within 512 ledgers (about 45 min). Either no cursor (indexer not writing, or storage broken: [`postgres.md#stellarindex_timescale_connections_saturated`](postgres.md#stellarindex_timescale_connections_saturated)), or no lake row in that window (empty, gapped, or wedged CH sink: compare `max(ledger_seq)` in `stellar.ledgers` with the cursor and check archivist / galexie lag).
- `no_observation`: live reader returned nothing and the static fallback was empty; common after deploy while the AccountEntry observer backfills. Wait, or populate `[supply.reserve_balances_stroops]` and `[metadata.issuer_home_domains]` as a bridge.
- `compute_error`: the algorithm failed (XLM reserve reader, one of four classic component sums, or the SEP-41 kind-totals query). Read the wrapped error; usually a code bug or unparseable asset; roll back a recent deploy.
- `write_error`: `Store.InsertSupply` failed (Postgres down, NUMERIC overflow, CHECK violation on `asset_supply_history`).
- `missing_freshness`: only with `[supply] strict_freshness_required = true`; a snapshot had `MinComponentLedger == 0`. Check freshness producers, or leave strict mode off.

### stale_component

The freshness gate rejected the snapshot: a component observation lags the chain tip by more than `[supply] stale_component_ledgers` (default 1000, about 85 min). Check whether `MinComponentLedger` is moving.

- Stalled producer (real): the observer for trustlines / claimable_balances / liquidity_pools / sac_balances (classic) or sep41_supply is wedged; indexer per-source freshness for its hypertable also lags and `gap=` in the WARN log keeps growing. Treat as an ingest stall and route to the ingest-pipeline runbooks. Never relax the gate to mask it.
- Dormant asset: a quiet asset (PHO, niche credits) has a frozen `MinComponentLedger` and its last observation IS current. The refresher accepts it as `outcome="dormant"` while the gap stays within `DefaultMaxDormantComponentLedgers` (17,280 ledgers, about 24 h). A single `stale_component` on the first tick after a restart is normal. Past 24 h it fails closed: publishing stops, this alert fires, and the log reads `stalled observer, not a dormant asset` with a `dormancy_horizon` field.
- Discriminator: did `min_component_ledger` last advance recently, and is the observer's ingest otherwise healthy? Healthy and nothing to write is dormant; stalled ingest is the first case.
- For a cold-start blip, raise that asset's threshold (below). For an asset confirmed dormant beyond 24 h, `[supply] max_dormant_component_ledgers = 0` restores unbounded dormancy, but it is global; prefer the per-asset override.

Per-asset override (do not loosen the gate fleet-wide):

```toml
[supply.stale_component_ledgers_by_asset]
# asset_key (CODE:ISSUER for classic, bare contract id for SEP-41)
# = relaxed threshold in ledgers. ~5000 = 7 h.
"PHO:GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO" = 5000
```

A key that does not parse, is outside the watched lists, or duplicates another spelling fails config validation at boot. `0` disables the gate for that asset only.

### Fix

1. Identify the dominant outcome and apply the matching cause above.
2. `no_observation` during bootstrap is expected; a new `watched_classic_assets` entry ticks `no_observation` until its locked-set members have rows.
3. Verify the `ok` rate exceeds the error rate; the alert clears within 30 min.

## stellarindex_aggregator_supply_refresh_dormant_fleet

P3 (`ticket`), `for: 30m`. Expr: `count(sum by (asset_key) (rate(..._supply_refresh_total{outcome="dormant"}[15m])) > 0) > 1`. Two or more assets `dormant` together. `MinComponentLedger` is a shared watermark (classic assets share the slowest of four component observers; SEP-41 share `MAX(ledger)` of `sep41_supply_events`), so a quiet asset cannot freeze it alone: the producer stopped, and served supply silently goes stale until the 24 h horizon flips these to `stale_component`.

### Check

Dormant `asset_key`s are all classic (or all SEP-41) and their logs carry the same `min_component_ledger`. Compare `MAX(ledger)` of `trustline_observations`, `claimable_observations`, `lp_reserve_observations` and `sac_balance_observations` (or `sep41_supply_events`) with the chain tip.

### Fix

1. The lowest watermark is the stalled producer; route to the ingest-pipeline runbooks.
2. Do not raise per-asset thresholds to silence it.

## stellarindex_sep41_supply_rollup_no_cursor

P3 (`ticket`), `for: 30m`, per `contract_id`. Served SEP-41 supply stays exact, but every read re-sums the contract's whole `sep41_supply_events` history (Postgres load; migration 0085 exists to prevent it). The rollup worker folds only rows the projector committed, bounded by `ingestion_cursors` row `(source='projector', sub_source='sep41_supply')`; when absent it folds nothing and counts `outcome="no_cursor"` on `stellarindex_sep41_supply_rollup_advances_total`. Log: `sep41 supply rollup pinned: projector cursor for sep41_supply is absent`.

### Check

```sh
psql -U stellarindex -d stellarindex -c \
  "SELECT source, sub_source, last_ledger, last_updated FROM ingestion_cursors WHERE source = 'projector' ORDER BY sub_source;"
psql -U stellarindex -d stellarindex -c \
  "SELECT contract_id, last_ledger, updated_at FROM sep41_supply_rollup ORDER BY contract_id;"
sudo journalctl -u stellarindex-indexer --since "30 min ago" | grep -i 'projector' | grep -i 'sep41_supply' | tail -20
```

### Likely causes

- Projector registers `sep41_supply` only when the indexer's `[supply] watched_sep41_contracts` is non-empty; the aggregator watches contracts the indexer does not.
- Projector running but not advancing.
- `ingestion_cursors` lost in a restore.

### Fix

1. Get the projector committing `sep41_supply` cycles (align the watched lists).
2. Not advancing: [projector-wedged](projector.md#stellarindex_projector_wedged) or [projector-row-quarantined](projector.md#stellarindex_projector_row_quarantined).
3. Lost cursor: never insert a cursor row by hand (it claims settlement never evidenced). Re-run `stellarindex-ops projector-replay -source sep41_supply` ([projector-replay](projector.md#stellarindex_projector_replay_stalled)).
4. Verify `ok` or `noop` resumes on the next rollup pass.

Reset and re-fold: [sep41-mint-recovery](../sep41-mint-recovery.md). 

## stellarindex_ch_supply_gapfill_failed

P3 (`ticket`), `for: 10m`. Expr: `node_systemd_unit_state{name="ch-supply.service",state="failed"} == 1`. The daily forward-gap-fill of `stellar.supply_flows` (backs `/v1/assets` SEP-41 supply) failed. Served supply is likely still correct; the backstop is down. Timer `ch-supply.timer` runs about 08:00.

### Check

```sh
ssh root@r1 'systemctl status ch-supply.service --no-pager'
ssh root@r1 'journalctl -u ch-supply.service -n 60 --no-pager'
```

### Likely causes

- `seed [X,Y] FAILED`: a `stellarindex-ops ch-supply` chunk errored, usually ClickHouse pressure.
- `ch-supply: tip unresolved` (Postgres `ingestion_cursors` tip empty) or `supply_flows watermark unresolved` (ClickHouse `max(ledger_seq)` probe failed; check ClickHouse on `:8123` and that `stellar.supply_flows` exists).
- `state the mode` or `flag provided but not defined: -write`: binary and `run-ch-supply.sh` from different releases (it must pass `-seed-flows -write`); re-run the ansible deploy.

### Fix

1. ClickHouse pressure: `systemctl start ch-supply.service` off-peak; `CHSUPPLY_MEMGUARD` throttles it. Idempotent (ReplacingMergeTree key).
2. Tip unresolved: confirm the indexer advances the `ledgerstream` cursor; fix upstream, re-run.
3. Verify: `ssh root@r1 'clickhouse-client --port 9300 -q "SELECT max(ledger_seq) FROM stellar.supply_flows"'` advanced toward tip; the alert clears about 1 min after the next scrape.

Script `configs/ansible/roles/archival-node/files/run-ch-supply.sh`. Sibling: [`verify-archive.md#stellarindex_verify_archive_unit_failed`](verify-archive.md#stellarindex_verify_archive_unit_failed). Escalate: a persistent failed state is real.

## Supply snapshot alerts: shared context

Rule file `configs/prometheus/rules.r1/supply-snapshot.yml` (group `stellarindex.supply_snapshot`, `component: supply`; twin `deploy/monitoring/rules/supply-snapshot.yml`). `circulating_zero` and `critical_stale` are `page` (P2); the other three are `ticket` (P3).

All five watch the systemd-timer path: `supply-snapshot.timer` -> `supply-snapshot.service` -> `stellarindex-ops supply snapshot` (native XLM, daily). The binary writes `/var/lib/node_exporter/textfile_collector/supply_snapshot.prom` itself (`internal/supply/textfile.go`, gated on `-textfile-output` / unit env `TEXTFILE_OUTPUT`). On r1 `/etc/default/supply-snapshot` is ansible-managed with `TEXTFILE_OUTPUT` set (`10-observability.yml`); finding it unset is drift to codify and fix. Do NOT silence an alert here because the aggregator goroutine (refresh alerts above) covers it. Overview: [supply-pipeline.md](../../architecture/supply-pipeline.md). Impact of all: `/v1/assets/{id}` F2 fields go stale or wrong.

```sh
ssh root@136.243.90.96
journalctl -u supply-snapshot.service -n 100 --output=cat
systemctl status supply-snapshot.timer; systemctl list-timers supply-snapshot.timer
systemctl start supply-snapshot.service        # force a one-off run
runuser -u stellarindex -- /usr/local/bin/stellarindex-ops supply snapshot -config /etc/stellarindex.toml -dry-run
```

The writer also dials ClickHouse for the ledger close time (`-ch-addr`, default `127.0.0.1:9300`); a down ClickHouse fails the run.

## stellarindex_supply_snapshot_unit_failed_alert

`stellarindex_supply_snapshot_unit_failed > 0` for 30 min (`ticket`). The gauge is emitted by `supplySnapshotMaybeEmitFailure` (`internal/ops/supply/supply.go`). F2 fields keep the previous value but go stale.

### Check

Shared commands above (journal, dry-run), then:

```sh
runuser -u stellarindex -- /usr/local/bin/stellarindex-ops docs-config | head   # parses cleanly?
grep -E "sdf_reserve_accounts|reserve_balances_stroops" /etc/stellarindex.toml
```

### Likely causes

- Missing `reserve_balances_stroops` entry, hit only when the static fallback is consulted for an observer-uncovered account: `supply: ConfigReserveBalanceReader: no balance configured for account G...`.
- Postgres or ClickHouse unavailable (`timescale.Open` / `InsertSupply` failed); see [`postgres.md#stellarindex_timescale_connections_saturated`](postgres.md#stellarindex_timescale_connections_saturated).
- Fresh box: `no ingestion cursors yet - pass -ledger explicitly`.
- Config edit broke parsing (`config:` prefix). A config parse error never sets `unit_failed=1` (it exits before the first emit), so it surfaces only about 36 h later via [`_stale`](#stellarindex_supply_snapshot_stale) or [`_never_initialized`](#stellarindex_supply_snapshot_never_initialized); check journald for `config:` first when arriving from those.

### Fix

1. Missing balance: add the entry, or backfill the AccountEntry observer so the live path covers the account.
2. No cursors: wait for the indexer's first cursor, or set `EXTRA_FLAGS="-ledger <known-good>"` in `/etc/default/supply-snapshot`.
3. Bad TOML: fix it and reload.
4. `systemctl start supply-snapshot.service`; verify `unit_failed` returns to 0 and `last_success_timestamp` updates.

## stellarindex_supply_snapshot_stale

`(time() - stellarindex_supply_snapshot_last_success_timestamp{asset_key=...}) > 36*3600`, `for: 5m`, `ticket`, `alert_family: supply_snapshot_stale` (24 h cadence + 12 h cushion). F2 `observed_at` is over a day behind. If it fired, the gauge existed, so a run did succeed once; an absent series is [`_never_initialized`](#stellarindex_supply_snapshot_never_initialized).

### Check

```sh
systemctl status supply-snapshot.timer ; systemctl list-timers supply-snapshot.timer   # scheduled?
journalctl -u supply-snapshot.service --since "3 days ago" -n 50                       # last run?
ls -la /var/lib/node_exporter/textfile_collector/supply_snapshot.prom                  # textfile written?
systemctl start supply-snapshot.service                                                # force a run
```

### Likely causes

- Timer disabled after maintenance.
- Service failing every run (fires with [`unit_failed_alert`](#stellarindex_supply_snapshot_unit_failed_alert)).
- Backwards host clock jump.

### Fix

1. Timer: `systemctl enable --now supply-snapshot.timer`.
2. Failing unit: follow `unit_failed_alert`.
3. Clock: fix ntp, then run the writer once.
4. Verify `last_success_timestamp` updates within 60 s of a successful run reaching node_exporter.

## stellarindex_supply_snapshot_critical_stale

Same expression and checks as [`_stale`](#stellarindex_supply_snapshot_stale) with threshold `> 72*3600`, `for: 5m`, `page` (P2). Three days without a snapshot; `_stale` has gone unanswered for 36 h. Follow `_stale` (and `unit_failed_alert` if runs are failing); escalate per the SEV-3 playbook.

## stellarindex_supply_snapshot_never_initialized

`absent_over_time(stellarindex_supply_snapshot_last_success_timestamp[36h]) == 1`, `for: 5m`, `ticket`. F2 fields render `null` for every asset (`/v1/assets/native` omits `circulating_supply` / `market_cap_usd`); `SELECT count(*) FROM asset_supply_history` returns 0. Invisible to `_stale` because `time() - <missing>` is no data. Goroutine-path sibling: [`aggregator_supply_refresh_never_initialized`](#stellarindex_aggregator_supply_refresh_never_initialized).

### Check

```sh
systemctl is-enabled supply-snapshot.timer     # "not-found" = never installed; "enabled" = installed
systemctl status supply-snapshot.timer --no-pager
systemctl status supply-snapshot.service --no-pager   # most recent run
grep -A 1 'aggregator_refresh_enabled' /etc/stellarindex.toml   # absent/false = goroutine path off
journalctl -u stellarindex-aggregator --since '2 days ago' | grep -E 'supply-refresh|supply.*ok|supply.*err'
```

### Likely causes

- Timer never installed, or `TEXTFILE_OUTPUT` empty (the shipped unit default): Postgres is written but the gauge is never published.
- `reserve_balances_stroops` not operator-set (the pipeline ships dormant by design; [`supply-snapshot.md`](../supply-snapshot.md)).

### Fix

Only the ops-CLI textfile writer (Path A) emits `last_success_timestamp`, so only Path A clears this alert.

1. Path A, systemd timer:

```sh
sudo cp deploy/systemd/supply-snapshot.{service,timer} /etc/systemd/system/
sudo systemctl daemon-reload

# CRITICAL: wire the textfile output BEFORE starting. The shipped unit defaults
# `Environment=TEXTFILE_OUTPUT=` (EMPTY, no metrics emitted). Started as-is the
# snapshot writes to Postgres but never publishes last_success_timestamp, so this
# alert never clears and the verify grep below fails on a working writer.
cat <<'EOT' | sudo tee /etc/default/supply-snapshot
TEXTFILE_OUTPUT=/var/lib/node_exporter/textfile_collector/supply_snapshot.prom
EOT

sudo systemctl enable --now supply-snapshot.timer
sudo systemctl start supply-snapshot.service
sudo journalctl -u supply-snapshot.service --no-pager -n 50
# Expect: `Wrote snapshot for asset_key=XLM ledger=<N> basis=<...>` and zero exit.
curl -s http://localhost:9100/metrics | grep stellarindex_supply_snapshot_last_success_timestamp
# Expect one line with a recent unix timestamp (after node_exporter rescrapes the .prom).
```

2. Path B, aggregator goroutine (does NOT clear this alert; it clears the aggregator sibling):

```sh
# /etc/stellarindex.toml
[supply]
aggregator_refresh_enabled = true
# optional: aggregator_refresh_cadence = "5m" (default)

sudo systemctl restart stellarindex-aggregator
sudo journalctl -u stellarindex-aggregator -f | grep -E 'supply-refresh'     # tick within one cadence
curl -s http://localhost:9465/metrics | grep stellarindex_aggregator_supply_refresh_total
# Expect at least one outcome="ok" line per watched asset_key.
```

3. If a deployment intentionally runs Path B only, silence this alert (`_stale` cannot fire there).
4. Verify within max(36 h, `aggregator_refresh_cadence`):

```sh
sudo -u postgres psql -d stellarindex -c \
  "SELECT asset_key, count(*) AS rows, max(time) AS latest
   FROM asset_supply_history GROUP BY asset_key ORDER BY asset_key"
curl -s 'https://api.stellarindex.io/v1/assets/native' | jq '.data.circulating_supply'   # numeric string, not null
```

Escalate: the first run fails; go to `unit_failed_alert`.

## stellarindex_supply_snapshot_circulating_zero

`stellarindex_supply_snapshot_circulating_xlm{asset_key="XLM"} <= 0`, `for: 5m`, `page` (P2). `/v1/assets/native` reports `circulating_supply: 0`, a customer-visible data-quality incident. Presumes the writer ran but produced a wrong value (writer failures are [`unit_failed`](#stellarindex_supply_snapshot_unit_failed_alert)). Native circulating = total - sum(SDF reserves) (ADR-0011); non-positive means reserves meet or exceed the frozen total, or the computer is broken.

### Check

The writer reads reserve balances from the `account_observations` observer FIRST; check the database before the TOML.

```sh
# 1. Latest snapshot (flags before the positional; Go's flag package stops at the first positional)
stellarindex-ops supply audit -config /etc/stellarindex.toml native

# 2. Which reserve source did the writer use? The reader consults the
#    account_observations hypertable first; the TOML map is used only when at
#    least one watched account has NO observation at-or-before the snapshot
#    ledger (the whole call then drops to the static map, no mixing).
runuser -u postgres -- psql -d stellarindex -c \
  "SELECT account_id, max(ledger) AS latest_obs,
          (SELECT balance_stroops FROM account_observations b
            WHERE b.account_id = a.account_id
            ORDER BY ledger DESC LIMIT 1) AS latest_balance
     FROM account_observations a
    WHERE account_id IN (SELECT unnest(ARRAY['G...','G...']))  -- paste sdf_reserve_accounts
    GROUP BY account_id;"

# 3. ONLY if step 2 shows an uncovered account (fallback in play): sum the TOML
#    balances vs the frozen total. A wrong TOML value is harmless while the
#    observer covers every account.
grep -A 100 "^\[supply" /etc/stellarindex.toml
python3 -c "
import re, sys
content = open('/etc/stellarindex.toml').read()
balances = re.findall(r'^\\s*\"?([A-Z0-9]+)\"?\\s*=\\s*\"?(\\d+)\"?', content, re.M)
total = sum(int(b) for _, b in balances if len(_) == 56)
print(f'sum of reserve balances: {total} stroops = {total/1e7:.2f} XLM')
print(f'frozen total:           500018068120000000 stroops = 50,001,806,812.00 XLM')
print(f'difference:             {500018068120000000 - total} stroops')
"

# 4. Dry-run to confirm reproduction
stellarindex-ops supply snapshot -config /etc/stellarindex.toml -dry-run
```

### Likely causes

- Corrupt or inflated observer balances (`latest_balance` implausible vs stellar.expert).
- TOML fallback balance in USD or XLM instead of stroops (10^7 too large); only with an uncovered account.
- `sdf_reserve_accounts` includes the issuer or a payment account (poisons both paths).
- XLMComputer regression: `-dry-run` is wrong with verified-correct inputs.

### Fix

1. Observer data: file a P2 against the AccountEntry observer; correct or re-derive the affected `account_observations` rows and re-run the writer.
2. TOML: divide the entry by 10^7 or remove the extra G-strkeys; force a run.
3. Computer bug: roll back the writer binary; file a P2.
4. Verify `circulating_supply > 0` on the next snapshot; clears within 5 min.

Escalate: P2 bug in every code-side case.

## stellarindex_supply_verify_rollup_stale

P3 (`ticket`). Rules `configs/prometheus/rules.r1/supply-verify-rollup.yml`, twin `deploy/monitoring/rules/supply-verify-rollup.yml`. Covers `_stale` and `_never_initialized`. No customer impact alone: the SEP-41 rollup is no longer certified against its source events.

Symptom: `_stale`: `stellarindex_supply_verify_rollup_last_success_timestamp` older than 36 h (runs failing or timer stopped). `_never_initialized`: series absent 36 h (timer never installed, node_exporter not reading the textfile directory, or no run ever passed clean).

### Check

```sh
systemctl list-timers supply-verify-rollup.timer
systemctl status supply-verify-rollup.service
cat /var/lib/node_exporter/textfile_collector/supply_verify_rollup.prom
```

### Likely causes

- Timer absent: the archival-node role was not applied since the unit landed; apply with `--tags ops-jobs`.
- File has `unit_failed 1`: [unit_failed_alert](#stellarindex_supply_verify_rollup_unit_failed_alert).
- File absent after a run: check `TEXTFILE_OUTPUT` in `/etc/default/supply-verify-rollup` and the unit's `ReadWritePaths`.

### Fix

1. `systemctl start supply-verify-rollup.service` for one clean run.
2. Verify `last_success_timestamp` advances; both alerts clear within one scrape plus `for: 5m`.

## stellarindex_supply_verify_rollup_unit_failed_alert

P3 (`ticket`), `for: 30m`, group `stellarindex.supply_verify_rollup`. `stellarindex-ops supply verify-rollup` re-sums `sep41_supply_events` per watched contract at its `sep41_supply_rollup` checkpoint and diffs against the fold, daily from `supply-verify-rollup.timer`. Served SEP-41 supply for a drifted contract can be wrong until the fold is rebuilt (30-60 min, one contract at a time).

Symptom: `stellarindex_supply_verify_rollup_unit_failed == 1`. The gauges tell the cause:

| `drift_total` | `missing_total` | `checked_total` | Cause |
| --- | --- | --- | --- |
| > 0 | any | > 0 | A checkpoint diverges from the re-sum. |
| 0 | > 0 | any | A watched contract has no `sep41_supply_rollup` row. |
| 0 | 0 | 0 | Nothing checked (empty rollup table, or `-contracts` matched nothing). |
| absent | absent | absent | Errored before counting (config, Postgres, timeout). |

### Check

```sh
cat /var/lib/node_exporter/textfile_collector/supply_verify_rollup.prom
journalctl -u supply-verify-rollup.service -n 100 --output=cat
```

The journal names every drifted `(contract, kind)` and missing contract.

### Fix

1. Drift: rebuild the fold for every watched contract (no contract filter; one transaction per contract), then `systemctl start supply-verify-rollup.service` and confirm `unit_failed 0`:

```sh
stellarindex-ops supply seed-sep41-genesis -config /etc/stellarindex.toml -write
```

2. Missing: the contract is in `[supply] watched_sep41_contracts` but never folded. Check [no_cursor](#stellarindex_sep41_supply_rollup_no_cursor) first, then seed as above.
3. Nothing checked: the role installs the job only where `stellarindex_watched_sep41_contracts` is non-empty (`supply_verify_rollup_enabled`); treat as missing.
4. Error: fix the journaled cause; a timeout means raising `RUN_TIMEOUT` in `/etc/default/supply-verify-rollup` and `TimeoutStartSec` in the unit.

Code: `internal/ops/supply/supply_verify_rollup.go`; units `configs/ansible/roles/archival-node/templates/systemd/supply-verify-rollup.{service,timer}.j2`. A failed run is never promoted to clean.

## CS-102: quiet is not stale

The stale-supply anchor for a quiet asset must be the observer watermark, not the asset's own last activity. A quiet asset has an old last-activity ledger and is still fresh. Regression tests for both storage paths were run with a full red/green proof on 2026-07-29: with the defect re-introduced they fail with `quiet asset anchor = 1000, want 5000 (the observer watermark)`, and they pass with the fix. PHO's +157% divergence is a separate cause (archived SAC entries seeded as live; see `docs/architecture/domain-traps.md`).

## Related

- [ADR-0011](../../adr/0011-supply-algorithm.md), [`supply-pipeline.md`](../../architecture/supply-pipeline.md), [`xlm-circulating-supply.md`](../../methodology/xlm-circulating-supply.md), [`entry-walk-renumbering.md`](entry-walk-renumbering.md) (skipped repair after an `EntryWalkVersion` bump widens `supply_divergence_high`).
