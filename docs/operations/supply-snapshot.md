---
title: Supply-snapshot writer — daily cron + operator-managed reserve balances
last_verified: 2026-05-03
status: living procedure
---

# Supply-snapshot writer — daily cron + operator-managed reserve balances

Operational companion to [ADR-0011](../adr/0011-supply-algorithm.md).
Code: `cmd/stellarindex-ops/supply.go` (`supply snapshot`),
`internal/supply/config_reader.go` (`ReserveBalanceReader`).

## Purpose

`/v1/assets/{id}` serves `total_supply`, `circulating_supply`,
`max_supply`, `market_cap_usd`, `fdv_usd`, `supply_basis` from the
`asset_supply_history` hypertable. Empty table = JSON null fields.

Two producers, both insert into `asset_supply_history`; the handler
reads the latest row per asset:

- systemd-timer CLI snapshot (XLM only).
- aggregator-resident refresher (`[supply].aggregator_refresh_enabled`):
  every watched asset, all three ADR-0011 algorithms.

## Why operator-managed reserve balances

ADR-0011 Algorithm 1:

```
total_supply       = 50,001,806,812 × 10^7 stroops      (frozen 2019)
max_supply         = total_supply                        (XLM is hard-capped)
circulating_supply = total_supply − Σ(SDF reserve balances)
```

