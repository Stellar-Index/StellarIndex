---
title: Chaos suite — design note (Task #75)
last_verified: 2026-07-24
status: design ratified (Wave 1 shipped)
related:
  - test/chaos/README.md
  - docs/operations/sev-playbook.md §"Quarterly live chaos"
  - docs/architecture/ha-plan.md §7.3 (companion: load suite)
---

# Chaos suite — design note

Forced-failure smoke for the Stellar Index stack, run as a deliberate
"break one component, assert sane behaviour" exercise. Companion to
the [k6 load suite](ha-plan.md#73-load-testing-k6) — load proves
"healthy stack stays within SLA," chaos proves "broken stack fails
in documented ways."

## Goal

One of the strongest guarantees the API makes is this: **when a
backing service fails, the API never silently serves bad data** — the
response either degrades-with-flag (documented) or 5xxs loud
(unmistakable). A 200-with-empty-`data` or 200-with-stale-stamps is
the nightmare. This suite is the behavioural fence for that specific
guarantee — backing-service-failure degradation — not a proof that no
other silent-failure path exists anywhere in the system; see
`docs/operations/production-confidence-campaign-2026-07-23.md` for the
tracked ledger of currently-open silent-failure-shaped findings from
the cold audit.

## Scope (Wave 1 — this PR)

In:

- 3 scenarios against the local docker-compose dev stack
  (`make dev`):
  - `01-redis-down.sh` — full Redis container stop. Exercises the
    rate-limit middleware's fail-open behaviour and the
    Postgres-fed read fallback.
  - `02-timescale-down.sh` — full Timescale stop. Exercises the
    "fail loud, never silent-empty" contract on `/v1/markets`.
  - `03-redis-network-partition.sh` — Redis container reachable but
    silent (network partition / pumba pause). Exercises go-redis's
    timeout path, distinct from connection-refused.
- A bash runner (`run.sh`) with a production-safety guard, shared `lib/common.sh`, and gitignored per-run reports under `reports/`.

Out (deferred to Wave 2): HA-shaped scenarios (Patroni promotion, Sentinel failover, VRRP VIP flip; they need the staging bare-metal stack), cross-region chaos, API mid-stream kill with cursor resume, and aggregator tick stall with alert fire-time measurement (needs Prometheus and AlertManager in the dev stack).

## Why bash, not Go

Docker and pumba operations are shell-shaped (a Go test would be `exec.Command` boilerplate), each scenario must run standalone during a SEV drill, and the shape matches the k6 load suite.

## Scenario matrix (Wave 1 + 2)

| ID | Scenario | Wave | Tooling | Pass criteria |
|---|---|---|---|---|
| 01 | Redis container stop | 1 | `docker stop` | API healthz 200/503; recovers in 30s |
| 02 | Timescale container stop | 1 | `docker stop` | API fails loud (5xx OR cached); recovers in 60s |
| 03 | Redis network partition | 1 | `pumba pause` / `docker network disconnect` | ≤ 1 transient failure across 30s |
| 04 | Postgres primary kill (HA) | 2 | ansible inventory + `pkill postgres` | Patroni promotes replica within 30 s |
| 05 | Redis Sentinel master kill | 2 | systemd kill on r1 | Sentinel quorum elects new master ≤ 10 s; clients reconnect ≤ 30 s |
| 06 | HAProxy + keepalived VIP flip | 2 | systemctl stop haproxy on owner | VIP migrates ≤ 5 s; in-flight requests retry-OK |
| 07 | Galexie / MinIO node failure | 2 | docker stop / systemctl stop | erasure-coded reads keep serving |
| 08 | API pod mid-stream kill | 2 | systemctl restart stellarindex-api | SSE clients reconnect with cursor ≤ 5 s |
| 09 | Aggregator tick stall | 2 | `kill -STOP $(pgrep stellarindex-aggregator)` | cached values serve until TTL; `aggregator-silent` fires within 5m |
| 10 | ClickHouse server stop | 2 | systemctl stop clickhouse-server | lake-backed explorer routes 5xx or serve a flagged stale snapshot; recover on restart |

The dev compose stack has no ClickHouse, so row 10 cannot run in Wave 1.
Until it does, the lake-down contract is pinned in Go by
`internal/api/v1/explorer/lake_down_test.go`, which runs in `make test`.

## Production-safety guard

Every script (and the runner) refuses to execute when
`CHAOS_TARGET` matches `*production*`, `*api.stellarindex.io*`, or
`*prod.*`. The check is duplicated at the runner level AND inside
every scenario's prologue — defence in depth. Production chaos
runs out of the SEV playbook's quarterly drill, not this suite.

## Reporting

Per-run markdown under `reports/chaos-run-<UTC-timestamp>.md` (gitignored; local artefacts, unlike the committed SLA-proof report), one row per scenario, greppable for `❌`.

Wave 2's HA-shaped scenarios are gated on a staging bare-metal stack deployable from the ansible roles; staging deploys are queued post-launch.

## Open questions

1. **CI integration cadence.** Dev-stack chaos against CI's
   ephemeral docker every PR? Nightly only? On-demand via
   `workflow_dispatch`? Resolved: nightly plus on-demand
   (`chaos-nightly.yml`, cron `17 3 * * *` and
   `workflow_dispatch`). The docker-compose start-up cost (~30s)
   is too high for per-PR runs.

2. **Should the chaos suite block a release?** No. The SLA-proof
   report (Task #77) is the per-release artefact; chaos is an
   ongoing readiness exercise. Failed chaos = file a ticket, fix
   before next release; doesn't block this one.

3. **pumba vs docker network disconnect parity.** They exercise
   subtly different go-redis branches. Wave 1 prefers pumba when
   available but accepts the docker fallback because installing
   pumba per CI runner is friction. Track whether the fallback
   ever masks a real go-redis regression; if so, make pumba a
   hard requirement.
