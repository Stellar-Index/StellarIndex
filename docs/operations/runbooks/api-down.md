---
title: Runbook — api-down
last_verified: 2026-08-28
status: ratified
severity: P1
---

# Runbook — `stellarindex_api_down`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_api_down` |
| Severity | P1 (page — SEV-1) |
| Detected by | `configs/prometheus/rules.r1/api.yml` (the r1 overlay loaded via `prometheus.r1.yml` → `/etc/prometheus/rules.r1/*.yml`; `deploy/monitoring/rules/api.yml` is the multi-host copy with underscored job names and is **not** what fires on r1) |
| Typical MTTR | 2–15 min |
| Impact | Complete public API outage — `/v1/price`, `/v1/history`, the Explorer/lake routes (`/v1/search` etc., ClickHouse-backed), everything. Every customer (Freighter, Stellar.expert, the lot) sees connection errors or Caddy's 502/503. |

## Symptoms

- `sum(up{job="stellarindex-api"}) == 0` for ≥ 60 s (`for: 60s`). On r1
  this is a **single** scrape target — `localhost:3000` with label
  `host=r1` (`configs/prometheus/prometheus.r1.yml`) — so one unit
  being down is the whole fleet being down.
- `/v1/healthz` and `/v1/readyz` return non-200 (or time out) when
  Caddy's active health check probes them.
- `caddy_reverse_proxy_upstreams_healthy` on `localhost:2019/metrics`
  reports 0 healthy upstreams.
- The pager fires on the `severity=page` label.

## Quick diagnosis (≤ 5 min)

Production is **one host**: `r1-01.stellarindex.io`
(`configs/ansible/inventory/r1.example.yml`). The edge is
Cloudflare → Caddy (`api.stellarindex.io`, `:443`, unit `caddy`) →
`reverse_proxy localhost:3000` → `stellarindex-api.service`
(`configs/ansible/roles/archival-node/templates/Caddyfile.j2`). The
API listens on loopback only (`stellarindex_api_listen_addr:
127.0.0.1:3000`), so external probes to `:3000` are *expected* to
fail. The HAProxy / keepalived / three-api-host topology in
[ADR-0008](../../adr/0008-ha-topology.md) and
[ha-plan.md](../../architecture/ha-plan.md) is a ratified design,
**Phase 1 not yet deployed** — the `haproxy` ansible role exists but
no playbook applies it.

```sh
# All on r1-01.

# Is the unit even running / parked in `failed`?
systemctl status stellarindex-api --no-pager | head -10

# Why did it stop? Last 100 log lines.
journalctl -u stellarindex-api -n 100 --no-pager

# Edge: is Caddy up, and is a maintenance-mode file left behind?
systemctl is-active caddy && ls -l /etc/caddy/MAINTENANCE_MODE
journalctl -u caddy -n 50 --no-pager

# Caddy's view of the upstream.
curl -s localhost:2019/metrics | grep caddy_reverse_proxy_upstreams_healthy

# If the unit is RUNNING but NOT serving: probe /v1/readyz directly.
# The response is an Envelope: {data:{status,uptime,checks:[{name,ok,error}]},as_of,flags}
curl -sS http://127.0.0.1:3000/v1/readyz | jq '.data.status, .data.checks'
```

`/v1/readyz` (handler `internal/api/v1/server.go::handleReadyz`,
checkers registered in `cmd/stellarindex-api/main.go`) runs these
checks:

| check | critical? | effect when red |
| ----- | --------- | --------------- |
| `postgres` (`storeChecker`) | yes | 503 |
| `schema` (`v1.NewSchemaVersionChecker`, REC-06 2026-08-14) | yes | 503 — applied migration head is below what the binary was built against, or dirty |
| `redis` (`redisChecker`) | no | 200 + `status="degraded"` |
| `clickhouse` (`clickhouseChecker`, only when `storage.clickhouse_addr` is set) | no | 200 + degraded; the lake routes 503 separately via their own lake-readiness probe |

Since wave 110 (F-1275) critical vs non-critical is distinguished
as above. Note that Caddy's active health check probes
**`/v1/healthz` only** (`health_uri /v1/healthz`, Caddyfile.j2), so a
readyz 503 does **not** drop the upstream — customers get the real
5xx from the API. If readyz reports red:

- `postgres` red → jump to [`timescale-primary-down.md`](timescale-primary-down.md).
- `schema` red → migrations vs binary mismatch. Compare the applied
  migration head with the deployed tag
  (`cat /var/lib/stellarindex/deployed-versions/stellarindex-api`,
  `curl -sf http://127.0.0.1:3000/v1/version`); run
  `stellarindex-migrate` for the deployed tag (or roll the binary
  back) before restarting.
