---
title: Runbook — supply-verify-rollup-unit-failed
last_verified: 2026-09-28
status: draft
severity: P3
---

# Runbook — `stellarindex_supply_verify_rollup_unit_failed_alert`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_supply_verify_rollup_unit_failed_alert` |
| Severity | P3 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/supply-verify-rollup.yml` (group `stellarindex.supply_verify_rollup`, `for: 30m`); multi-host twin in `deploy/monitoring/rules/supply-verify-rollup.yml`. |
| Typical MTTR | 30–60 min (a fold rebuild runs one contract at a time) |
| Impact | Served SEP-41 supply (`/v1/assets/{id}` and `/supply`) for a drifted contract is read from a fold that no longer matches its source events, so it can be wrong until the fold is rebuilt. |

## Why this exists

`stellarindex-ops supply verify-rollup` re-sums `sep41_supply_events` for
each watched contract at its `sep41_supply_rollup` checkpoint and diffs that
against the fold. It runs daily from `supply-verify-rollup.timer` and writes
`/var/lib/node_exporter/textfile_collector/supply_verify_rollup.prom`, so
"the rollup reconciles" is a scraped fact rather than a pasted transcript.

## Symptoms

`stellarindex_supply_verify_rollup_unit_failed == 1`. The run failed for one
of four reasons, told apart by the gauges it wrote:

| `drift_total` | `missing_total` | `checked_total` | Cause |
| --- | --- | --- | --- |
| > 0 | any | > 0 | A checkpoint diverges from the re-sum. |
| 0 | > 0 | any | A watched contract has no `sep41_supply_rollup` row — unexamined, not clean. |
| 0 | 0 | 0 | Nothing was checked (empty rollup table, or `-contracts` matched nothing). |
| absent | absent | absent | The run errored before counting (config, Postgres, timeout). |

## Quick diagnosis (≤ 5 min)

```sh
cat /var/lib/node_exporter/textfile_collector/supply_verify_rollup.prom
journalctl -u supply-verify-rollup.service -n 100 --output=cat
```

The journal names every drifted `(contract, kind)` and every missing contract.

## Mitigation

- **Drift:** rebuild the named contracts' fold. `stellarindex-ops supply
  seed-sep41-genesis -config /etc/stellarindex.toml -write` rebuilds the fold
  under the genesis baseline in one transaction per contract (the W5.4
  procedure in [v1-launch-plan.md](../v1-launch-plan.md)). Then re-run
  `systemctl start supply-verify-rollup.service` and confirm
  `unit_failed 0`.
- **Missing:** the contract is in `[supply] watched_sep41_contracts` but has
  never been folded. Check the rollup worker first —
  [sep41-supply-rollup-no-cursor](supply-refresh.md#stellarindex_sep41_supply_rollup_no_cursor) — then
  seed it as above.
- **Nothing checked:** the role installs this job only where
  `stellarindex_watched_sep41_contracts` is non-empty
  (`supply_verify_rollup_enabled`), so the watched set has no rollup rows at
  all. Treat as missing.
- **Error:** fix the cause in the journal; a timeout means raising
  `RUN_TIMEOUT` in `/etc/default/supply-verify-rollup` (and
  `TimeoutStartSec` in the unit with it).

## Known false-positive patterns

None known. A failed run is never promoted to clean.

## Related

- Implementation: `internal/ops/supply/supply_verify_rollup.go`,
  `internal/supply/textfile.go`.
- Unit: `configs/ansible/roles/archival-node/templates/systemd/supply-verify-rollup.{service,timer}.j2`.
- Companion: [supply-verify-rollup-stale](supply-verify-rollup-stale.md) —
  no clean run for 36 h, or never.
- [sep41-supply-rollup-no-cursor](supply-refresh.md#stellarindex_sep41_supply_rollup_no_cursor) — the
  fold is not advancing at all.
