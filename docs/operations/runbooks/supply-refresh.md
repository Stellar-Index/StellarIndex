---
title: Runbook — supply-refresh
last_verified: 2026-10-06
status: living
severity: P2
---

# Runbook — supply-refresh alerts

Six alerts, one rule file (`configs/prometheus/rules.r1/supply-refresh.yml`, group `stellarindex.supply_refresh`, `component: supply`; the multi-host twin is `deploy/monitoring/rules/supply-refresh.yml`). Only `_stalled` is `severity: page` (P2); the rest are `severity: ticket` (P3).

The aggregator-resident supply refresher (`runSupplyRefresh`, `cmd/stellarindex-aggregator`) ticks every `aggregator_refresh_cadence` (default 5 min) per watched asset and increments `stellarindex_aggregator_supply_refresh_total{asset_key, outcome}`. It feeds the F2 fields on `/v1/assets/{id}` (`circulating_supply`, `total_supply`, `max_supply`, `market_cap_usd`, `fdv_usd`) via `asset_supply_history`; on any failure the previous snapshot stays, so consumers see correct-but-old data. This is the goroutine path (`[supply] aggregator_refresh_enabled`); the code default is false but the ansible template renders `true` on r1. The systemd-timer path has its own alerts (`supply-snapshot-*`).

Shared commands (aggregator metrics on r1 are on `localhost:9465`; `ssh root@136.243.90.96`):

```sh
systemctl status stellarindex-aggregator
curl -s http://localhost:9465/metrics | awk '/^stellarindex_aggregator_supply_refresh_total\{/' | sort
sudo journalctl -u stellarindex-aggregator --since "1 hour ago" -n 200 | grep -E "supply refresh|supply-refresh"
grep -A 10 "^\[supply" /etc/stellarindex.toml
```

The metric is keyed by `(asset_key, outcome)`. Equivalent PromQL: `sum by (asset_key, outcome) (rate(stellarindex_aggregator_supply_refresh_total[15m]))`. Every asset stopped = fleet-wide; one `asset_key` failing while others tick = per-asset.

Log gotcha: the `supply refresh ok` line is Debug-level and invisible at the default log level, so its absence proves nothing. The visible signals are the Warn/Error lines (`supply refresh: no ledger` / `compute failed` / `insert failed`, and `supply refresh: <outcome>`).

## At a glance