- `redis` red → the API serves fail-open for rate limiting and
  degraded-envelope for price; this should *not* take the host out
  of Ready. If it does, that's a bug — file it.

## Typical root causes

1. **A bad release.** A new binary fails `config.Validate()`
   (`internal/config/validate.go`) at startup and exits non-zero
   before the first request is served. systemd records the unit as
   `failed` after `StartLimitBurst=10` retries inside
   `StartLimitIntervalSec=5min` (`RestartSec=10s` on r1). The
   exit reason is in `journalctl -u stellarindex-api`.

2. **Schema migration drift.** A binary whose expected migration
   head is ahead of (or dirty vs) the database now 503s `/v1/readyz`
   via the critical `schema` checker (REC-06, 2026-08-14) — it
   presents as readyz red with `checks[].name == "schema"`. If
   readyz is 200 and traffic 500s, that is not this alert — see
   [`api-5xx.md`](api-5xx.md).

3. **Credential / config-file rotation without a unit restart.**
   The API reads `/etc/stellarindex.toml` plus secrets
   (`STELLARINDEX_POSTGRES_DSN` with the password, the ClickHouse
   serving password, SEP-10 seed / JWT if set) from
   `/etc/default/stellarindex` via `EnvironmentFile=` (ansible
   `14-stellarindex-services.yml`, `stellarindex.env.j2`).
   `/etc/default/stellarindex-ops` is the `stellarindex-ops` /
   verify-archive env, **not** the API's. Rotating Redis or Postgres
   credentials without restarting the unit produces
   `authentication failed` log spam and a red `postgres` check.
   Note the unit only has `After=`/`Wants=` on postgresql/redis (no
   `Requires=`), so a Postgres stop does not stop the API — it
   fails readyz instead.

4. **Whole host down / Hetzner network.** Everything on r1 goes
   together — including Prometheus itself — so expect
   `stellarindex_deadmansswitch` / the heartbeat to fire rather
   than this alert.

5. **Caddy / Cloudflare / DNS, or a stale maintenance file.**
   `up{job="stellarindex-api"}` is scraped at `127.0.0.1:3000`
   directly, but customer traffic flows through Cloudflare → Caddy.
   A Caddy failure, a Cloudflare/DNS failure, or a leftover
   `/etc/caddy/MAINTENANCE_MODE` file (returns 503 to every request,
   evaluated per-request, no reload needed) means customers are
   down while `up==1` — this alert will **not** fire. Cross-check
   `journalctl -u caddy`, the sla-probe textfile metrics, and the
   Cloudflare dashboard.

6. **StartLimit exhaustion after a crash loop.** The unit is parked
   in `failed` and `systemctl restart` alone will not revive it;
   `systemctl reset-failed stellarindex-api` first, after fixing the
   cause.

## Mitigation (≤ 15 min)

- [ ] Step 1 — **declare SEV-1** in whatever incident channel you use.
      Downtime is user-visible and a breach of our service SLA.

- [ ] Step 2 — find the root cause via the diagnosis above. Do NOT
      blindly `systemctl restart stellarindex-api` on a binary that's
      crashing on startup — the restart loop will just burn the
      `StartLimitBurst` budget faster (and once burned you need
      `systemctl reset-failed stellarindex-api`).

- [ ] Step 3 — if the **last release** is the cause: roll back per
      [`release-process.md`](../release-process.md) → Rollback.
      Preferred:
      `gh workflow run deploy.yml -f region=r1 -f version=<prev-tag> -f binaries=stellarindex-api`
      (health-probes `/v1/healthz` and auto-rolls back on failure,
      `configs/ansible/tasks/deploy-one-binary.yml`). Manual fallback:
      `systemctl stop stellarindex-api && cp /usr/local/bin/stellarindex-api.prev-<prev-tag> /usr/local/bin/stellarindex-api && echo <prev-tag> > /var/lib/stellarindex/deployed-versions/stellarindex-api && systemctl start stellarindex-api`.
      On r1 there is **one** API instance: a rollback is a
      stop/swap/start and is itself a brief full outage — there is
      no rolling rollback.

- [ ] Step 4 — if **config or secret** is the cause: fix the source
      (commit to `configs/` or rotate the secret in vault), run
      the relevant ansible playbook to push it out, then
      `systemctl restart stellarindex-api`.

