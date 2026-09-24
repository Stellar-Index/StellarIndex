---
title: Runbook — stellarindex_ch_schema_probe_absent
last_verified: 2026-09-24
status: draft
severity: P3
---

# Runbook — `stellarindex_ch_schema_probe_absent`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ch_schema_probe_absent` (P3 / ticket) |
| Detected by | Prometheus rule in `deploy/monitoring/rules/api.yml` and the R1 single-host overlay `configs/prometheus/rules.r1/api.yml`. |
| Typical MTTR | 5–30 min: create or backfill the named lake object, then restart the API if the verdict latched. |
| Impact | One explorer surface is served from its slow fallback (full-table or bloom-index scans) or 503s, depending on the probe. Nothing is wrong on the wire; it is slower or unavailable. |

## Why this exists

The API's explorer reader (`internal/storage/clickhouse/explorer_reader.go`)
asks ClickHouse whether each optional lake object exists before it uses
it — the `tx_hash_index` fast path, the rollups behind `/v1/accounts`,
the `version` column on `ledger_entries_current`, and so on. When the
lake answers "no such table/column" to a process that has never seen
the object, the probe keeps that answer until the process restarts. An
API that starts before the lake DDL is applied therefore stays on the
fallback path indefinitely. Before `stellarindex_ch_schema_probe_present`
existed that verdict was visible nowhere.

## What this fires on

`stellarindex_ch_schema_probe_present{probe="…"} == 0` for 30 minutes on
one API instance. The gauge moves only on an **answer** from the lake:

- `0` — the object is absent, or (for a row-requiring probe) exists but
  is empty.
- `1` — the object is there and usable.

A lake that is not answering at all does not move the gauge; that shows
as a rising `stellarindex_ch_schema_probe_unanswered_total` and, usually,
`stellarindex_clickhouse_server_down`.

The `probe` label names the object:

| `probe` | Lake object |
| ------- | ----------- |
| `tx_hash_index` | `stellar.tx_hash_index` |
| `contract_active_ledgers` | `stellar.contract_active_ledgers` |
| `contract_instance_changes` | `stellar.contract_instance_changes` |
| `contract_instance_changes_tx_key` | `tx_hash` + `intra_ledger_seq` on `stellar.contract_instance_changes` |
| `contracts_census_daily` | `stellar.contracts_census_daily` |
| `accounts_stats` | `stellar.accounts_stats` |
| `account_creators_rollup` | `stellar.account_creators_rollup` |
| `account_sponsors_rollup` | `stellar.account_sponsors_rollup` |
| `account_creator_edges` | `stellar.account_creator_edges` |
| `account_sponsor_edges` | `stellar.account_sponsor_edges` |
| `asset_holders_rollup` | `stellar.asset_holders_rollup` |
| `ops_by_source` | `stellar.ops_by_source` |
| `account_activity` | `stellar.account_activity` |
| `ledger_entries_current_version` | `version` column on `stellar.ledger_entries_current` |

## Quick diagnosis (≤ 5 min)

```sh
# 1. Which probes read 0 on this API?
ssh r1 'curl -s http://localhost:3000/metrics | grep ^stellarindex_ch_schema_probe'

# 2. Does the object exist, and does it hold rows? (substitute the table)
clickhouse-client --port 9300 -q "EXISTS TABLE stellar.tx_hash_index"
clickhouse-client --port 9300 -q "SELECT count() FROM stellar.tx_hash_index"

# 3. For a column probe, is the column there?
clickhouse-client --port 9300 -q "SELECT name FROM system.columns
  WHERE database = 'stellar' AND table = 'ledger_entries_current' AND name = 'version'"
```

- Object missing: the deployment has not applied its DDL (see
  `deploy/clickhouse/`). Go to Mitigation A.
- Object present and populated, gauge still `0`: the verdict latched
  before the DDL landed. Go to Mitigation B.
- Object present but empty: a backfill has not run or the table was
  truncated. Go to Mitigation C.

## Mitigation (≤ 15 min)

### A. The object does not exist

- [ ] Apply the object's DDL from `deploy/clickhouse/` (the file named
      after the table, or the one that adds the column).
- [ ] Continue with B — a process that answered "absent" before the
      object existed does not pick it up by itself.

### B. The verdict latched

- [ ] `ssh r1 'sudo systemctl restart stellarindex-api'`
- [ ] Verification: the probe's gauge reads `1` after the first request
      that exercises that surface, and the alert clears.

### C. The object is empty

- [ ] Run the backfill documented beside the object's DDL. A row-requiring
      probe re-asks every few seconds under traffic, so no restart is
      needed once rows exist.

## Known false-positive patterns

- **No traffic since the object was repopulated.** Probes run only when a
  request needs them; the gauge keeps the last answer until then. One
  request to the affected route settles it.
- **A deployment that deliberately omits an optional object.** The
  fallback is correct there; the alert is a standing ticket until the
  object is built. Silence it with an expiry, not indefinitely.

## Related

- [clickhouse-server-health](clickhouse-server-health.md) — the lake
  itself being down or failing queries.
- `docs/reference/metrics/README.md` — `stellarindex_ch_schema_probe_present`
  and `stellarindex_ch_schema_probe_unanswered_total`.