- [`stellarindex_aggregator_supply_refresh_stalled`](#stellarindex_aggregator_supply_refresh_stalled): no `ok` tick fleet-wide for 30 min (page)
- [`stellarindex_aggregator_supply_refresh_never_initialized`](#stellarindex_aggregator_supply_refresh_never_initialized): `ok` series absent for 36 h
- [`stellarindex_aggregator_supply_refresh_error_dominant`](#stellarindex_aggregator_supply_refresh_error_dominant): one asset > 50% non-ok ticks for 30 min
- [`stellarindex_aggregator_supply_refresh_dormant_fleet`](#stellarindex_aggregator_supply_refresh_dormant_fleet): 2+ assets `dormant` together for 30 min
- [`stellarindex_sep41_supply_rollup_no_cursor`](#stellarindex_sep41_supply_rollup_no_cursor): SEP-41 rollup pinned, projector cursor absent
- [`stellarindex_ch_supply_gapfill_failed`](#stellarindex_ch_supply_gapfill_failed): `ch-supply.service` failed

## stellarindex_aggregator_supply_refresh_stalled

Severity P2 (`page`), `for: 5m`. MTTR 15-30 min. Impact: F2 fields go increasingly stale across all watched assets; customer-visible a few minutes after the alert.

Expr: `sum(changes(stellarindex_aggregator_supply_refresh_total{outcome="ok"}[30m])) == 0`. The fleet-wide `sum()` is deliberate: one healthy asset means the goroutine is alive and per-asset failure belongs to [error_dominant](#stellarindex_aggregator_supply_refresh_error_dominant). The older `time() - max(timestamp(...))` form never fired (`timestamp()` returns scrape time, about now) and must not be reintroduced.

Quick diagnosis (5 min):

1. `systemctl status stellarindex-aggregator`: process up?
2. `curl -s http://localhost:9465/metrics | grep stellarindex_aggregator_ticks_total`: is the orchestrator alive (counter incrementing)?
3. The shared per-`asset_key` outcome listing above: if only some keys stalled, `error_dominant` should also be firing.
4. Recent supply-refresh Warn/Error logs.

Root causes:

1. **Aggregator process down.** Investigate the crash in journald; restart.
2. **Orchestrator wedged.** Process runs but `stellarindex_aggregator_ticks_total` is also stalled. Restart the binary and file a P2 bug for the wedge; take a pprof goroutine dump first if available.
3. **Every tick failing.** Goroutine alive but every per-asset tick has a non-ok outcome. `error_dominant` should also be firing; go there.
4. **Refresher disabled cannot be what fired this.** With the `changes()` expr a disabled refresher leaves the series ABSENT, which is [never_initialized](#stellarindex_aggregator_supply_refresh_never_initialized). If `_stalled` fired, the refresher was recently alive.

Mitigation: check process health; if up but stalled and `ticks_total` is also stalled, restart; if the orchestrator ticks but supply does not, confirm `aggregator_refresh_enabled = true` and look for repeated outcome labels. Restart is the safe mitigation, then investigate from journald. Verification: `outcome="ok"` increments resume within one cadence (5 min); the alert clears once any `ok` increment lands inside the trailing 30 min window.

False positives: the first minutes after an aggregator restart have no observations; `for: 5m` absorbs about one cadence, longer restarts still trip it. A disabled refresher does not fire here (see cause 4).

Also see [`aggregator.md#stellarindex_aggregator_silent`](aggregator.md#stellarindex_aggregator_silent) when the orchestrator's own tick counter is stalled.

## stellarindex_aggregator_supply_refresh_never_initialized

Severity P3 (`ticket`), `for: 5m`. MTTR 15-60 min. The rule lives in the supply-refresh rule file in both trees (not `aggregator.yml`). Its `runbook_url` deliberately points at [supply-snapshot.md#stellarindex_supply_snapshot_never_initialized](supply-snapshot.md#stellarindex_supply_snapshot_never_initialized), the shared cold-deploy page covering both refresh paths; this section is self-sufficient for the aggregator alert.

Impact: the supply-refresh goroutine has never produced a successful tick; F2 fields on `/v1/assets/{id}` are NULL for every asset.

Expr: `absent_over_time(stellarindex_aggregator_supply_refresh_total{outcome="ok"}[36h]) == 1`. A never-incremented counter does not exist as a series: absent, not zero. The 36 h window means a fresh boot has over a day of grace and cannot false-positive (operators eyeballing the metric in the first 5 min may still misread "no data yet" as broken). Distinct from `_stalled`, which needs the series to have existed; together they cover "was working, stopped" and "never worked".

Symptoms:

- `outcome="ok"` series absent from the scrape.
- `/v1/assets/USDC-G...` returns the `AssetDetail` envelope with all `*_supply` and `*_cap_usd` fields null.
- No `supply refresh complete` info lines in the aggregator log.

Quick diagnosis (5 min):

```sh
journalctl -u stellarindex-aggregator -n 200 --no-pager | grep -iE 'supply.*refresh|watched_'
grep -E '\[supply\]|watched_classic_assets|watched_sep41_contracts|sdf_reserve_accounts' /etc/stellarindex.toml
sudo -u postgres psql -d stellarindex -c "SELECT * FROM asset_supply_history ORDER BY time DESC LIMIT 5;"
```

- Empty `[supply].watched_*`: the refresh path is gated on a non-empty asset set, so unset means the goroutine is intentionally silent. Most common cause (F-1266, audit-2026-05-12). The usual chain: a new asset launches, the operator does not add it to the watched list, its F2 fields show null, a support ticket lands.
- Non-empty config but `asset_supply_history` empty: goroutine wired but every asset failing; check the LCM reader / classic-supply observers.
- Also confirm `aggregator_refresh_enabled = true`: when false (the code default) the goroutine never ticks.
- Aggregator recently restarted: wait 5 min; the first refresh waits on the bootstrap window.

Mitigation (15 min):

1. Populate the watched list in `/etc/stellarindex.toml`. Keys are `watched_classic_assets` / `watched_sep41_contracts`; the short forms `watched_classic` / `watched_sep41` are NOT recognised and the TOML is rejected at load:
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
3. Within 5 min `outcome="ok"` should increment; sample a watched asset's `/v1/assets/{id}`.
4. Verification: `circulating_supply` non-null on at least one watched asset; `market_cap_usd` non-null when it has a USD price.

Long-term fix (tracked separately): auto-populate the watched list from the verified-currency catalogue (`internal/currency/data/seed.yaml`).

## stellarindex_aggregator_supply_refresh_error_dominant

Severity P3 (`ticket`), `for: 30m`. MTTR 15-60 min. Impact: F2 fields stale or wrong for the affected asset; the previous snapshot stays in `asset_supply_history`.

Expr (aggregated per asset, F-1320): `sum by (asset_key) (rate(..._supply_refresh_total{outcome!~"ok|dormant|missing_baseline"}[5m])) / (sum by (asset_key) (rate(..._supply_refresh_total[5m])) > 0) > 0.5`. Per asset because a single asset failing 100% of its ticks could never push a fleet-wide fraction past 50% while healthy siblings diluted it. Counted outcomes: `no_ledger`, `no_observation`, `compute_error`, `write_error`, `stale_component`, `missing_freshness`, `static_reserve` (XLM published from the dated static reserve map because the live account observer could not answer). Excluded:

- `dormant`: an accepted snapshot (see [stale_component](#outcomestale_component) case 2).
- `missing_baseline`: the SEP-41 genesis baseline is unseeded (SAC-wrapper whose pre-Soroban opening balance is not seeded, incident 2026-07-06). Run `stellarindex-ops supply seed-sep41-genesis`; it is range-scoped, not corruption, and a genuine post-seed negative still surfaces as `compute_error`. Visible only in the per-asset series.

Symptoms: `> 50%` of one asset's ticks non-ok for 30 min; repeated `supply refresh: <outcome>` lines with the same label.

Quick diagnosis (5 min):

1. Which outcome dominates: `curl -s http://localhost:9465/metrics | grep stellarindex_aggregator_supply_refresh_total | sort -t' ' -k2 -rn | head`.
2. Per-asset breakdown (shared commands). If one `asset_key` dominates the non-ok rate while others are healthy, the fault is per-asset (config drift on `watched_classic_assets`, missing locked-set member, SAC wrapper map gap) rather than fleet-wide.
3. Count wrapped errors: `sudo journalctl -u stellarindex-aggregator --since "30 min ago" -n 200 | grep "supply refresh: " | sort | uniq -c | sort -rn | head`.
4. Sanity-check the `[supply]` config (shared commands).

`outcome="dormant"` is NOT an error: the component anchor did not move since the last tick, so the last observation was re-stamped as current (F-1320). It is also what a dead observer looks like, so it is not evidence the producer is alive; do not chase it provided `MinComponentLedger` last moved within about 24 h (17,280 ledgers, `DefaultMaxDormantComponentLedgers`, R-002 audit-2026-07-23). Past that horizon the gate fails closed to `stale_component`: publishing stops and this alert fires.

### Root causes by dominant outcome

#### `outcome="no_ledger"`

The aggregator cannot resolve a real chain position to stamp the snapshot at. It takes the `ledgerstream` cursor from `ingestion_cursors` and clamps it to the newest ClickHouse `stellar.ledgers` row in the 512 ledgers below that cursor (about 45 min; an older row is refused anyway). Two conditions reach this outcome:

1. No cursor at all: the indexer has not produced its first `ledgerstream` row, or `ingestion_cursors` is unreachable.
2. No `stellar.ledgers` row in that window: the lake is empty, wholly gapped below the chain position, or its tip stalled further back than the clamp accepts (a wedged CH sink, not the ordinary landing race).

The cursor routinely leads the lake by a few ledgers (Postgres is realtime, the CH sink lands seconds later); that lead is clamped away and does not reach this outcome, so a sustained `no_ledger` rate is one of the two conditions, not ordinary lag.

- Signal: `no_ledger` increments far exceed any other outcome; the wrapped error text in the log names which condition fired and which cursor supplied the bound.
- Mitigation: for (1) confirm the indexer runs and writes cursors; if storage is broken, route to `pg-conns-saturated.md`. For (2) compare `max(ledger_seq)` in `stellar.ledgers` with the `ledgerstream` cursor (empty or gapped lake vs stalled lake) and inspect archivist / galexie stack lag.

#### `outcome="no_observation"`

The chain reader (live LCM) returned no observation for at least one watched asset AND the operator-static fallback was empty. Most common after a fresh deploy: the AccountEntry observer has not backfilled deep enough.

- Signal: `no_observation` dominates; per-asset logs identify the uncovered watched accounts.
- Mitigation: (a) wait for the observer backfill (hours to days for the configured watched set), or (b) populate the operator-static blocks (`[supply.reserve_balances_stroops]`, `[metadata.issuer_home_domains]`) as a bridge.

#### `outcome="compute_error"`

The supply algorithm failed: Algorithm 1 (XLM) the reserve reader or XLMComputer threw; Algorithm 2 (classic) one of the four component sums failed; Algorithm 3 (SEP-41) the kind-totals query failed.

- Signal: `compute_error` increments without `no_observation` / `no_ledger`.
- Mitigation: read the wrapped error in the per-asset logs; typically a code bug or config inconsistency (asset not parseable). Roll back the binary if it is a recent deploy.

#### `outcome="write_error"`

`Store.InsertSupply` failed: Postgres unreachable, NUMERIC overflow on a malformed amount, etc. Confirm Postgres is reachable; check recent `asset_supply_history` rows for CHECK-constraint violations.

#### `outcome="stale_component"`

The F-1236 freshness gate rejected the snapshot: a per-component observation lags the snapshot's chain-tip ledger by more than `[supply] stale_component_ledgers` (default `1000` ledgers, about 85 min). The previous snapshot stays served.

The gate compares the always-advancing chain tip against `MinComponentLedger`, sourced from the change-driven classic/SEP-41 observers. Two different causes, not distinguishable by the gap alone; look at whether `MinComponentLedger` is moving:

1. **Stalled producer (real staleness).** The observer filling the component tables (trustlines / claimable_balances / liquidity_pools / sac_balances for classic; sep41_supply for SEP-41) is wedged or far behind; `MinComponentLedger` advances but cannot keep up, or regressed. This is what the gate is meant to catch.
   - Signal: the indexer's per-source freshness for that observer hypertable also lags; the `gap=...` in the WARN log does not stabilise around a fixed `min_component_ledger`.
   - Mitigation: treat as an observer-ingest stall; confirm the indexer is healthy and the observer progressing; route to the ingest-pipeline runbooks. Do NOT relax the gate to mask a stalled producer.
2. **Dormant asset (not staleness, F-1320; bounded to about 24 h, R-002).** A low-activity asset (governance tokens like PHO, niche classic credits) had no balance change, so `MinComponentLedger` is frozen and its last observation IS the current supply. Under the pre-F-1320 gate every future tick was rejected and the row went permanently stale (observed on PHO: gap 1017 -> 1324 and climbing). The refresher now treats an unchanged `MinComponentLedger` as dormant and accepts the snapshot (`outcome="dormant"`, row inserted) while the gap since it last moved stays within `DefaultMaxDormantComponentLedgers` (17,280 ledgers, about 24 h at 5 s close). Expect a single `stale_component` on the first tick after an aggregator restart for a quiet asset (cold start: dormant vs stalled is indistinguishable until a second tick), then it flips to `dormant`.

   Past the 24 h horizon the gate fails closed: a frozen `MinComponentLedger` is what a dead observer also looks like, so the tick is rejected with `stale_component`, publishing STOPS for that asset, and this alert fires. A sustained `stale_component` stream for one asset is therefore case 1 or a dormant asset past the horizon.
   - Signal: in the WARN log `min_component_ledger` is constant across ticks while `gap` climbs; `first_observation` is logged on the cold-start tick. Once `gap` exceeds the horizon the line becomes `supply refresh: rejecting snapshot — component ledger frozen past the dormancy horizon (stalled observer, not a dormant asset)` with a `dormancy_horizon` field instead of `first_observation`.
   - Discriminator (quiet asset vs dead observer): check the component observer itself. When did `min_component_ledger` last advance, and is the indexer's per-source freshness for the relevant observer hypertable otherwise healthy? Healthy and nothing to write = dormant; observer ingest stalled = case 1 even though the alert looks identical (route to ingest-pipeline runbooks). Do not widen the horizon to silence the alert without checking.
   - Mitigation: for the pre-horizon cold-start blip (or a binary predating F-1320), raise that asset's threshold (see [per-asset threshold override](#per-asset-threshold-override-stale_component-remedy)). For an asset confirmed legitimately dormant beyond 24 h (and monitored by other means), `[supply] max_dormant_component_ledgers = 0` restores legacy unbounded dormancy; it is global with no per-asset equivalent, so prefer raising that asset's `stale_component_ledgers` when only one asset needs it.

#### `outcome="missing_freshness"`

Strict-freshness mode (`[supply] strict_freshness_required = true`) rejected a snapshot with `MinComponentLedger == 0` (no freshness anchor: the static-XLM fallback, or a transiently failing freshness producer such as a Postgres/Redis blip).

- Signal: increments only when strict mode is enabled.
- Mitigation: confirm every freshness producer is wired and not failing. If the deployment legitimately runs the static fallback for some assets, leave strict mode off (the default) until the storage-backed readers cover the watched set.

#### Per-asset threshold override (`stale_component` remedy)

The global `[supply] stale_component_ledgers` is one number for all assets. Give a known low-activity asset a relaxed per-asset threshold rather than loosening the gate fleet-wide (which would let a stalled high-traffic asset like XLM or USDC through):

```toml
[supply.stale_component_ledgers_by_asset]
# asset_key (CODE:ISSUER for classic, bare contract id for SEP-41)
# = relaxed threshold in ledgers. ~5000 = 7 h.
"PHO:GAX5TXB5RYJNLBUR477PEXM4X75APK2PGMTN6KEFQSESGWFXEAKFSXJO" = 5000
```

Identify the asset from the `asset_key` label on `stellarindex_aggregator_supply_refresh_total`. The code option is `supply.WithStaleComponentLedgersFor(assetKey, maxLag)`, wired automatically from the TOML. Keys are canonicalised (`PHO-G...` and `native` resolve like `PHO:G...` and `XLM`). A key that does not parse, names an asset outside `watched_classic_assets` / `watched_sep41_contracts` (XLM is always watched), or duplicates another spelling of the same asset fails config validation at boot. A value of `0` disables the gate for that asset only. After F-1320 the override is no longer required to keep a dormant row fresh (the gate self-recovers via `dormant`); use it to silence the single cold-start blip or on a binary predating the fix.

### Mitigation

1. Identify the dominant outcome (Quick diagnosis 1).
2. Apply the matching fix above.
3. `no_observation` with the observer not yet backfilled is expected during bootstrap: wait or populate the operator-static fallback blocks.
4. Verification: `ok` rate exceeds the error-outcome rate; the alert clears within 30 min as the rolling window catches up.

False positives: bootstrap after a fresh deploy (`no_observation` dominates while the AccountEntry observer, Task #54, backfills; `for: 30m` usually absorbs it, longer bootstraps trip it and the alert may be silenced during deploy windows). A new entry in `watched_classic_assets` produces `no_observation` ticks until the observer has rows for its locked-set members; other assets keep ticking `ok`.

## stellarindex_aggregator_supply_refresh_dormant_fleet

Severity P3 (`ticket`), `for: 30m`. Same diagnostics as [error_dominant](#stellarindex_aggregator_supply_refresh_error_dominant); this alert exists because `dormant` is excluded from that one, which would stay silent until the 24 h horizon flips these assets to `stale_component`.

Expr: `count(sum by (asset_key) (rate(..._supply_refresh_total{outcome="dormant"}[15m])) > 0) > 1`, i.e. two or more assets report `dormant` together.

`MinComponentLedger` is not a per-asset age: classic assets share the slowest of the four component observers' `MAX(ledger)`, SEP-41 contracts share `MAX(ledger)` of `sep41_supply_events`. A quiet asset cannot freeze that watermark alone, so shared dormancy means the producer stopped advancing, and every tick re-stamps a frozen observation as current until the 24 h horizon (served supply goes stale silently).

- Signal: the dormant `asset_key`s are all classic (or all SEP-41) and their WARN/DEBUG logs carry the same `min_component_ledger`.
- Mitigation: check each component observer advances: `MAX(ledger)` of `trustline_observations`, `claimable_observations`, `lp_reserve_observations` and `sac_balance_observations` (or `sep41_supply_events`) against the chain tip. The lowest is the stalled producer; route to the ingest-pipeline runbooks as in [stale_component](#outcomestale_component) case 1. Do not raise per-asset thresholds to silence it.

## stellarindex_sep41_supply_rollup_no_cursor

Severity P3 (`ticket`), `for: 30m`, per `contract_id`. MTTR 15 min once the projector commits `sep41_supply` cycles. Impact: served SEP-41 supply stays exact, but every supply read for the contract re-sums its whole `sep41_supply_events` history, the Postgres load behind the 2026-07-06 incident (migration 0085 exists to prevent it).

Why: the aggregator's rollup worker folds only rows the projector has durably committed, reading the bound from `ingestion_cursors` row `(source='projector', sub_source='sep41_supply')`. When absent it fails closed: folds nothing and leaves `sep41_supply_rollup.last_ledger` where it is (often 0). Correctness holds but the checkpoint + delta fast path does not. Those passes are labelled `outcome="no_cursor"` on `stellarindex_sep41_supply_rollup_advances_total` so they are not mistaken for a dormant token's `noop`. Expr: `sum by (contract_id) (increase(stellarindex_sep41_supply_rollup_advances_total{outcome="no_cursor"}[15m])) > 0`.

Symptoms:

- `no_cursor` climbs for the `contract_id` with no `ok` increments.
- One WARN per contract on entering the state: `sep41 supply rollup pinned: projector cursor for sep41_supply is absent`.
- Postgres IO and `stellarindex_aggregator_supply_refresh_duration_seconds` p99 may climb while nothing else looks wrong.

Quick diagnosis (5 min):

```sh
psql -U stellarindex -d stellarindex -c \
  "SELECT source, sub_source, last_ledger, last_updated FROM ingestion_cursors WHERE source = 'projector' ORDER BY sub_source;"
psql -U stellarindex -d stellarindex -c \
  "SELECT contract_id, last_ledger, updated_at FROM sep41_supply_rollup ORDER BY contract_id;"
sudo journalctl -u stellarindex-indexer --since "30 min ago" | grep -i 'projector' | grep -i 'sep41_supply' | tail -20
```

If the first query shows a `sep41_supply` row the alert should clear on the next rollup pass; if not, check that the aggregator and projector point at the same database.

Mitigation (15 min):

- Get the projector committing `sep41_supply` cycles. It registers `sep41_supply` only when the indexer's config has a non-empty `[supply] watched_sep41_contracts`; an aggregator watching contracts the indexer does not causes this alert.
- If the projector runs but does not advance: [projector-wedged](projector.md#stellarindex_projector_wedged) or [projector-row-quarantined](projector.md#stellarindex_projector_row_quarantined).
- If `ingestion_cursors` was lost in a restore, do NOT insert a cursor row by hand: it claims settlement the projector never evidenced and the fold would permanently exclude any row still missing below it. Re-run the projection from a known ledger with `stellarindex-ops projector-replay -source sep41_supply` ([projector-replay](projector.md#stellarindex_projector_replay_stalled)).
- Verification: `ok` (or `noop`) increments resume on the next rollup pass; the alert resolves within 15 min.

Code: `AdvanceSEP41SupplyRollup` in `internal/storage/timescale/sep41_supply_events.go`; `runSEP41SupplyRollup` in `cmd/stellarindex-aggregator/main.go`. Rollup reset and re-fold: [sep41-mint-recovery](../sep41-mint-recovery.md).

## stellarindex_ch_supply_gapfill_failed

Severity P3 (`ticket`), `for: 10m`. MTTR 15-30 min. Detected via `node_systemd_unit_state` (node_exporter `--collector.systemd`): `node_systemd_unit_state{name="ch-supply.service",state="failed"} == 1`. Impact: the daily defensive forward-gap-fill of `stellar.supply_flows` (backs `/v1/assets` SEP-41 supply) failed. Served supply is likely still correct (live decode-at-ingest keeps it current), but the backstop is down and a real live-writer gap would go unhealed.

Why: `ch-supply.service` failed silently for weeks in 2026-07. The 2026-07-03 non-root hardening set `User=stellarindex` but the script still appended to a root-owned `/var/log/ch-supply-refresh.log` (`Permission denied` every run) and nothing alerted. Logging moved to journald (the script writes nothing to disk); this alert makes future failures loud.

Symptoms: the unit is `failed` for 10 min or more; the daily timer (`ch-supply.timer`, about 08:00) shows a failed last run.

Quick diagnosis (5 min):

```sh
ssh root@r1 'systemctl status ch-supply.service --no-pager'
ssh root@r1 'journalctl -u ch-supply.service -n 60 --no-pager'
```

Classify:

- `seed [X,Y] FAILED` (stderr): a `stellarindex-ops ch-supply` chunk errored; usually ClickHouse pressure (Phase-0 / heavy re-derive) or a transient CH error.
- `ch-supply: tip unresolved`: the Postgres `ingestion_cursors` tip query returned empty/0. Check Postgres and the `ledgerstream` cursor.
- `ch-supply: supply_flows watermark unresolved (got '...')`: the ClickHouse `max(ledger_seq)` probe failed (curl error on stderr above it) or returned non-digits. Check ClickHouse on `:8123` and that `stellar.supply_flows` exists; nothing was seeded.
- Any `Permission denied` / disk write: regression of the original bug; the script must not write to disk (see `run-ch-supply.sh`).

Mitigation (15 min):

- ClickHouse pressure: re-run off-peak with `systemctl start ch-supply.service`; the memory guard (`CHSUPPLY_MEMGUARD`) throttles it. Idempotent (ReplacingMergeTree key), safe to re-run.
- Tip unresolved: confirm the indexer is advancing (`ledgerstream` cursor in `ingestion_cursors`); fix upstream, then re-run.
- Verification: a clean run flips the metric to 0 and the alert clears within about 1 min of the next scrape. Confirm the watermark advanced toward tip: `ssh root@r1 'clickhouse-client --port 9300 -q "SELECT max(ledger_seq) FROM stellar.supply_flows"'`.

False positive: a single failure during an intense re-derive that the next daily run heals. `for: 10m` rides out the run window but not a persistent failed state; a sustained failure is real.

Script: `configs/ansible/roles/archival-node/files/run-ch-supply.sh`; unit: `templates/systemd/ch-supply.service.j2`. Sibling failed-unit alert: [`verify-archive.md#stellarindex_verify_archive_unit_failed`](verify-archive.md#stellarindex_verify_archive_unit_failed). Architecture: `docs/architecture/storage-considerations.md#supply-flows-in-the-lake`.

## Related

- [`supply-snapshot.md#stellarindex_supply_snapshot_never_initialized`](supply-snapshot.md#stellarindex_supply_snapshot_never_initialized): the timer-path never-initialized alert and the `runbook_url` target for `_never_initialized`.
- `supply-snapshot.md#stellarindex_supply_snapshot_stale`: systemd-timer-path equivalent (different metric, different expectation).
- ADR-0011 (three-domain supply algorithm), ADR-0021, ADR-0022, ADR-0023: algorithms and observer designs the refresher consumes.