Only the SDF reserve sum moves. The writer reads it from the LCM
AccountEntry observer (`account_observations`) when every watched
account has rows; otherwise it falls back to the static
`[supply].reserve_balances_stroops` map (chained-fallback reader:
[supply-pipeline](../architecture/supply-pipeline.md#the-chained-fallback-reader-pattern)).
The static map is a bring-up cushion; it may be empty once the observer
has caught up.

```toml
[supply]
sdf_reserve_accounts = [
  "GA5XIGA5C7QTPTWXQHY6MCJRMTRZDOSHR6EFIBNDQTCQHG262N4GGKTM",
  "GBLDBN3QQAA2QAH7ZQI6LQ5TXGMVCOATJYBSXQYDQB7ZUR3OVF5JEHO5",
  # … one entry per active SDF reserve account, per latest SDF announcement
]

[supply.reserve_balances_stroops]
GA5XIGA5C7QTPTWXQHY6MCJRMTRZDOSHR6EFIBNDQTCQHG262N4GGKTM = "12345678900000000"
GBLDBN3QQAA2QAH7ZQI6LQ5TXGMVCOATJYBSXQYDQB7ZUR3OVF5JEHO5 = "98765432100000000"
```

Writer start validates that **every** `sdf_reserve_accounts` entry has a
`reserve_balances_stroops` entry. Missing = hard fail (treating unknown
as zero would overstate circulating supply, which ADR-0011 prohibits).

### When SDF announces a reserve move

Bootstrap-only once the observer covers the operator set.

1. Wait for SDF's public announcement (account + stroop amount).
2. Edit `[supply.reserve_balances_stroops]`; add new accounts to
   `sdf_reserve_accounts`.
3. Next timer fire picks it up; no restart.
4. Optional out-of-cadence run: `sudo systemctl start supply-snapshot.service`

### Live LCM-derived reserve balances (shipped)

Each run chains `LCMReserveBalanceReader` (latest observation
at-or-before the attributed ledger) with `ConfigReserveBalanceReader` as
fallback. If every reserve account has an observation, the live sum wins
and the static config is not consulted. If any account lacks one, or
storage errors transiently, the whole call drops to the static reader.

## Daily cron

### Files

- `deploy/systemd/supply-snapshot.timer`: `OnCalendar=04:42 UTC` daily,
  up to 5 min jitter (after archive-completeness verify 02:17 and
  verify-archive-tier-a 03:23).
- `deploy/systemd/supply-snapshot.service`: runs
  `stellarindex-ops supply snapshot -config $CONFIG_PATH -asset $ASSET`.

### Operator wiring

```sh
sudo cp deploy/systemd/supply-snapshot.{service,timer} /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now supply-snapshot.timer
```

Override via `/etc/default/supply-snapshot`:

```sh
CONFIG_PATH=/etc/stellarindex.toml      # default
ASSET=native                            # default; only `native` at v1
EXTRA_FLAGS="-ledger 50000000"          # pin to a specific ledger
                                        # (default: max from ingestion_cursors)
```

### Pre-flight: dry-run

Before enabling the timer:

```sh
sudo -u stellarindex /usr/local/bin/stellarindex-ops supply snapshot \
  -config /etc/stellarindex.toml -dry-run
```

Prints `total_supply` / `circulating_supply` / `max_supply` / `basis` /
`ledger_sequence`. `total_supply − circulating_supply` must equal the
latest SDF-announced reserve sum; if not, fix
`reserve_balances_stroops` before enabling the cron.

## Asset-class scope

| Asset class       | Algorithm                               | Status   | Reference |
| ----------------- | --------------------------------------- | -------- | --------- |
| Native XLM        | 1 — `total − Σ(SDF reserve balances)`   | Shipped  | ADR-0011  |
| Classic credit    | 2 — `Σ trustline+claimable+LP+SAC`      | Shipped  | ADR-0022  |
| SEP-41 Soroban    | 3 — `Σ mint − Σ burn − Σ clawback`      | Shipped  | ADR-0023  |

The aggregator refresher covers XLM (always), classic
(`watched_classic_assets`) and SEP-41 (`watched_sep41_contracts`): one
`Refresher.Tick` goroutine per asset, outcomes counted in
`stellarindex_aggregator_supply_refresh_total`. The CLI supports
`-asset native` only.

## Textfile-collector integration

`-textfile-output PATH` writes a Prometheus textfile per run
(`<path>.tmp` then rename; node_exporter skips `.tmp`):

```sh
# /etc/default/supply-snapshot
TEXTFILE_OUTPUT=/var/lib/node_exporter/textfile_collector/supply_snapshot.prom
```

### Metric set

```
stellarindex_supply_snapshot_total_xlm{asset_key=}             gauge   XLM
stellarindex_supply_snapshot_circulating_xlm{asset_key=}       gauge   XLM
stellarindex_supply_snapshot_max_xlm{asset_key=}               gauge   XLM (only when set)
stellarindex_supply_snapshot_ledger{asset_key=}                gauge   ledger seq
stellarindex_supply_snapshot_observed_at_seconds{asset_key=}   gauge   unix
stellarindex_supply_snapshot_run_duration_seconds              gauge   seconds
stellarindex_supply_snapshot_unit_failed{asset_key=}           gauge   1 on fail, 0 on pass
stellarindex_supply_snapshot_last_success_timestamp{asset_key=} gauge  unix; only on pass
```

Values are in **XLM**, not stroops (float64; sub-stroop precision lost,
fine for monitoring). The hypertable keeps full NUMERIC.

### Alerts

`deploy/monitoring/rules/supply-snapshot.yml`; each has a runbook under
`docs/operations/runbooks/supply-snapshot-*.md`.

| Alert | Condition | Severity |
|-------|-----------|----------|
| `stellarindex_supply_snapshot_unit_failed_alert` | unit_failed=1 sustained 30 min | P3 ticket |
| `stellarindex_supply_snapshot_stale` | last_success > 36 h | P3 ticket |
| `stellarindex_supply_snapshot_critical_stale` | last_success > 72 h | **P2** page |
| `stellarindex_supply_snapshot_circulating_zero` | circulating ≤ 0 (XLM only) | **P2** page |

## Verifying it ran

```sh
stellarindex-ops supply audit native -config /etc/stellarindex.toml
```

Prints `total_supply` / `circulating` / `max_supply` / `basis` /
`ledger_sequence` / `observed_at`. A second daily run should show the
same `circulating_supply` (same config) and a newer `ledger_sequence`.
Divergence with no config edit: see
[supply-cross-check-divergence](runbooks/supply.md#stellarindex_supply_cross_check_divergence).

## Why daily, not hourly

Values change only on operator config edits (a few times a year); daily
keeps `observed_at` fresh enough.

## Aggregator-resident goroutine path (preferred once observer is backfilled)

```toml
[supply]
aggregator_refresh_enabled = true
aggregator_refresh_cadence = "5m"
```

Runs the same chained-fallback reader in-process; `observed_at` tracks
the current ledger within the cadence instead of a day. Mutually
exclusive with the timer: disable it with
`sudo systemctl disable --now supply-snapshot.timer` (double-writes are
idempotent on conflict, just wasteful).
