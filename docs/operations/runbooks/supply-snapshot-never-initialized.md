---
title: Runbook — supply-snapshot-never-initialized
last_verified: 2026-08-29
status: current
severity: P3
---

# Runbook — `stellarindex_supply_snapshot_never_initialized`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_supply_snapshot_never_initialized` (P3, `severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/supply-snapshot.yml` (group `stellarindex.supply_snapshot`, `absent_over_time(...[36h]) == 1`, `for: 5m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/supply-snapshot.yml`. |
| Shares this runbook | `stellarindex_aggregator_supply_refresh_never_initialized` (`configs/prometheus/rules.r1/supply-refresh.yml` + multi-host twin; `severity: ticket`, `absent_over_time(stellarindex_aggregator_supply_refresh_total{outcome="ok"}[36h]) == 1`, `for: 5m`) — its `runbook_url` deliberately routes HERE; per-alert detail in [aggregator-supply-refresh-never-initialized.md](aggregator-supply-refresh-never-initialized.md). |
| Typical MTTR | 10 min (one-shot operator action) |
| Impact | `/v1/assets/{id}` F2 fields (circulating / total / max / market_cap_usd / fdv_usd) render as `null` for every asset. |

## Why this exists separately from `_stale`

`_stale` fires when `stellarindex_supply_snapshot_last_success_timestamp`
is *older than 36 h*. That requires the metric to have existed at
some point — Prometheus's `time() - <missing>` is `no data`, not
infinity, so a deployment that has *never* written a snapshot is
invisible to `_stale`.

This alert closes that blind spot via
`absent_over_time(...[36h]) == 1`. The 36 h window matches `_stale`'s
cushion so a fresh install that hasn't had one daily timer fire
doesn't false-positive.

## Symptoms

- The alert annotation summary: "supply snapshot has never published — pipeline uninitialized."
- `/v1/assets/native` returns no `circulating_supply` /
  `market_cap_usd` fields (they're omitted when null per the
  wire-shape contract).
- `/v1/assets/xlm` (catalogue-slug variant) likewise.
- `psql … -c "SELECT count(*) FROM asset_supply_history"` returns 0.

## Quick diagnosis (≤ 5 min)

```sh
# 1. Is the timer installed at all?
ssh root@<host>
systemctl is-enabled supply-snapshot.timer
# "not-found" / "Unit could not be found" → never installed
# "enabled"   → installed; check status next

# 2. If installed, has it ever fired?
systemctl status supply-snapshot.timer  --no-pager
systemctl status supply-snapshot.service --no-pager  # most recent run

# 3. Is the goroutine path enabled instead?
grep -A 1 'aggregator_refresh_enabled' /etc/stellarindex.toml
# absent or =false → not enabled
# =true            → check aggregator logs for "supply-refresh" lines
journalctl -u stellarindex-aggregator --since '2 days ago' | grep -E 'supply-refresh|supply.*ok|supply.*err'
```

## Resolution

Pick **one** path. Both populate `asset_supply_history`; never run
both simultaneously.

### Path A — systemd timer (daily, simpler)

```sh
sudo cp deploy/systemd/supply-snapshot.{service,timer} /etc/systemd/system/
sudo systemctl daemon-reload

# CRITICAL — wire the textfile output BEFORE starting. The shipped
# unit defaults `Environment=TEXTFILE_OUTPUT=` (EMPTY — no metrics
# emit at all). Started as-is, the snapshot writes to Postgres but
# never publishes stellarindex_supply_snapshot_last_success_timestamp,
# so this alert NEVER clears and the verify grep below fails on a
# perfectly working writer. Create the override first:
cat <<'EOF' | sudo tee /etc/default/supply-snapshot
TEXTFILE_OUTPUT=/var/lib/node_exporter/textfile_collector/supply_snapshot.prom
EOF

sudo systemctl enable --now supply-snapshot.timer

# Verify the unit fires immediately (one-off):
sudo systemctl start supply-snapshot.service
sudo journalctl -u supply-snapshot.service --no-pager -n 50
# Expect: `Wrote snapshot for asset_key=XLM ledger=<N> basis=<...>`
# + zero exit.

# Verify the metric appears (once node_exporter rescrapes the .prom):
curl -s http://localhost:9100/metrics | grep stellarindex_supply_snapshot_last_success_timestamp
# Expect a single line with a recent unix timestamp.
```

