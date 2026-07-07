---
title: Runbook — stellarindex_ingest_gap_detector_silent
last_verified: 2026-07-06
status: ratified
severity: P2
---

# Runbook — `stellarindex_ingest_gap_detector_silent`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_ingest_gap_detector_silent` |
| Severity | P2 (ticket) |
| Detected by | `(time() - stellarindex_ingest_gap_detector_last_success_unix) > 8h` (per source/table) OR the detector metric absent for 15 min (aggregator down) |
| Typical MTTR | 15 min (restart) — 1 h (deeper Postgres issue) |
| Impact | The data-gap detector goroutine is wedged for a target. `stellarindex_ingest_gap_max_size_ledgers` gauges read stale value; the paging `ingest_gap_detected` alert can't fire even if a real gap forms. The system has lost its data-derived ingest-health signal for that target. |

## Symptoms

- `stellarindex_ingest_gap_detector_last_success_unix{source,table}` for one target is more than 8h old (or the whole detector metric is absent → aggregator down).
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

The detector starts immediately on aggregator boot (light targets ~3 s; the heavy `sdex`/`soroban-events` targets take up to ~11 min for the first post-restart trailing-window catch-up scan). Each successful scan re-stamps `stellarindex_ingest_gap_detector_last_success_unix`, so a healthy restart clears the alert within ~15 min.

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

- **Fresh deploy / operator-triggered restart.** The startup scan re-stamps the last-success gauge (light targets ~3 s, heavy targets up to ~11 min), so staleness resets well under the 8h threshold — a restart clears the alert, it does not cause it. This is the class the 2026-07-06 fix eliminated: the previous `rate(runs_total{outcome="ok"}[7h]) == 0` expr false-fired because the heavy targets' `ok` counter is pinned at `1` per process life and `1 → 1` across a restart is invisible to `rate()`.
- **Heavy target between 6h scans.** `sdex`/`soroban-events` scan every 6h, so their staleness sawtooths up to ~6h + scan duration (~11 min). That peak (~6.2h) is below the 8h threshold, so it does not fire. Only a genuinely missed cycle (no success in 8h) trips the alert.

## Related

- `ingest-gap-detected.md` — the paging alert this meta-alert protects from going silent.
- `aggregator-silent.md` — sibling meta-alert for the aggregator binary itself.
- `docs/operations/backfill-with-live-ingest.md` — F-0020 posture for managing Postgres pool pressure.

## Changelog

- 2026-07-06 — re-keyed the alert off `stellarindex_ingest_gap_detector_last_success_unix` staleness (`> 8h`) instead of `rate(runs_total{outcome="ok"}[7h]) == 0`. The rate expr false-fired for >7h on the 6h-cadence heavy targets because their `ok` counter is pinned at `1` per process life and `1 → 1` across a restart defeats Prometheus counter-reset detection. A wall-clock gauge is reset-proof.
- 2026-05-28 — initial draft alongside the gap detector worker ship.
