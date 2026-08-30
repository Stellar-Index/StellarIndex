---
title: Runbook — postgres_ping_failing
last_verified: 2026-08-29
status: ratified
severity: P1
---

# Runbook — `stellarindex_postgres_ping_failing`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_postgres_ping_failing` |
| Severity | P1 (page) |
| Detected by | `configs/prometheus/rules.r1/storage.yml` (the overlay r1 actually loads from `/etc/prometheus/rules.r1/*.yml`); multi-host template: `deploy/monitoring/rules/storage.yml`. Both trees carry the same expr. |
| Typical MTTR | 5 min |
| Impact | Indexer ingest is stalled or about to stall; live ledger writes failing every 60 s probe. |

## Why this exists

During the disk-full SEV cascade of 2026-05-26-27, a postgres
outage brought
`postgresql@15-main.service` down for ~10 h. Postgres recovered
when disk was freed, but the indexer's `*sql.DB` connection pool
held stale conns and silently failed writes for an additional ~3 h
until a manual `systemctl restart stellarindex-indexer`. Total
ledger gap: ~14 h.

The code fix shipped alongside this runbook:

1. `internal/storage/timescale/store.go` now sets
   `SetConnMaxLifetime(30 min)` + `SetConnMaxIdleTime(5 min)` —
   automatic pool refresh, bounds the cascade-gap to the lifetime
   interval.
2. `cmd/stellarindex-indexer/main.go::watchPostgresPing` probes the
   pool every 60 s and emits
   `stellarindex_postgres_ping_total{outcome="ok"|"error"}` plus the
   live streak gauge `stellarindex_postgres_ping_failure_streak`.

This alert fires on ANY nonzero ping-error rate sustained for
2 min (`rate(...{outcome="error"}[5m]) > 0`) — the live signal that
the safety-net hasn't refreshed yet AND something past the conn
layer is broken.

The original threshold was `> 0.5/s`, which was **unreachable**:
the probe ticks once per 60 s, so a permanently-wedged pool tops
out at 1/60 ≈ 0.0167/s — 30× below the trip point. The page could
never fire in its own target scenario. Corrected to `> 0` in both
rule trees on 2026-08-04; at the 60 s cadence a single failed probe
inside the 5 m window is already enough, and `for: 2m` rides through
a clean Postgres restart.

## Symptoms

- `rate(stellarindex_postgres_ping_total{outcome="error"}[5m]) > 0` for 2+ min.
- `stellarindex_postgres_ping_failure_streak` climbing past 3.
- Indexer journal: `pool may be wedged` log line.
- Downstream: `stellarindex_source_insert_errors_total{kind="trade"}`
  climbing, and/or `stellarindex_ingestion_trade_insert_backpressure`
  firing (post-ADR-0041 the sink RETRIES infrastructure faults rather
  than dropping, so backpressure is the earlier signal and a
  `kind="dropped"` increase means the bounded buffer overflowed —
  genuine loss). `stellarindex_trade_inserts_total` carries only
  `source` + `usd_volume_populated` labels; it has no `outcome`
  label to read here.

## Quick diagnosis (≤ 5 min)

```sh
# 1. Is postgres itself up? (F-0154 — use the CLUSTER name, not the umbrella.)
ssh root@136.243.90.96 'systemctl status postgresql@15-main.service'

# 2. Can a fresh client connect at all?
ssh root@136.243.90.96 'sudo -u postgres pg_isready -t 5'

# 3. What does the indexer journal say about the ping streak?
ssh root@136.243.90.96 'journalctl -u stellarindex-indexer.service --since "10 min ago" | grep -E "postgres.ping|pool may be wedged"'

# 4. Live metric on r1's prometheus (loopback-bound — query from the host):
ssh root@136.243.90.96 "curl -s http://localhost:9090/api/v1/query \
  --data-urlencode 'query=stellarindex_postgres_ping_failure_streak'" | jq .
```

## Mitigation (≤ 15 min)

If postgres@15-main is DOWN:

- [ ] Fix postgres first (see `db-disk-full.md` / `timescale-primary-down.md` depending on cause).
- [ ] The pool will refresh automatically within
      `PoolConnMaxLifetime` (30 min) once postgres is reachable,
      but you can force it now with a restart:
      `ssh root@136.243.90.96 'systemctl restart stellarindex-indexer.service'`
- [ ] Verification: ping streak resets to 0 + `outcome="ok"` rate climbs back.

If postgres is UP but ping still fails:

- [ ] Likely a network blip, firewall reset, or auth misconfig.
- [ ] Verify the DSN. There is no `/etc/default/stellarindex-indexer`
      on r1. The password-free DSN lives in the TOML as
      `postgres_dsn`; the unit's `EnvironmentFile=` is
      `/etc/default/stellarindex`, which carries the WITH-password
      override `STELLARINDEX_POSTGRES_DSN` (env wins):
      ```sh
      ssh root@136.243.90.96 'grep -n postgres_dsn /etc/stellarindex.toml'
      # redacts the password before it reaches your terminal/journal:
      ssh root@136.243.90.96 \
        "sed -n 's/^\(STELLARINDEX_POSTGRES_DSN=postgres:\/\/[^:]*\):[^@]*@/\\1:***@/p' /etc/default/stellarindex"
      ```
      Fix either one via ansible (`--check --diff` first, secrets via
      `ansible-vault edit inventory/r1.secrets.yml`), never by hand —
      a hand edit is reverted by the next playbook run.
- [ ] If pool wedged but DB healthy, restart the indexer to drain.

## Root cause analysis

- The 14 h cascade gap on 2026-05-26-27 root caused this whole
  resilience seam. Future post-mortems should record:
  - the streak length before the alert fired,
  - whether the lifetime safety-net or the manual restart refreshed
    the pool first.

## Known false-positive patterns

- A postgres restart will briefly fail pings — but
  `for: 2m` should ride through any clean restart. If you see this
  alert on every routine restart, the restart is taking too long
  (investigate separately).
- A network partition between indexer + DB will look identical to
  a pool problem from this alert's perspective; correlate with
  `up{job="postgres_exporter"}` (the scrape job landed 2026-05-27,
  F-0152 — see `exporter-down.md`).

## Related

- `internal/storage/timescale/store.go` — `configurePool` +
  `PingContext` (the implementation).
- `cmd/stellarindex-indexer/main.go::watchPostgresPing` —
  the probe goroutine.
- `db-disk-full.md` — the most common upstream cause.
- `timescale-primary-down.md` — adjacent alert; this one fires
  when the DB is reachable from prometheus but not from the
  indexer.

## Changelog

- 2026-05-27 — initial draft alongside the F-0151 resilience fix.
- 2026-08-29 — re-verified against HEAD (runbook re-verification
  wave K). Threshold corrected to the shipped `> 0` (the documented
  0.5/s was unreachable by 30× at the 60 s probe cadence, so the
  page could never fire); the downstream signal `trade_inserts_total{outcome="error"}`
  does not exist (no `outcome` label) and was replaced with
  `source_insert_errors_total` + the ADR-0041 backpressure alert;
  DSN lookup repointed at `/etc/stellarindex.toml` +
  `/etc/default/stellarindex`; F-0152 landed; host shapes → r1's IP.
