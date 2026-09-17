---
title: Runbook — curated-rwa-sync stale
last_verified: 2026-09-17
status: ratified
severity: P3
---

# Runbook — `stellarindex_curated_rwa_sync_stale`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_curated_rwa_sync_stale` (P3, `severity: ticket`), routed by `configs/alertmanager/alertmanager.r1.yml` |
| Severity | P3 — a comparison panel degrades; the verified RWA surface beside it is untouched |
| Scope | r1 / pubnet — the curated arm is only meaningful where the curator's list exists (Dune's Stellar datasets are pubnet). The unit is installed on every network but a test net with no key stays `unwired` by design and this alert does not fire there, because the metric is stamped on a dry run too. |
| Detected by | `deploy/monitoring/rules/curated-rwa-sync.yml` + `configs/prometheus/rules.r1/curated-rwa-sync.yml` (byte-identical; group `stellarindex.curated_rwa_sync`) |
| Metric source | `node_exporter` textfile_collector reads `/var/lib/node_exporter/textfile_collector/curated_rwa_sync.prom`, written by `stellarindex-ops ingest curated-rwa-sync` at the end of every run — dry or wet, success or refusal — from `curated-rwa-sync.timer` (daily, 04:12 UTC) |
| Steady-state | `last_run_unix` advances once a day; `assets` ≈ 30–40 rows (the curator's recognised set ⋈ its latest priced day); `execution_cost_credits` a small constant per run |
| Customer impact | None on verified figures. `/v1/rwa/assets` reports `curated.status: unavailable` once the 48-hour recognition bound passes and the explorer's curated panel says the comparison is unavailable rather than showing stale rows |
| Companions | [api-smoke-stale](api-smoke-stale.md) (same textfile-stamp pattern), `docs/methodology/rwa-coverage-reconciliation.md` § *The curated arm* |

## Why this exists

The curated arm exists so the RWA page can be compared with an external
curator's figure on one screen. Its reader is deliberately fail-closed:
a row whose `synced_at` is older than 48 hours is not served, so a sync
that stops does not leave a stale price on the wire — it leaves an
absence. That is the right behaviour and also the quiet one. Without
this rule the first person to learn the sync stopped would be whoever
next opened the page and found the panel empty. 30 hours is one missed
daily run plus jitter, which fires with 18 hours to spare before the
bound empties the arm.

## Quick diagnosis

```sh
# 1. Is the timer scheduled, and when did the unit last actually run?
ssh r1 'systemctl list-timers curated-rwa-sync.timer'
ssh r1 'systemctl show curated-rwa-sync.service -p Result,InactiveEnterTimestamp,ExecMainStartTimestamp'
# Type=oneshot: read Result WITH its timestamp. An empty
# InactiveEnterTimestamp means there is no run for that Result to describe.

# 2. What did the last run say?
ssh r1 'journalctl -u curated-rwa-sync -n 40 --no-pager'

# 3. Is the textfile there, and does it move?
ssh r1 'ls -la /var/lib/node_exporter/textfile_collector/curated_rwa_sync.prom'
ssh r1 'cat /var/lib/node_exporter/textfile_collector/curated_rwa_sync.prom'

# 4. Is the key present? (never print it — test for the assignment only)
ssh r1 'grep -c "^DUNE_API_KEY=." /etc/default/curated-rwa-sync'   # 1 = set, 0 = empty placeholder

# 5. What does the API say the arm's state is?
curl -s https://api.stellarindex.io/v1/rwa/assets | jq '.curated | {status, assets, additional_value_usd, census}'
```

## Triage tree

1. **`grep -c` in step 4 prints `0`** — the env file still carries the
   placeholder the role installs (`DUNE_API_KEY=`). The unit exits
   before executing anything and stamps nothing, so `last_run_unix` is
   absent and the rule's `absent_over_time` arm fires. This is the
   expected state of a fresh install until an operator sets the key.
   Set it (`DUNE_API_KEY=…` in `/etc/default/curated-rwa-sync`, mode
   0600), then `systemctl start curated-rwa-sync.service` and re-read
   the textfile. The reader recognises the arm within its 10-minute
   cache TTL.

2. **Timer not listed, or `NEXT` is `n/a`** — the timer was not enabled
   (a deploy that skipped task 14, or a host bootstrapped before the
   unit existed). `systemctl enable --now curated-rwa-sync.timer`; the
   ansible role is the source of record, so re-run the archival-node
   playbook if this drifts again.

3. **Journal shows `execute: 4xx`** — Dune refused the execution. `401`
   / `403`: the key was rotated or revoked; replace it. `402` / `429`:
   the team's credit allowance is exhausted — every run costs
   `execution_cost_credits` (visible on the gauge) and Dune meters the
   free tier monthly. Either wait for the reset or lower the cadence in
   `curated-rwa-sync.timer.j2`; the 48-hour bound tolerates one missed
   day, not more.

4. **Journal shows `wait: execution did not finish`** — Dune's engine
   queued past `RUN_TIMEOUT` (6 min). Usually transient; if it repeats
   across two days, the query is contending for the `small` tier —
   check Dune's status page before touching the query.

5. **Journal shows `parse:` / `rows:` errors** — the curator changed a
   column name or layout in their uploaded datasets. The query in
   `internal/ops/ingest/curated_rwa_sync.go` (`curatedRWADuneSQL`)
   names the columns explicitly; compare against the dashboard's
   query 6961846 and adjust. A layout change is a code change with a
   test, not an ops fix.

6. **Textfile is written but `last_run_unix` frozen** — the unit ran and
   wrote, but node_exporter is not scraping the directory (permission
   on the file, or the collector flag missing). `ls -la` the file and
   check `node_exporter` args for `--collector.textfile.directory`.

7. **Everything above is healthy and the alert still fires** — the
   scrape target for r1 is down; this alert is a symptom, not the
   cause. Follow the node-exporter-down runbook instead.

## Resolution

The alert clears on its own once a run stamps a fresh `last_run_unix`
(`for: 10m` after the next successful scrape). No manual silence is
needed for the known-empty-key case on a test net — the rule is
evaluated per instance and a test net whose unit never stamps will
fire; silence it in Alertmanager with a matcher on the instance label
if that net is not expected to carry a key.

## Related

- `internal/ops/ingest/curated_rwa_sync.go` — the sync command: the
  Dune query, the execute/poll/page loop, and the textfile writer whose
  `last_run_unix` this alert reads.
- `internal/storage/timescale/rwa_curated_directory.go` — the reader
  whose 48-hour recognition bound is the deadline this alert runs ahead
  of; `internal/api/v1/rwa_curated.go` turns its empty set into
  `curated.status: unavailable`.
- `api-smoke-stale.md` — the same "textfile stamped every run, absent
  branch for never-ran" shape; the diagnosis order there applies here.
- `docs/methodology/rwa-coverage-reconciliation.md` § *The curated arm*
  — why the curator's figure is served apart from the verified set, and
  what a stale curated arm does and does not affect.
- `configs/ansible/roles/archival-node/templates/systemd/curated-rwa-sync.service.j2`
  — the unit, its `EnvironmentFile`, and the `ReadWritePaths` grant for
  the textfile directory.

## Changelog

- 2026-09-17 — created with the alert (curated arm, v0.88.0).
