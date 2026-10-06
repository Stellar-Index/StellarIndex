---
title: Chaos suite Wave 1 — dev-stack execution
last_verified: 2026-10-05
status: operator runbook
---

# Chaos suite Wave 1 (dev stack)

Checks that each documented graceful-degradation path degrades gracefully. Suite
code: `test/chaos/`. Wave 2 (HA: Patroni failover, Sentinel quorum loss, region
cutover) is post-launch.

| Scenario | Kills | Expected | Runbook validated |
| --- | --- | --- | --- |
| `01-redis-down` | Redis container | `/v1/healthz` 200; `/v1/price/*` 200 or documented 503 (rate limit fails open, VWAP falls through to Postgres) | [cache](runbooks/cache.md#stellarindex_redis_master_down) |
| `02-timescale-down` | Timescale container | readiness flips; `/v1/price` 503 with structured envelope (no 5xx leak); recovers within 30s of restart | [timescale-primary-down](runbooks/timescale-primary-down.md) |
| `03-redis-network-partition` | iptables-drops Redis from API host | same as 01 | same as 01 |

## Run

```sh
make dev                                              # ~90s to healthy
curl -sf http://localhost:8080/v1/healthz | jq .      # expect {"status":"ok",...}
curl -sf "http://localhost:8080/v1/price?base=native&quote=fiat:USD" | jq .  # non-zero price
./test/chaos/run.sh                                   # all; or: ./test/chaos/run.sh 01 03
CHAOS_TARGET=http://staging.stellarindex.io:8080 ./test/chaos/run.sh   # staging, never production
```

Fix a failing pre-flight before chaos. The runner refuses `*.stellarindex.io`
production hosts (see the head of `run.sh`). Logs: `test/chaos/reports/<UTC-timestamp>/<scenario>.log`.

## Record the run

Keep `test/chaos/reports/<timestamp>/` (per-scenario logs), the runner's final
summary table, and a `RETRO.md` there: what the runbook missed, surprises, PRs
the run motivated.

Wave 1 closes when all 3 scenarios pass, the retro is free of real bugs, and the reports directory is committed.

## Verdicts

- Pass: scenario exits 0 (each `scenarios/0X-*.sh` asserts its own bar).
- Real fail: exit 1 and a documented behaviour did not happen. Fix the matching
  runbook or code, attach the reports directory, re-run.
- Flaky: exit 1 but re-runs clean. Fix the scenario's wait-for-stable loop, not production code.

Ctrl-C is safe (scenarios clean up via `trap`). Service logs:
`docker compose -f deploy/docker-compose/dev.yml logs <service>`.
Escalation for a bug found mid-launch: [sev-playbook.md](sev-playbook.md).
