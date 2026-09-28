---
title: Runbook — supply-verify-rollup-stale
last_verified: 2026-09-28
status: draft
severity: P3
---

# Runbook — `stellarindex_supply_verify_rollup_stale` / `_never_initialized`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_supply_verify_rollup_stale`, `stellarindex_supply_verify_rollup_never_initialized` |
| Severity | P3 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/supply-verify-rollup.yml`; multi-host twin in `deploy/monitoring/rules/supply-verify-rollup.yml`. |
| Typical MTTR | 15 min |
| Impact | No customer impact by itself: the SEP-41 supply rollup is simply no longer certified to reconcile with its source events. |

## Symptoms

- `_stale`: `stellarindex_supply_verify_rollup_last_success_timestamp` is
  older than 36 h (daily cadence + 12 h). Every run since has failed, or the
  timer stopped.
- `_never_initialized`: the series has not existed for 36 h. The timer was
  never installed, node_exporter is not reading the textfile directory, or no
  run has ever passed clean — a failed run writes no
  `last_success_timestamp`.

## Quick diagnosis (≤ 5 min)

```sh
systemctl list-timers supply-verify-rollup.timer
systemctl status supply-verify-rollup.service
cat /var/lib/node_exporter/textfile_collector/supply_verify_rollup.prom
```

- Timer absent: the archival-node role has not been applied since the unit
  landed. Apply with `--tags ops-jobs`.
- File present with `unit_failed 1`: follow
  [supply-verify-rollup-unit-failed](supply-verify-rollup-unit-failed.md).
- File absent after a run: check `TEXTFILE_OUTPUT` in
  `/etc/default/supply-verify-rollup` and the unit's `ReadWritePaths`.

## Mitigation

- [ ] Get one clean run: `systemctl start supply-verify-rollup.service`.
- [ ] Verification: `last_success_timestamp` advances and both alerts clear
      within one scrape plus `for: 5m`.

## Related

- Companion: [supply-verify-rollup-unit-failed](supply-verify-rollup-unit-failed.md).
- Sibling pattern: [supply-snapshot-stale](supply-snapshot-stale.md),
  [supply-snapshot-never-initialized](supply-snapshot-never-initialized.md).