The alert clears within 5 min of a successful first run.

### Path B — aggregator-resident goroutine (sub-minute cadence)

```sh
# Edit /etc/stellarindex.toml:
[supply]
aggregator_refresh_enabled = true
# Optional: aggregator_refresh_cadence = "5m" (default)

sudo systemctl restart stellarindex-aggregator
sudo journalctl -u stellarindex-aggregator -f | grep -E 'supply-refresh'
# Expect a "supply-refresh" tick line within `aggregator_refresh_cadence`.

# Verify the metric appears in the aggregator's /metrics:
curl -s http://localhost:9465/metrics | grep stellarindex_aggregator_supply_refresh_total
# Expect at least one outcome="ok" line per watched asset_key.
```

Path B does NOT silence THIS alert. Nothing but the ops-CLI
textfile writer (Path A) emits
`stellarindex_supply_snapshot_last_success_timestamp`, so the
textfile-path `_never_initialized` alert KEEPS firing under Path B
alone. What Path B clears is its sibling,
`stellarindex_aggregator_supply_refresh_never_initialized`
(which deliberately routes its `runbook_url` to this page — see
At a glance), and it populates the goroutine-path metrics tracked
by [supply-refresh-stalled.md](supply-refresh-stalled.md). If the
deployment intentionally runs Path B exclusively, silence the
textfile-path `_never_initialized` alert. The textfile-path
`_stale` alert cannot fire on Path B (its series is absent —
`_stale` requires the metric to have existed) — no silence needed
for it.

## Why neither path is the default

The supply pipeline ships dormant by design — the operator-managed
`reserve_balances_stroops` config is the source of truth for SDF
reserves (the publicly-known accounts whose balances we subtract
from the total to compute circulating). Without those balances,
the writer would emit nonsense; the gate forces operator review
before the first publish.

See [docs/operations/supply-snapshot.md](../supply-snapshot.md)
for the operator-side wiring guide.

## Verifying the fix

After enabling either path, the following should be true within
`max(36 h, aggregator_refresh_cadence)`:

```sh
# Alert clears in Prometheus.

# Database has rows.
sudo -u postgres psql -d stellarindex -c \
  "SELECT asset_key, count(*) AS rows, max(time) AS latest
   FROM asset_supply_history GROUP BY asset_key ORDER BY asset_key"

# API surfaces the data.
curl -s 'https://api.stellarindex.io/v1/assets/native' | jq '.data.circulating_supply'
# Expect a numeric string, not null.
```

## Related

- [`supply-snapshot-stale.md`](supply-snapshot-stale.md) —
  sibling alert: fires when a previously-working pipeline goes
  stale. Both alerts cover the same data-quality surface; this
  runbook is the never-initialised case (cold deploy), the
  sibling is the steady-state-degraded case.
- [`supply-refresh-stalled.md`](supply-refresh-stalled.md) —
  sibling alert for Path B (goroutine refresher) rather than
  Path A (one-shot timer).
- `internal/supply/refresher.go` — implementation; the
  `OutcomeKindMissingFreshness` outcome from F-1236 wave 60
  surfaces here when the operator-opt-in
  `[supply].strict_freshness_required` is enabled.
- [docs/architecture/supply-pipeline.md](../../architecture/supply-pipeline.md) — the three-algorithm design.
- [docs/adr/0011-supply-algorithm.md](../../adr/0011-supply-algorithm.md) — original ADR.

## Changelog

- 2026-08-29 — re-verified against HEAD (Wave I). Path A now
  creates `/etc/default/supply-snapshot` with `TEXTFILE_OUTPUT=`
  set BEFORE starting — the shipped unit defaults it EMPTY, so the
  as-written sequence never emitted the metric, the alert never
  cleared, and the verify grep failed on a working writer. Log
  line corrected to the real
  `Wrote snapshot for asset_key=XLM ledger=<N> basis=<...>`.
  Path B section rewritten: it does NOT silence this alert (only
  the timer path emits `last_success_timestamp`); it clears the
  sibling `stellarindex_aggregator_supply_refresh_never_initialized`,
  whose `runbook_url` routes here — now cross-linked both ways.
  Detected-by made dual-tree. Status promoted ratified → current.
