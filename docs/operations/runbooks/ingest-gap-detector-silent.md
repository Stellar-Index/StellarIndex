---
title: Runbook — stellarindex_ingest_gap_detector_silent
last_verified: 2026-08-28
status: ratified
severity: P2
---

# Runbook — `stellarindex_ingest_gap_detector_silent`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingest_gap_detector_silent` |
| Severity | P2 (ticket) |
| Detected by | `(time() - stellarindex_ingest_gap_detector_last_success_unix) > 8h` (per source/table) OR the detector metric absent for 15 min (aggregator down) OR `runs_total{outcome="error"}` present now and 8h ago with no last-success stamp seen in 8h (a target that has never succeeded in this process life) |
| Typical MTTR | 15 min (restart) — 1 h (deeper Postgres issue) |
| Impact | The data-gap detector goroutine is wedged for a target. `stellarindex_ingest_gap_max_size_ledgers` gauges read stale value; the paging `ingest_gap_detected` alert can't fire even if a real gap forms. The system has lost its data-derived ingest-health signal for that target. |

## Symptoms

- `stellarindex_ingest_gap_detector_last_success_unix{source,table}` for one target is more than 8h old (or the whole detector metric is absent → aggregator down).
- **Or the stamp does not exist at all** for the target and the alert carries `outcome="error"`: the target has never scanned successfully in this process life (no `gap-detector-scan` cursor row, so nothing re-emits a stamp on boot) and has been erroring for 8h+. Before 2026-08-28 this case fired nothing — the error counter satisfied the "runs_total present" clause and there was no stamp to age. The 2026-08-28 r1 `soroban-events/soroban_events` statement_timeout loop (PR #258) is the canonical example.
- `stellarindex_ingest_gap_detector_runs_total{outcome="error"}` is climbing for that target (the scan is failing every cycle), and the aggregator log shows `gap-detector: scan failed` lines with a large `elapsed_s` (timeout) or a Postgres error string.
- Operators reading the dashboard see the gap-size gauge frozen on its last-known value.
- May coincide with `stellarindex_aggregator_silent` (aggregator binary is down) or `stellarindex_postgres_exporter_down` (Postgres is unreachable).

> **Why a timestamp gauge, not `rate(runs_total{outcome="ok"})`?**
> The heavy targets (`sdex`/`trades`, `soroban-events`/`soroban_events`)
> scan on a 6h `ScanCadence`, so their `ok` counter increments only once
> per 6h. When the aggregator restarts more often than that, each process
> life records exactly one `ok`, pinning the counter at `1`. Because the
> value is `1` both before and after the restart, Prometheus counter-reset
> detection never fires and `rate(...ok[7h])` reads a flat line → `0` → the
> alert false-fired for >7h on 2026-07-06 even though every scan succeeded.
> The wall-clock gauge is reset-proof: a healthy startup scan re-stamps it
> to `now()`, so the alert clears the moment a scan succeeds.

## Triage — 5 minutes

1. **Aggregator service healthy?**

   ```sh
   ssh root@<region-host> 'systemctl status stellarindex-aggregator | head'
   ```

   If inactive or crash-looping, that's the root cause — fix the aggregator first (`journalctl -u stellarindex-aggregator -n 200`).

2. **Postgres reachable?**

   ```sh
   ssh root@<region-host> 'sudo -iu postgres pg_isready'
   ```

   If not, the detector's per-target scan timeout (15 min Go-side / 13 min SQL `statement_timeout`) is firing every cycle and incrementing `outcome=error` instead — the last-success gauge stops advancing and staleness grows past 8h. Cross-check `stellarindex_postgres_exporter_down`.

3. **Connection pool saturated?**

   ```sh
   ssh root@<region-host> "sudo -iu postgres psql -d stellarindex -c 'SELECT count(*), state FROM pg_stat_activity GROUP BY state;'"
   ```

   `active` count near `max_connections` means the detector can't get a connection. Likely caused by concurrent fill walks per F-0020; see `docs/operations/backfill-with-live-ingest.md` for the recommended posture.

## Remediation

### Aggregator down

```sh
ssh root@<region-host> 'systemctl restart stellarindex-aggregator'
ssh root@<region-host> 'journalctl -u stellarindex-aggregator -f'
```

The detector's first cycle runs immediately on aggregator boot, but since 2026-08-28 it honours each target's persisted schedule: the light targets scan within seconds; the heavy `sdex`/`soroban-events` targets (6h cadence) are scanned only if 6h have elapsed since their `gap-detector-scan` cursor's `last_updated`, otherwise they are skipped until their cadence is due and their `last_success_unix` + gap gauges are re-emitted from the persisted cursor / `source_coverage_snapshots` row. (Before that date every restart re-ran both heavy scans immediately — each a >10-min full scan of `soroban_events` — which is how a deploy loop turned into the 2026-08-28 IO incident.) A healthy restart therefore clears the alert within ~15 min for a light target; for a heavy target the re-emitted stamp is the previous process's last success, so the alert clears as soon as the next due scan succeeds (≤ 6h + ~13 min after that stamp — still under the 8h threshold). If you need a heavy target scanned NOW, delete its cursor row (`DELETE FROM ingestion_cursors WHERE source = 'gap-detector-scan' AND sub_source = 'soroban-events/soroban_events'`) before restarting — this also widens its next scan to the `GapDetectorFirstScanCap` window.

### Postgres degraded

Defer the detector restart until Postgres is healthy. Once `pg_isready` returns clean, the detector recovers on its own cycle (no aggregator restart needed unless the goroutine has fully exited — check the aggregator log for `gap-detector` warnings).

### Pool saturation

Reduce concurrent walk parallelism per `docs/operations/backfill-with-live-ingest.md`:

```sh
# Stop the running fill (a manual operator invocation, not a systemd unit) —
# find its PID and kill -INT it per backfill-with-live-ingest.md
# "Stop a running fill walk". Then wait for connection-count to drop and
# resume at -parallel 4 instead of -parallel 12.
```

## Known false-positive patterns

- **Fresh deploy / operator-triggered restart.** The startup cycle re-stamps the last-success gauge for the light targets (~3 s) and re-emits the PERSISTED stamp for heavy targets whose 6h cadence has not elapsed (they are not re-scanned on restart since 2026-08-28), so staleness never reads higher after a restart than before it — a restart clears or preserves the alert state, it does not cause it. This is the class the 2026-07-06 fix eliminated: the previous `rate(runs_total{outcome="ok"}[7h]) == 0` expr false-fired because the heavy targets' `ok` counter is pinned at `1` per process life and `1 → 1` across a restart is invisible to `rate()`.
- **Heavy target between 6h scans.** `sdex`/`soroban-events` scan every 6h, so their staleness sawtooths up to ~6h + scan duration (~11 min). That peak (~6.2h) is below the 8h threshold, so it does not fire. Only a genuinely missed cycle (no success in 8h) trips the alert.

## Related

- `ingest-gap-detected.md` — the paging alert this meta-alert protects from going silent.
- `aggregator-silent.md` — sibling meta-alert for the aggregator binary itself.
- `docs/operations/backfill-with-live-ingest.md` — F-0020 posture for managing Postgres pool pressure.

## Changelog

- 2026-08-28 — third clause: fire when `runs_total{outcome="error"}` is present now and 8h ago and no `last_success_unix` stamp has been seen for the target in 8h. Closes the blind spot where a target that had never once succeeded (no stamp to age; error counter satisfying `absent_over_time(runs_total)`) fired nothing. promtool unit tests in `deploy/monitoring/rule-tests/ingestion_test.yml`.
- 2026-08-28 — restart no longer re-scans heavy targets ahead of their cadence (schedule seeded from the persisted `gap-detector-scan` cursor; last-success stamp + gap gauges re-emitted from persisted state). Density count for `soroban-events` now reads the `ledger_ingest_log` census instead of a full scan of `soroban_events`; PG `statement_timeout` for both detector queries is 13 min (the count had been 2h against a 15-min Go context, orphaning backends). r1 incident 2026-08-28 18:23Z.
- 2026-07-06 — re-keyed the alert off `stellarindex_ingest_gap_detector_last_success_unix` staleness (`> 8h`) instead of `rate(runs_total{outcome="ok"}[7h]) == 0`. The rate expr false-fired for >7h on the 6h-cadence heavy targets because their `ok` counter is pinned at `1` per process life and `1 → 1` across a restart defeats Prometheus counter-reset detection. A wall-clock gauge is reset-proof.
- 2026-05-28 — initial draft alongside the gap detector worker ship.
