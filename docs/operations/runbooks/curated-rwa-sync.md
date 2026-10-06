---
title: Runbook — curated-rwa-sync
last_verified: 2026-10-06
status: ratified
severity: P3
---

# Runbook — curated-rwa-sync

Alerts: `stellarindex_curated_rwa_sync_stale`, `stellarindex_curated_rwa_sync_refused`, `stellarindex_curated_rwa_published_stale`. All P3 (`severity: ticket`), routed by `configs/alertmanager/alertmanager.r1.yml`. Rules: `deploy/monitoring/rules/curated-rwa-sync.yml` and `configs/prometheus/rules.r1/curated-rwa-sync.yml` (byte-identical; group `stellarindex.curated_rwa_sync`).

- **Impact:** a comparison panel degrades; the verified RWA surface is untouched. The reader is fail-closed: a row whose `synced_at` is older than 48 h is not served, so `/v1/rwa/assets` reports `curated.status: unavailable` and the explorer panel says so. 30 h (one missed daily run plus jitter) fires with 18 h to spare.
- **Scope:** r1 / pubnet (Dune's Stellar datasets are pubnet). The unit is installed on every network; a test net with no key stays `unwired` by design and these alerts do not fire there, because the metric is stamped on a dry run too. A test net whose unit never stamps will fire `_stale` (evaluated per instance): silence it in Alertmanager with an `instance` matcher if that net carries no key.
- **Metric source:** `node_exporter` textfile_collector reads `/var/lib/node_exporter/textfile_collector/curated_rwa_sync.prom`, written by `stellarindex-ops curated-rwa-sync` at the end of every run (dry or wet, success or refusal) from `curated-rwa-sync.timer` (daily 04:12 UTC, `RandomizedDelaySec=600`). A failed read or write stamps nothing.
- **Steady state:** `stellarindex_curated_rwa_sync_last_run_unix` advances daily; `_rows` ≈ 220–230 (monthly total series ~13 points plus per-subclass split ~210 rows); `_datapoints_read` a small constant (Dune metering for two result reads; a run never executes a query); `_executed_at_unix` advances about daily on the curator's own schedule.
- **Companion:** [api-smoke-stale](api-smoke.md#stellarindex_api_smoke_stale) (same textfile-stamp pattern, same diagnosis order); `docs/methodology/rwa-coverage-reconciliation.md` § *The curated arm*.

## At a glance

- [`stellarindex_curated_rwa_sync_stale`](#stellarindex_curated_rwa_sync_stale)
- [`stellarindex_curated_rwa_sync_refused`](#stellarindex_curated_rwa_sync_refused)
- [`stellarindex_curated_rwa_published_stale`](#stellarindex_curated_rwa_published_stale)

## Quick diagnosis

```sh
# 1. Timer scheduled? When did the unit last run?
ssh r1 'systemctl list-timers curated-rwa-sync.timer'
ssh r1 'systemctl show curated-rwa-sync.service -p Result,InactiveEnterTimestamp,ExecMainStartTimestamp'
# Type=oneshot: read Result WITH its timestamp. Empty InactiveEnterTimestamp = no run for Result to describe.

# 2. What did the last run say?
ssh r1 'journalctl -u curated-rwa-sync -n 40 --no-pager'

# 3. Textfile present and moving?
ssh r1 'ls -la /var/lib/node_exporter/textfile_collector/curated_rwa_sync.prom'
ssh r1 'cat /var/lib/node_exporter/textfile_collector/curated_rwa_sync.prom'

# 4. Key present? (never print it)
ssh r1 'grep -c "^DUNE_API_KEY=." /etc/default/curated-rwa-sync'   # 1 = set, 0 = empty placeholder

# 5. Arm state per the API
curl -s https://api.stellarindex.io/v1/rwa/assets | jq '.curated | {status, assets, census, published: (.published | {total_usd, as_of, executed_at, gap_vs_verified_usd})}'
```

## stellarindex_curated_rwa_sync_stale

Trips (`for: 10m`):

```
(time() - stellarindex_curated_rwa_sync_last_run_unix) > (30 * 3600)
or
(
  absent_over_time(stellarindex_curated_rwa_sync_last_run_unix[30h])
  and on() (count_over_time(up{job="node_exporter"}[30h]) > 1700)
)
```

The absent arm is gated on Prometheus having watched 30 h of scrapes (~1,700 samples at 60 s is 28 h+), so a fresh rule load or Prometheus restart does not fire it. If it fires on a host that never stamped, the timer has not fired in 30 h: go to step 1. The two rule trees are identical.

Means: no run has stamped `last_run_unix` in 30 h, or none ever has (timer not scheduled, unit failing before it writes, Dune read failing, textfile dir not writable). At 48 h the reader empties the arm.

Triage, in order:

1. **Timer not listed, or `NEXT` is `n/a`:** the timer was not enabled (deploy skipped task 14, or host bootstrapped before the unit existed). `systemctl enable --now curated-rwa-sync.timer`; the ansible role is the source of record, so re-run the archival-node playbook if it drifts again.
2. **Journal `GET /api/v1/query/…/results: HTTP 4xx`:** Dune refused the read. `401`/`403`: key rotated or revoked; replace it (see `_refused` fix). `402`/`429`: credit allowance exhausted. A run reads two public query results, metered by datapoint (`datapoints_read`), never by execution, so this is almost always another consumer of the same key. Wait for the monthly reset or lower the cadence in `curated-rwa-sync.timer.j2`; the 48 h bound tolerates one missed day, not more. `404`: the curator deleted or privatised the query; find its successor at dune.com/stellar/rwas. The ids are constants in `internal/ops/ingest/curated_rwa_sync.go`.
3. **Journal `latest execution is QUERY_STATE_…, not completed`:** the curator's own scheduled run failed. Nothing to do here: the previous day's rows stay served until the 48 h bound and the next successful curator run clears it. If it persists past a day the dashboard is broken: say so on the page's issue, not in code.
4. **Journal `printed no usable row` / `printed N of the M rows it declared`:** the curator changed a column name/layout, or its paging broke. Rows are decoded STRICTLY (`duneMonthlyTotalRow`, `duneMonthlyBySubclassRow` in `internal/ops/ingest/curated_rwa_sync.go`); compare with the query's current columns on dune.com. A layout change is a code change with a test, not an ops fix.
5. **Journal `Read N rows; kept …` then a Postgres error:** curator read, cache write failed (DSN, pool, constraint, or `-timeout` expiring mid-commit). A failed write stamps nothing, so `last_run_unix` keeps the last committed run's time. Fix the database side, re-run the unit.
6. **Textfile written but `last_run_unix` frozen:** node_exporter is not scraping the directory (file permission, or collector flag missing). `ls -la` the file; check node_exporter args for `--collector.textfile.directory`.
7. **All healthy and still firing:** the r1 scrape target (node_exporter) is down; the alert is a symptom.

Clears on its own `for: 10m` after the next scrape of a fresh `last_run_unix`.

## stellarindex_curated_rwa_sync_refused

Trips:

```
stellarindex_curated_rwa_sync_refused == 1
```

`for: 2h`. Identical in both trees.

Means: the run exited clean without reading the curator because `DUNE_API_KEY` is empty in `/etc/default/curated-rwa-sync`. It still stamps `last_run_unix` (so `_stale` measures the timer, not the key). Expected state of a fresh install until an operator sets the key. Confirm: step 4 above prints `0`. The arm reads `unwired`/`unavailable` meanwhile.

Fix: the file is `root:root` mode `0600` (only systemd reads it, as PID 1; no group read unlike sibling `/etc/default/*` files). Set the key the way the role does so the next apply agrees with the host: set `vault_dune_api_key` in `inventory/r1.secrets.yml` (workstation, `configs/ansible/`), then

```sh
ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
  --tags stellarindex --check --diff            # always --check --diff first
```

The role renders `DUNE_API_KEY={{ vault_dune_api_key }}` whenever the vault defines a non-empty value; with the variable undefined it installs the empty placeholder once and never overwrites a pasted value. Pasting by hand works, but the vault is the source of record: a hand-set key is replaced by the vault's on the first apply after the variable is defined. Then `systemctl start curated-rwa-sync.service` and re-read the textfile; the gauge clears on that run. The reader recognises the arm within its 10-minute cache TTL.

## stellarindex_curated_rwa_published_stale

Trips:

```
(time() - stellarindex_curated_rwa_sync_executed_at_unix) > (72 * 3600)
and
stellarindex_curated_rwa_sync_executed_at_unix > 0
```

`for: 1h`. Identical in both trees. `executed_at_unix` is 0 when a run read nothing; the `_refused`/`_stale` rules own that case.

Means: the sync is healthy (`last_run_unix` advances, arm recognised) but the CURATOR has stopped re-executing its public query, so the figure is frozen. Healthy worst case is ~48 h (daily curator read by daily sync, held a day), so 72 h is a full missed curator day beyond that. The API's `curated.published.executed_at` shows the same stamp. Past 48 h the API serves the block with `stale: true`; past 7 days it drops `curated.published` entirely (`curated.status` reads `unavailable` when no per-asset row is readable either). The split is a separate query: `curated.published.by_subclass_executed_at` is its own execution time, and a split older than 7 days is withheld while a fresher total is still served.

Fix: nothing on this side. Confirm on dune.com that the curator's query schedule is paused or broken.

## Related

- `internal/ops/ingest/curated_rwa_sync.go`: sync command (query ids, paging loop, strict row shapes, textfile writer).
- `internal/storage/timescale/rwa_curated_published.go`: reader with the 48 h recognition bound; `internal/api/v1/rwa_curated.go` turns an empty set into `curated.status: unavailable` and a full one into `published_totals`. (`rwa_curated_directory.go` is the per-asset reader; the first curator's per-asset tables are private, so it stays empty.)
- `configs/ansible/roles/archival-node/templates/systemd/curated-rwa-sync.service.j2`: unit, `EnvironmentFile`, `ReadWritePaths` grant for the textfile directory.
- `configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml`: the two tasks (vault render / first-install placeholder) owning `/etc/default/curated-rwa-sync`.
