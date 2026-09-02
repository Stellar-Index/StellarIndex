---
title: Runbook — stellarindex_worker_panicked
last_verified: 2026-09-02
status: active
severity: P1
---

# Runbook — `stellarindex_worker_panicked`

## At a glance

| | |
|---|---|
| **Alert** | `stellarindex_worker_panicked` — `increase(stellarindex_worker_panics_total[10m]) > 0` |
| **Severity** | page |
| **What it means** | A goroutine wrapped in `worker.Recover` panicked. The PROCESS is up; that WORKER is stopped and will not run again until its unit restarts. |
| **First action** | Read the `worker` label, find the panic in the owning unit's journal, restart that unit. |
| **Why page** | Nothing downstream fires until the symptom appears — stale prices, an unbounded table, a cold cache — and which one depends on which worker died. |

## Why this exists

`worker.Recover` (internal/worker/recover.go) swallows a panic so one bad
goroutine cannot take the binary down. Before #368 M4 the only trace was
one `logger.Error` line: ~45 background workers across the three
long-lived binaries could die one by one with nothing in either rule tree
noticing until a downstream freshness alert fired hours later. The
counter is per worker so the alert names the dead goroutine; it counts
panics, not stopped workers — one panic = one dead worker until restart.

## Symptoms

- `stellarindex_worker_panicked{worker="<name>"}` firing.
- Journal line `background worker panicked — worker STOPPED, process still running`
  with `worker=<name>`, `panic=<value>` and a stack, in one of
  `stellarindex-api`, `stellarindex-indexer`, `stellarindex-aggregator`.
- Later, if unhandled: whatever that worker fed goes stale or unbounded
  (a prewarm worker → cold-path latency; a snapshot/refresh worker → a
  freshness alert; a reaper → a bounded auth table growing, #368 M5).

## Quick diagnosis (≤ 5 min)

```sh
ssh root@136.243.90.96 'journalctl -u stellarindex-api -u stellarindex-indexer -u stellarindex-aggregator --since "-30m" --no-pager | grep -A3 "background worker panicked"'
```

The `worker` value tells you the binary and the blast radius. Confirm
the count and which unit:

```sh
ssh root@136.243.90.96 'curl -s "http://localhost:9090/api/v1/query?query=stellarindex_worker_panics_total" | python3 -m json.tool | grep -E "worker|value"'
```

## Mitigation (≤ 15 min)

1. **Restart the owning unit** — `Recover` does not restart the worker:
   `systemctl restart stellarindex-<binary>`. An indexer restart resets the
   ledgerstream cursor: confirm it advances afterwards
   ([frozen-indexer-cursor](frozen-indexer-cursor.md)).
2. Confirm the alert resolves (the counter is monotonic; `increase(...[10m])`
   drops to 0 once ten minutes pass without a new panic).
3. If the same worker panics again on the same input, it is a poison input
   (see #371 F1) — stop the unit rather than crash-loop it, and escalate.

## Root cause analysis

A worker panic is always a code defect — an unvalidated index, a nil
dereference, a decoder fed attacker-shaped bytes. File it with the stack
from the journal. The alert exists so panics get FIXED, not tolerated: a
worker that "only panicked once" is a worker that is off until the next
deploy.

## Known false-positive patterns

None known. A recovered panic is never benign: even when the worker is
low-value, the process is now running without it.

## Related

- #368 (CON worker-fleet audit) M4 — the finding this closes.
- `internal/worker/recover.go` — the only place the counter increments.
- [dependency-down](dependency-down.md), [frozen-indexer-cursor](frozen-indexer-cursor.md).

## Changelog

- 2026-09-02 — created with the metric + alert (#368 M4).