- [ ] Step 5 — if the repair will take a while: serve a clean 503
      instead of connection errors with
      `touch /etc/caddy/MAINTENANCE_MODE` (keeps `/v1/healthz` and
      `/metrics` alive, unlike stopping the unit). **`rm` it
      afterwards** — a forgotten file is a customer-visible outage
      that no alert catches (root cause 5).

- [ ] Step 6 — if **Caddy / edge** is the cause: `systemctl status
      caddy`, `journalctl -u caddy`, `caddy validate --config
      /etc/caddy/Caddyfile`, and confirm in Cloudflare that
      `api.stellarindex.io` still proxies to r1's IP.

- [ ] Verification: `up{job="stellarindex-api"}` returns to 1;
      `curl -sI https://api.stellarindex.io/v1/healthz` returns 200
      from outside the host; alert clears within 5 min.

## Root cause analysis

Gather for the postmortem:

- `journalctl -u stellarindex-api --since "30 min ago"`.
- Caddy's access/upstream log for the same window:
  `journalctl -u caddy --since "30 min ago"` (Caddy logs JSON to
  stdout → journald; there is no `/var/log/haproxy.log`).
- Prometheus screenshots: `up{job="stellarindex-api"}` (single
  series on r1), `http_requests_total`,
  `process_start_time_seconds` (restart count proxy).
- The release tag running before vs during the outage from
  [`deployed-versions.md`](../deployed-versions.md), the on-host
  sidecar `/var/lib/stellarindex/deployed-versions/stellarindex-api`,
  `/v1/version`, and `git tag`. (`r1-deployment-state.md` is a
  historical snapshot — do not read versions from it.) The
  `stellarindex_binary_version_skew` /
  `stellarindex_binary_version_probe_degraded` alerts
  (`rules.r1/binary-version-skew.yml`) tell you if the running
  binary differed from the deployed tag.
- Whether this alert alone fired or it was a symptom of an upstream
  (Timescale / Redis / host / DC) issue.

## Known false-positive patterns

- **Any API restart longer than 60 s.** Production is a single host,
  so every API deploy or restart briefly drives `up == 0`, and
  `for: 60s` means a restart that exceeds 60 s (StartLimit
  exhaustion, slow startup) **will page**. Cross-check
  `journalctl -u stellarindex-api` for a deploy-workflow restart in
  the window before treating it as an incident; do not assume this
  is a staging-only artefact.
- **Scrape-path breakage at the same time as a real outage.** If
  Prometheus cannot reach `127.0.0.1:3000`, you get `up == 0`
  identical to a real outage. Cross-check against Caddy's journald
  access log (real customer traffic) and the sla-probe textfile
  metrics (`stellarindex_sla_probe_availability_pct`,
  `stellarindex_sla_probe_unit_failed`, `rules.r1/sla-probe.yml`);
  `stellarindex_prometheus_scrape_failing` (`rules.r1/meta.yml`) is
  the scrape-path discriminator. If Caddy is still serving 200s
  while Prometheus says `up==0`, it's the scrape path, not the API.

## Related

- [`api-5xx.md`](api-5xx.md) — handlers returning errors but
  hosts healthy.
- [`api-latency.md`](api-latency.md) — slow but alive.
- [`timescale-primary-down.md`](timescale-primary-down.md),
  [`redis-master-down.md`](redis-master-down.md) — upstream
  failures that can cascade into readyz red.
- [`binary-version-skew.md`](binary-version-skew.md) — running
  binary != deployed tag.
- [HA plan §9](../../architecture/ha-plan.md) — degradation envelope.
- [`release-process.md`](../release-process.md) → Rollback — the
  binary-swap procedure for backing out a bad release.

## Changelog

- 2026-04-23 — initial draft. Lint-docs required a runbook for
  the page-severity alert.
- 2026-05-02 — converted from kubectl/k8s commands to systemd /
  HAProxy / journalctl, reflecting the bare-metal deployment
  ratified in ADR-0008.
- 2026-08-28 — re-verified against HEAD. Rewritten for the deployed
  r1 topology (single host, Cloudflare → Caddy → localhost:3000; the
  ADR-0008 HAProxy/keepalived tier is not deployed); alert expr is
  `up{job="stellarindex-api"}` from `configs/prometheus/rules.r1/api.yml`;
  readyz checks are `postgres`/`schema`/`redis`/`clickhouse` with the
  critical `schema` checker (REC-06); env file is
  `/etc/default/stellarindex` (not `-ops`); rollback is the deploy
  workflow / single-instance swap; maintenance-mode file as a hidden
  root cause and as the drain replacement.
