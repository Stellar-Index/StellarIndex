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
| Alerts | `stellarindex_curated_rwa_sync_stale` (P3, `severity: ticket`) — the timer stopped stamping; `stellarindex_curated_rwa_sync_refused` (P3, `severity: ticket`) — it ran and refused because no API key is set. Both routed by `configs/alertmanager/alertmanager.r1.yml` |
| Severity | P3 — a comparison panel degrades; the verified RWA surface beside it is untouched |
| Scope | r1 / pubnet — the curated arm is only meaningful where the curator's list exists (Dune's Stellar datasets are pubnet). The unit is installed on every network but a test net with no key stays `unwired` by design and this alert does not fire there, because the metric is stamped on a dry run too. |
| Detected by | `deploy/monitoring/rules/curated-rwa-sync.yml` + `configs/prometheus/rules.r1/curated-rwa-sync.yml` (byte-identical; group `stellarindex.curated_rwa_sync`) |
| Metric source | `node_exporter` textfile_collector reads `/var/lib/node_exporter/textfile_collector/curated_rwa_sync.prom`, written by `stellarindex-ops ingest curated-rwa-sync` at the end of every run — dry or wet, success or refusal — from `curated-rwa-sync.timer` (daily, 04:12 UTC) |
| Steady-state | `last_run_unix` advances once a day; `rows` ≈ 220–230 (the curator's monthly total series, ~13 points, plus its per-subclass split, ~210 rows); `datapoints_read` a small constant per run (what Dune metered for the two result reads — fractions of a credit; a run never executes a query); `executed_at_unix` advances about daily on the curator's own schedule |
| Customer impact | None on verified figures. `/v1/rwa/assets` reports `curated.status: unavailable` once the 48-hour recognition bound passes and the explorer's curated panel says the comparison is unavailable rather than showing a stale published total |
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
curl -s https://api.stellarindex.io/v1/rwa/assets | jq '.curated | {status, assets, census, published: (.published | {total_usd, as_of, executed_at, gap_vs_verified_usd})}'
```

## Triage tree

1. **`grep -c` in step 4 prints `0`, and the firing alert is `_refused`**
   — the env file still carries the placeholder the role installs
   (`DUNE_API_KEY=`). The run exits clean without reading the curator,
   stamps `last_run_unix` (so `_stale` measures the timer, not the key)
   and sets `stellarindex_curated_rwa_sync_refused` to 1. This is the
   expected state of a fresh install until an operator sets the key.
   The file is `/etc/default/curated-rwa-sync`, `root:root` mode `0600`
   — only systemd reads it, as PID 1, so it carries no group read
   unlike the sibling `/etc/default/*` files. Set the key the way the
   role does, so the next apply agrees with the host:

   ```sh
   # on the workstation, from configs/ansible/
   ansible-vault edit inventory/r1.secrets.yml     # set vault_dune_api_key
   ansible-playbook -i inventory/r1.yml playbooks/archival-node.yml \
     --tags stellarindex --check --diff            # always --check --diff first
   ```

   The role renders `DUNE_API_KEY={{ vault_dune_api_key }}` into the
   file whenever the vault defines a non-empty value; with the variable
   undefined it installs the empty placeholder once and never overwrites
   what an operator pasted in. Pasting the key by hand still works
   (that is how r1 was keyed on 2026-09-17), but the vault is the source
   of record — a hand-set key is replaced by the vault's on the first
   apply after the variable is defined. Then `systemctl start
   curated-rwa-sync.service` and re-read the textfile — the gauge clears
   on that run. The reader recognises the arm within its 10-minute cache
   TTL.

   If `_stale` fires on a host that never stamped, the timer has not
   fired at all in 30 hours of scrapes: the `absent_over_time` arm is
   gated on Prometheus having actually watched that long, so a fresh
   rule load or a Prometheus restart does not fire it. Go to step 2.

2. **Timer not listed, or `NEXT` is `n/a`** — the timer was not enabled
   (a deploy that skipped task 14, or a host bootstrapped before the
   unit existed). `systemctl enable --now curated-rwa-sync.timer`; the
   ansible role is the source of record, so re-run the archival-node
   playbook if this drifts again.

3. **Journal shows `GET /api/v1/query/…/results: HTTP 4xx`** — Dune
   refused the read. `401` / `403`: the key was rotated or revoked;
   replace it. `402` / `429`: the team's credit allowance is exhausted —
   a run reads two public query results and is metered by datapoint
   (`datapoints_read` on the gauge, a fraction of a credit), never by
   execution, so this is almost always another consumer of the same key.
   Either wait for the monthly reset or lower the cadence in
   `curated-rwa-sync.timer.j2`; the 48-hour bound tolerates one missed
   day, not more. `404`: the curator deleted or privatised the query —
   open dune.com/stellar/rwas and find its successor; the ids are
   constants in `internal/ops/ingest/curated_rwa_sync.go`.

4. **Journal shows `latest execution is QUERY_STATE_…, not completed`**
   — the curator's own scheduled run of the query failed, so its latest
   result is not one this index will read. Nothing to do on this side:
   the previous day's rows stay served until the 48-hour bound, and the
   curator's next successful run clears it. If it persists past a day,
   the dashboard itself is broken — say so on the page's issue, not in
   code.

5. **Journal shows `printed no usable row` / `printed N of the M rows it
   declared`** — the curator changed a column name or layout in a
   public query, or its paging broke. The row shapes in
   `internal/ops/ingest/curated_rwa_sync.go` are decoded STRICTLY
   (`duneMonthlyTotalRow`, `duneMonthlyBySubclassRow`); compare against
   the query's current columns on dune.com and adjust. A layout change
   is a code change with a test, not an ops fix.

   **`executed_at_unix` frozen while `last_run_unix` advances** — the
   run is healthy and the CURATOR has stopped refreshing its queries.
   The API's `curated.published.executed_at` shows the same stamp; the
   figure is still the curator's latest, only older. Nothing on this
   side fixes it.

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
  two public query ids, the results-paging loop, the strict row shapes,
  and the textfile writer whose `last_run_unix` this alert reads.
- `internal/storage/timescale/rwa_curated_published.go` — the reader
  whose 48-hour recognition bound is the deadline this alert runs ahead
  of; `internal/api/v1/rwa_curated.go` turns its empty set into
  `curated.status: unavailable` and a full one into `published_totals`.
  (`rwa_curated_directory.go` is the per-asset reader the arm was built
  on; the first curator's per-asset tables are private, so it stays
  empty.)
- `api-smoke-stale.md` — the same "textfile stamped every run, absent
  branch for never-ran" shape; the diagnosis order there applies here.
- `docs/methodology/rwa-coverage-reconciliation.md` § *The curated arm*
  — why the curator's figure is served apart from the verified set, and
  what a stale curated arm does and does not affect.
- `configs/ansible/roles/archival-node/templates/systemd/curated-rwa-sync.service.j2`
  — the unit, its `EnvironmentFile`, and the `ReadWritePaths` grant for
  the textfile directory.
- `configs/ansible/roles/archival-node/tasks/14-stellarindex-services.yml`
  — the two tasks (vault render / first-install placeholder) that own
  `/etc/default/curated-rwa-sync`, and why it is `root:root 0600`.

## Changelog

- 2026-09-17 — created with the alert (curated arm, v0.88.0).
- 2026-09-17 — `_refused` added; a keyless run stamps instead of failing, and the `_stale` absent arm is gated on 30 h of observed scrapes (it fired 10 min after the v0.88.1 deploy).
- 2026-09-17 — key mechanism made one thing: the role renders `/etc/default/curated-rwa-sync` from `vault_dune_api_key` (`root:root 0600`, matching what r1 carries and what this runbook and the alert prescribe); the tier named here is `medium` (#518).
- 2026-09-17 — the sync reads the curator's PUBLISHED totals (public query results) instead of its private per-asset tables, which no outside key can read; `execution_cost_credits` replaced by `datapoints_read`, `executed_at_unix` added, `priced` dropped; triage steps 3–5 rewritten for a read that never executes.
