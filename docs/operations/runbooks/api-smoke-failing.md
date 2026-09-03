---
title: Runbook — api-smoke-failing
last_verified: 2026-09-03
status: ratified
severity: P3
---

# Runbook — `stellarindex_api_smoke_failing`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_api_smoke_failing` |
| Severity | P3 (`severity: ticket`) |
| Detected by | `configs/prometheus/rules.r1/api-smoke.yml` (group `stellarindex.api_smoke`, `severity: ticket`, `for: 30m`) — the file r1 actually loads; multi-host twin in `deploy/monitoring/rules/api-smoke.yml`. |
| Typical MTTR | 30 min |
| Impact | At least one launch-critical endpoint is answering with the wrong shape or the wrong status. Customers integrating against the OpenAPI spec see it before we do unless this is worked. |

## Why this exists

The smoke is the only check that reads response **bodies**. The
per-binary heartbeats prove a process is listening on its metrics port;
the SLA probe measures latency and freshness. Neither can see
`/v1/coins` returning HTTP 200 without a `data` field, or a documented
`400 invalid-cursor` regressing into a silent 200 — the class of bug
that motivated the smoke's `expect_status` pins in the first place.

Until 2026-09-03 the smoke's only sink was `HEALTHCHECKS_URL_SMOKE`,
which has been empty on r1 since install, so that whole layer of shape
assertions ran every five minutes into the journal and nowhere else.
`smoke.sh` now writes
`/var/lib/node_exporter/textfile_collector/api_smoke.prom` on every run
and this alert is the delivered signal.

## Symptoms

- `stellarindex_api_smoke_failures > 0` for ≥ 30 min — six consecutive
  failing runs at the 5-minute cadence. The gauge is the smoke's exit
  code, which is its failed-check count.
- The 30-minute `for` is deliberate: a handful of the 34 checks are
  data-dependent (`/v1/ohlc` and `/v1/oracle/prices` accept a documented
  404 on an empty window; cold-cache responses can approach the 10 s
  per-request budget), so a single failing run is not news.
- The gauge is rewritten every run, so it clears within one cadence of
  the first clean run — it never carries an old verdict forward.

## Quick diagnosis (≤ 5 min)

```sh
ssh root@136.243.90.96

# 1. Which checks are failing? The wrapper captures the smoke's full
# output; each failing check prints its own line.
journalctl -u stellarindex-smoke.service --since "1 hour ago" -n 200

# 2. Current verdict as Prometheus sees it.
cat /var/lib/node_exporter/textfile_collector/api_smoke.prom

# 3. Reproduce on demand — the smoke is read-only and safe to re-run.
API_BASE_URL=http://localhost:3000 \
  bash /opt/stellarindex/healthchecks/r1-smoke.sh; echo "failed: $?"

# 4. Same surface from outside, to separate "the API is wrong" from
# "the loopback path is wrong" (Caddy, TLS, host routing).
API_BASE_URL=https://api.stellarindex.io bash scripts/dev/r1-smoke.sh
```

## Typical root causes

1. **A handler changed shape** — a serialiser field renamed or dropped,
   so the jq assertion fails against an otherwise-healthy 200. The
   failing check names the endpoint.
   - Mitigation: fix forward, or roll the API binary back to the
     previous version per `docs/operations/deploy-config-apply.md`.

2. **A documented 4xx regressed into a 200** — the `expect_status` pins
   exist for exactly this. Treat it as an API contract break, not a
   smoke bug: downstream clients branch on those statuses.

3. **A deploy landed the binary without its config** — a feature gated
   on ansible-rendered config ships dead, and the endpoint answers with
   the pre-feature shape. Check `config-apply-gate` on the deploy run
   and whether the archival-node role was applied.

4. **The deployed `r1-smoke.sh` is behind the repo** — the assertions on
   the host are older or newer than the API they run against. The
   archival-node role copies it (`17-stellarindex-healthchecks.yml`);
   confirm with `diff scripts/dev/r1-smoke.sh` against
   `/opt/stellarindex/healthchecks/r1-smoke.sh`.

5. **A genuinely broken dependency** — Postgres, Redis or ClickHouse
   down makes many checks fail at once. Then the API-plane alerts
   (`api-5xx`, `api-down`) are the primary signal and this one is
   downstream noise; work those first.

## Mitigation

- [ ] Step 1 — Read the failing check names from the journal.
- [ ] Step 2 — Reproduce with a manual run to confirm it is current and
      not a resolved blip still inside the `for` window.
- [ ] Step 3 — Apply the matching fix from "Typical root causes"; prefer
      a binary rollback over a live forward-fix during the window.
- [ ] Verification: `stellarindex_api_smoke_failures` returns to 0 at the
      next timer firing (≤ 5 min) and the alert clears.

## Known false-positive patterns

- **A single slow cold-cache run** — `/v1/markets?limit=5` on a cold
  hypertable has historically taken 6–8 s against a 10 s budget. One run
  can trip; six consecutive ones is a real regression, which is what the
  30-minute `for` encodes.
- **Empty data windows** — the checks that accept `200|404` already
  tolerate a quiet pair; a check that fails with a 404 outside that set
  is a real break, not an empty window.
- **A smoke run during a deploy** — the API restarts mid-run. Confirm
  against the deploy timeline before chasing a handler.

## Related

- `api-smoke-stale.md` — the companion alert for the smoke not running
  at all. One says the check found a problem; the other says the check
  did not report. Work them separately.
- `configs/healthchecks/smoke.sh` — the wrapper (Healthchecks ping +
  node_exporter textfile).
- `scripts/dev/r1-smoke.sh` — the checks themselves.
- `sla-probe-unit-failed.md` — the latency/freshness counterpart; the
  smoke asserts shape, the probe asserts speed.
- `api-5xx.md` / `api-down.md` — work these first when they are firing;
  a broken API makes the smoke fail as a consequence.

## Changelog

- 2026-09-03 — initial version, alongside the textfile metric + alert
  rules that gave the smoke a monitored sink.
