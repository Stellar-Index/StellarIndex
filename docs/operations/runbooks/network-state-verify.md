---
title: Runbook — network-state-verify
last_verified: 2026-10-03
status: draft
severity: P3
---

# Runbook — `stellarindex_network_state_verify_stale` / `stellarindex_network_state_verify_failed`

## At a glance

Network-state verify alerts.

P3 (`severity: ticket`). Rules: `configs/prometheus/rules.r1/storage.yml` (twin `deploy/monitoring/rules/storage.yml`). Impact: current state served from `ledger_entries_current` (account pages, holders, supply) may disagree with the network for the reported keys.

`verify-network-state.timer` runs `stellarindex-ops verify-network-state` daily at 13:47 UTC (+ up to 10 min jitter) via `run-heavy-job.sh`; it is the only check comparing lake-derived state with state the network publishes. The verdict goes to `/var/lib/node_exporter/textfile_collector/network_state_verify.prom`, written only after every check completed.

| Check (`check=`) | Compares | Fails when |
| ----- | ----- | ----- |
| `hot_archive` | Archived entries in the history archive's hot-archive buckets at the newest shared checkpoint vs their `ledger_entries_current` row | Body differs, or the lake row is a removal; or none could be compared. |
| `lumens` | Native XLM in accounts, claimable balances, liquidity pools and native-SAC balances plus `fee_pool`, vs `total_coins`, as of the newest `stellar.ledgers` row | Residual (held + fee_pool - total_coins) is not exactly 0. |

Upgrades can rewrite hot-archive entries without ledger meta (CAP-0076, ledger 59,501,299); only `hot_archive` sees that. Keys the lake lacks (`class="absent"`) or changed after the checkpoint (`class="newer"`) are counted in `stellarindex_network_state_verify_hot_archive_entries`, not failures.

## Check

```sh
systemctl list-timers verify-network-state.timer
systemctl status verify-network-state.service
journalctl -u verify-network-state --since -50h | tail -n 80
cat /var/lib/node_exporter/textfile_collector/network_state_verify.prom
```

`_failed{check="hot_archive"}`: `journalctl -u verify-network-state -n 300` lists up to 20 `MISMATCH` lines. `_failed{check="lumens"}`: the journal prints each domain's sum and the residual in stroops (negative = lake missing holdings; positive = lake holds lumens the network no longer counts).

## Likely causes

- `_stale` (no completed run in 48 h): timer not installed (apply the archival-node role with `--tags ops-jobs`), history archive unreachable (check `stellar.history_archive_url` and that it publishes the checkpoint named in the journal), or ClickHouse errors (fix ClickHouse; the old verdict stays until it ages out).
- `hot_archive`: network rewrote entries without ledger meta, or the lake is behind.
- `lumens` negative on a lake whose entry changes do not reach genesis: dormant holdings never captured.

## Fix

1. Date a `hot_archive` change by running at checkpoints either side of the suspect ledger: `stellarindex-ops verify-network-state -config /etc/stellarindex.toml -checks hotarchive -checkpoint 59501247` and `-checkpoint 59501311`. Clean before and failing after = a network rewrite. There is no repair verb (`ledger_entries_current` has one writer, the `ledger_entry_changes` view): record the keys and open an issue; the correction is code.
2. Lake behind (`verify-lake` also reports `entry_changes`): `stellarindex-ops ch-backfill -config PATH -from LO -to HI -bucket galexie-archive -write`, then re-run.
3. `lumens`: run `reconcile-balances` on a sample of accounts to find the short domain.
4. Verify: `systemctl start verify-network-state.service`; every `stellarindex_network_state_verify_failures` series reads 0.

## Seed entry state from a history-archive checkpoint

For entries dormant before the lake's capture window (ADR-0021). Reads one checkpoint's state into `ledger_entry_changes`. Size with `-dry-run`; `-write` needs `-limit 0`.

```sh
stellarindex-ops state-snapshot -config /etc/stellarindex.toml -scope contracts -limit 0 -dry-run
stellarindex-ops state-snapshot -config /etc/stellarindex.toml -scope contracts -limit 0 -write
```

## Related

[lake-verify](lake-verify.md), [alerts-catalog](../alerts-catalog.md)
