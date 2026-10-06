---
title: Runbook — ledger ingest alerts (meta decode failing, cursor stuck, ledger stalled)
last_verified: 2026-10-06
status: active
severity: P1 | P2 | P3
---

# Runbook — ledger ingest alerts

Alerts on the Galexie -> ledgerstream -> dispatcher -> cursor path. `stellarindex_ingestion_cursor_stuck` and `stellarindex_ingestion_ledger_stalled` are detected by `configs/prometheus/rules.r1/ingestion.yml` (group `stellarindex.ingestion`, the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/ingestion.yml`). The two ledger-meta alerts are in `stellar-stack-version.yml` of the same two trees.

## At a glance

| Alert | Severity | Detected by | Meaning |
| ----- | -------- | ----------- | ------- |
| [`stellarindex_ledger_meta_decode_failing`](#stellarindex_ledger_meta_decode_failing) | P1 page | `stellarindex_ledger_meta_decode_failures_total > 0` for 10m | a component cannot decode ledger meta the network now produces |
| [`stellarindex_ledger_meta_decode_probe_stale`](#stellarindex_ledger_meta_decode_failing) | P3 ticket | probe metric age > 1h for 30m | the probe itself stopped reporting |
| [`stellarindex_ingestion_cursor_stuck`](#stellarindex_ingestion_cursor_stuck) | P2 ticket, `for: 5m`, MTTR 10-30 min | per-SOURCE cursor flat | cursor not advancing for a configured source |
| [`stellarindex_ingestion_ledger_stalled`](#stellarindex_ingestion_ledger_stalled) | P1 page, `for: 5m` over a 5-minute flat window (~10 min to page), MTTR 10-30 min | `ledgerstream` cursor flat | no ledger ingested for >= 10 min |

## stellarindex_ledger_meta_decode_failing

Covers `stellarindex_ledger_meta_decode_failing` (page) and `stellarindex_ledger_meta_decode_probe_stale` (ticket).

What it means: a component cannot decode ledger meta the network is now producing, almost always "we are behind a protocol upgrade": the installed galexie / indexer predates an XDR change (e.g. P28's `ParallelTxExecutionStage`) and refuses the ledger.

Data loss: none. The decode is fail-closed. The affected component has STOPPED ingesting and will not resume on its own.

Probe-stale variant: `stellarindex_ledger_meta_decode_probe_stale` means the probe itself hasn't reported in over an hour, so a real decode failure would go unseen. Check the `ledger-meta-decode-probe.timer` / `.service` units.

Fix:

1. Identify the affected unit from the `unit` label.
2. Check `stellarindex_stellar_stack_version_lag` (see the [stellar-stack-version-lag runbook](stellar-node.md#stellar-stack-version-lag)) to confirm which component and target version.
3. Follow the [protocol-upgrade procedure](../protocol-upgrades.md) to bump `galexie_version` / `stellar_core_version` / the release binary's `go-stellar-sdk` and re-apply.
4. Confirm `stellarindex_ledger_meta_decode_failures_total` stops increasing and the component's ingest cursor resumes advancing.

## stellarindex_ingestion_cursor_stuck

Impact: on indexer restart, the source re-scans from the last-persisted cursor. If the cursor froze hours ago, restart triggers a huge replay (slow + expensive). While the cursor is stuck, the source is either idle (nothing to advance) or advancing without persisting (data loss on restart).

Symptoms:

- `increase(stellarindex_cursor_last_ledger{source=...}[5m]) == 0` AND `stellarindex_source_enabled == 1`.
- Dashboard: *Ingestion -> Cursor progress* panel shows a flat line for the offending source.
- `stellarindex_source_events_total` may still rise (events are being persisted); this now points more narrowly at cursor-update failure, not at a separate legacy persister goroutine.

Quick diagnosis (<= 5 min):

```sh
# Which source + how far back is the cursor?
stellarindex-ops list-cursors -config /etc/stellarindex.toml

# How does that compare to the network tip?
stellarindex-ops detect-gaps -config /etc/stellarindex.toml -threshold 100

# If detect-gaps says "ok" but the alert fires: the source isn't
# lagging, it's just not seeing events. Check SourceEventsTotal
# rate in Grafana. If it's zero, this may actually be
# "source-stopped" rolled up incorrectly — check that alert too.
```

Key signals:

- **Cursor flat + events > 0**: cursor upserts are failing or the live pipeline is rejecting ledgers before commit. Inspect indexer logs for `cursor upsert` warnings or dispatcher rejection/panic logs.
- **Cursor flat + events == 0**: no events to advance on. Source may be legitimately quiet. Check `source-stopped` before treating this as a persistence-only issue.
- **Cursor flat + repeated indexer errors**: treat as a live ingest fault, not a harmless replay delay.

Mitigation (<= 15 min):

- [ ] Step 1: if upstream is unhealthy, fix that first. The indexer reads ledger metadata from Galexie's MinIO output (`galexie-live` bucket); confirm Galexie is producing fresh objects (`ssh root@136.243.90.96 'mc ls local/galexie-live | tail'`) and that the indexer can reach MinIO. If MinIO/Galexie itself is the problem, jump to [all-ingestion-down](ingestion.md#stellarindex_ingestion_all_sources_stopped). The cursor will advance once ledgers start flowing again. (Pre-2026-04-23 deployments routed via stellar-rpc; that path was removed from r1 and isn't the upstream today.)
- [ ] Step 2: if events are flowing but cursor is flat, capture recent logs (`journalctl -u stellarindex-indexer -n 500 --no-pager > /tmp/indexer.log` on the indexer host) then restart the unit. The current live path updates the cursor inline after successful ledger processing, so a flat cursor usually means repeated ledger failure or DB upsert trouble.

  ```sh
  ssh root@136.243.90.96 systemctl restart stellarindex-indexer
  ```

- [ ] Step 3: if the cursor has regressed (persisted value < events observed), this should not happen (advance-only guard) and indicates a real bug. Capture the cursor table before restart: `runuser -u postgres -- psql -d stellarindex -c 'TABLE ingestion_cursors'` (on the r1 host) and attach to the postmortem.
- [ ] Verification: `stellarindex_cursor_last_ledger{source=...}` starts climbing again after the indexer resumes successful ledger commits.

Root cause analysis, for the postmortem gather:

- Indexer logs around when the cursor stopped moving. Search for `cursor upsert`, `dispatcher rejected ledger`, and `dispatcher panicked`.
- The cursor table snapshot before + after restart.
- `stellarindex_source_events_total` vs `stellarindex_cursor_last_ledger` over the incident window.
- If the issue happened post-deploy: diff the live `ledgerstream -> dispatcher -> UpsertCursor` path rather than the retired orchestrator code.

Known false-positive patterns:

- **Quiet sources during low-volume windows.** If a source emits no events, its cursor does not advance either. Cross-check `source-stopped` and the raw event rate before treating a flat cursor as a persistence failure.
- **Container just started.** Give the indexer time to process and commit at least one successful ledger before treating the flat line as actionable.

## stellarindex_ingestion_ledger_stalled

Impact: no ledger has been ingested for >= 10 min. Every on-chain surface (trades, protocol events, supply, coverage, the explorer) is frozen at the last committed ledger, and the freeze widens for as long as this is unfixed. Off-chain prices (CEX/FX) keep updating, so the API looks partly alive.

Why this exists: `stellarindex_ingestion_all_sources_stopped` is NOT the cover for a lake outage on this deployment, though the indexer unit file claimed it was until 2026-09-03. The CEX/FX connectors run inside the **same binary** as the Galexie->dispatcher path, so `sum(rate(source_events_total[5m]))` keeps moving while no ledger is being read at all. Two more changes closed the remaining paths: #371 F3 gave the live-tail read a ~5-minute retry budget (the process no longer exits promptly), and `StartLimitBurst=60 / 15min` means a ~5min10s start cycle can never park the unit, so `stellarindex_systemd_unit_failed` (a **ticket**, 15 m) cannot fire for it either. `stellarindex_ingestion_cursor_stuck` cannot fire for this cursor at all: it joins `on (source) stellarindex_source_enabled`, and `ledgerstream` is a cursor namespace, not a configured source, so no such series exists.

This alert watches the one gauge that means "a ledger was fully processed AND its cursor row committed" (`cmd/stellarindex-indexer/main.go`, `processAndPersistCursor`), so it covers the whole path (lake read, dispatch, sink, Postgres commit) rather than any single cause.

The F3 budget covered the WALK only; the datastore open + schema load ran before it, un-retried, so a lake outage present at process start failed in ~1s and the "~5min10s start cycle" above did not hold for the one fault it was written about. The retry now covers the open too, and `stellarindex_ledgerstream_live_start_retries_total` is what makes the resulting stall visible.

Symptoms:

- `stellarindex_cursor_last_ledger{source="ledgerstream"}` flat for >= 10 min, or the series absent entirely (a restart that never commits a ledger never creates it).
- `/v1/coverage` and the explorer's ledger badge stop advancing; price endpoints fed by CEX/FX keep updating, which is what makes this easy to misread as healthy.
- `stellarindex_ingestion_all_sources_stopped` is **quiet**: expected, not reassuring.

Quick diagnosis (<= 5 min):

```sh
# Is the process alive, or restart-looping on a dependency?
ssh root@136.243.90.96 'systemctl status stellarindex-indexer --no-pager | head -20'
ssh root@136.243.90.96 'journalctl -u stellarindex-indexer -n 200 --no-pager | tail -60'

# Is the lake readable, and is Galexie still writing?
ssh root@136.243.90.96 'mc ls local/galexie-live | tail'
ssh root@136.243.90.96 'systemctl is-active minio galexie'

# Where is the committed cursor vs the bucket?
ssh root@136.243.90.96 "runuser -u postgres -- psql -d stellarindex -c \
  \"SELECT source, sub_source, last_ledger, last_updated FROM ingestion_cursors WHERE source = 'ledgerstream'\""
```

Key signals:

- **MinIO/Galexie down or unreachable**: lake outage. The indexer is burning its retry budget; expect `SignatureDoesNotMatch` (credential drift) or connection errors in the journal.
- **`stellarindex_ledgerstream_live_start_retries_total` climbing**: same conclusion, without an ssh. That counter moves only when the live tail cannot OPEN the lake (datastore or schema unreadable), so a climbing value confirms lake reachability as the cause and a flat one rules it out. Nothing is being skipped while it climbs: the retry re-issues the identical range and the cursor is written from the callback, which has not run.
- **Lake healthy, cursor flat, no restarts**: the dispatch goroutine is wedged or the sink is blocked. Check `stellarindex_decoder_panics_total` (a recovered decoder panic that left a lock held wedges the walker) and Postgres write health (`stellarindex_postgres_ping_failure_streak`, `stellarindex_trade_insert_buffer_depth`).
- **Unit restart-looping every ~5 min**: a permanently broken dependency or config. The unit will NOT park in `failed` (see "Why this exists" above), so systemd state is not the signal here.
- **Series absent + unit inactive/failed**: the process is not running; `systemctl status` gives the reason.

Mitigation (<= 15 min):

- [ ] Step 1: fix the upstream first if it is down: MinIO, then Galexie. Nothing the indexer does helps while the lake is unreadable; it resumes from its committed cursor on its own. Credential drift is the usual r1 cause; see `docs/operations/runbooks/ingestion.md#stellarindex_ingestion_all_sources_stopped`.
- [ ] Step 2: if the lake is healthy and the cursor is still flat, capture the journal (`journalctl -u stellarindex-indexer -n 1000 --no-pager > /tmp/indexer-stall.log`) **before** restarting; a wedge leaves no trace after the restart. `ssh root@136.243.90.96 systemctl restart stellarindex-indexer`.
- [ ] Step 3: if Postgres is the blocker (commit failures, locks), work that incident first; the cursor is committed in the same path.
- [ ] Verification: `stellarindex_cursor_last_ledger{source="ledgerstream"}` climbs again within ~1 min of the fix, and this alert clears about 10 min after that (5m window + `for: 5m`).

Root cause analysis, for the postmortem gather: the indexer journal across the stall window; `ingestion_cursors` before and after; the MinIO/Galexie unit states and their own logs; `stellarindex_ledgerstream_tier_read_total` by outcome; and the gap the stall left (`stellarindex-ops detect-gaps`, then the ADR-0033 verdict once ingest has caught up).

Known false-positive patterns:

- **A deploy or host reboot longer than ~10 min.** Legitimate, and the page is arguably correct: ingest really is stopped. Silence it for the window rather than widening the rule.
- **A network with no live tail** (an indexer parked on a bounded backfill range that has finished). The gauge stops advancing because there is nothing left to ingest. This does not apply to r1, which always live-tails.
- **The absent branch is fleet-wide.** On a multi-host deployment `absent_over_time` only fires when EVERY indexer's series is gone; a single dead host is caught by the delta branch while its series survives, and by `stellarindex_systemd_unit_failed` after that.

## Related

- [stellar-stack-version-lag runbook](stellar-node.md#stellar-stack-version-lag): the proactive signal the decode alert backstops. [Protocol upgrades](../protocol-upgrades.md): the upgrade procedure. [Alerts catalog](../alerts-catalog.md).
- [cursor-stuck](#stellarindex_ingestion_cursor_stuck): the per-SOURCE cursor ticket. It cannot fire for `source="ledgerstream"` (no `source_enabled` series to join against), which is the gap [ledger_stalled](#stellarindex_ingestion_ledger_stalled) fills.
- `ingestion.md#stellarindex_ingestion_source_stopped`: adjacent alert when events stop flowing entirely.
- [ingestion.md#stellarindex_ingestion_all_sources_stopped](ingestion.md#stellarindex_ingestion_all_sources_stopped): where to route when Galexie / MinIO (the actual upstream) is the problem, and the sibling page for "no events from ANY source"; that one covers the CEX/FX side going quiet too, this one covers the ledger path specifically.
- `rpc-lag` (see [stellar-node.md](stellar-node.md#stellarindex_stellar_rpc_lag)): only relevant if your deployment routes through stellar-rpc (r1 doesn't).
- [infra.md#stellarindex_systemd_unit_failed](infra.md#stellarindex_systemd_unit_failed): ticket, 15 m; will not fire while the unit is restart-looping inside its StartLimit budget.
- Implementation: `cmd/stellarindex-indexer/main.go` (`processAndPersistCursor`, `recordCursorMetric`), `internal/ledgerstream/`.
