---
title: Runbook — ch-supply-gapfill-failed
last_verified: 2026-07-17
status: draft
severity: P3
---

# Runbook — `stellarindex_ch_supply_gapfill_failed`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ch_supply_gapfill_failed` |
| Severity | P3 (ticket) |
| Detected by | Prometheus rule in `configs/prometheus/rules.r1/supply-refresh.yml` (via `node_systemd_unit_state`, node_exporter `--collector.systemd`) |
| Typical MTTR | 15–30 min |
| Impact | The daily **defensive** forward-gap-fill of `stellar.supply_flows` (backs `/v1/assets` SEP-41 supply) failed. Served supply is likely still correct (the live decode-at-ingest path keeps it current), but the backstop is down — a real live-writer gap would go unhealed. |

## Why this exists

`ch-supply.service` failed **silently for weeks** in 2026-07: the 2026-07-03
non-root hardening changed the unit to `User=stellarindex`, but the script still
appended to a root-owned `/var/log/ch-supply-refresh.log` → `Permission denied`
every run, and **nothing alerted on it**. The fix moved logging to journald
(`writes nothing to disk`, matching the unit's hardening); this alert makes any
future failure loud.

## Symptoms

- `node_systemd_unit_state{name="ch-supply.service",state="failed"} == 1` for ≥ 10 min.
- The daily timer (`ch-supply.timer`, ~08:00) shows a failed last run.

## Quick diagnosis (≤ 5 min)

```sh
ssh root@r1 'systemctl status ch-supply.service --no-pager'
ssh root@r1 'journalctl -u ch-supply.service -n 60 --no-pager'   # logs now live here
```

Classify the failure:
- **`seed [X,Y] FAILED`** (stderr) — a `stellarindex-ops ch-supply` chunk errored. Usually ClickHouse pressure (Phase-0 / heavy re-derive) or a transient CH error.
- **`ch-supply: tip unresolved`** — the Postgres `ingestion_cursors` tip query returned empty/0. Check Postgres + the `ledgerstream` cursor.
- **Any `Permission denied` / disk write** — a regression of the original bug; the script must not write to disk (see `run-ch-supply.sh`).

## Mitigation (≤ 15 min)

- [ ] If ClickHouse pressure (Phase-0 window): re-run off-peak — `systemctl start ch-supply.service`; the memory-guard (`CHSUPPLY_MEMGUARD`) throttles it. It is idempotent (ReplacingMergeTree key), so a re-run is safe.
- [ ] If tip-unresolved: confirm the indexer is advancing (`ledgerstream` cursor in `ingestion_cursors`); fix upstream, then re-run.
- [ ] **Verification:** a clean run flips `node_systemd_unit_state{...,state="failed"}` to 0; the alert clears within ~1 min of the next scrape. Confirm `SELECT max(ledger_seq) FROM stellar.supply_flows` advanced toward tip.

## Known false-positive patterns

- A single failure during an intense re-derive (ClickHouse busy) that the next daily run heals. The `for: 10m` rides out the run window but not a persistent failed state — a sustained failure is real.

## Related

- Script: `configs/ansible/roles/archival-node/files/run-ch-supply.sh`; unit: `templates/systemd/ch-supply.service.j2`.
- Architecture: `docs/architecture/clickhouse-supply-from-ch.md`.
- Sibling failed-unit alert (same `node_systemd_unit_state` pattern): [`verify-archive-unit-failed.md`](verify-archive-unit-failed.md).

## Changelog

- 2026-07-17 — initial draft; added alongside the journald log-perm fix that ended the silent-failure regression.
