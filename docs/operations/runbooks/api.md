---
title: Runbook — api alerts
last_verified: 2026-10-05
status: draft
---
# API alerts

One section per alert in `configs/prometheus/rules.r1/api.yml`.

## At a glance

- [`stellarindex_admin_audit_write_failing`](#stellarindex_admin_audit_write_failing)
- [`stellarindex_after_response_tasks_dropping`](#stellarindex_after_response_tasks_dropping)
- [`stellarindex_api_cache_miss_rate_high`](#stellarindex_api_cache_miss_rate_high)
- [`stellarindex_api_cache_refresh_failing`](#stellarindex_api_cache_refresh_failing)
- [`stellarindex_api_down`](#stellarindex_api_down)
- [`stellarindex_api_error_rate_critical`](#stellarindex_api_error_rate_critical)
- [`stellarindex_api_error_rate_high`](#stellarindex_api_error_rate_high)
- [`stellarindex_api_latency_p95_high`](#stellarindex_api_latency_p95_high)
- [`stellarindex_api_latency_p99_high`](#stellarindex_api_latency_p99_high)
- [`stellarindex_api_price_stale`](#stellarindex_api_price_stale)
- [`stellarindex_api_price_stream_not_delivering`](#stellarindex_api_price_stream_not_delivering)
- [`stellarindex_auth_reaper_stalled`](#stellarindex_auth_reaper_stalled)
- [`stellarindex_ch_schema_probe_absent`](#stellarindex_ch_schema_probe_absent)
- [`stellarindex_customer_webhook_delivery_exhausted`](#stellarindex_customer_webhook_delivery_exhausted)
- [`stellarindex_customer_webhook_delivery_failing`](#stellarindex_customer_webhook_delivery_failing)
- [`stellarindex_customer_webhook_mark_errors`](#stellarindex_customer_webhook_mark_errors)
- [`stellarindex_dex_tvl_refresh_failing`](#stellarindex_dex_tvl_refresh_failing)
- [`stellarindex_dex_tvl_total_divergent`](#stellarindex_dex_tvl_total_divergent)
- [`stellarindex_directory_sync_unflagged`](#stellarindex_directory_sync_unflagged)
- [`stellarindex_failed_auth_rate_high`](#stellarindex_failed_auth_rate_high)
- [`stellarindex_login_code_lockout_table_growing`](#stellarindex_login_code_lockout_table_growing)
- [`stellarindex_magic_link_token_table_growing`](#stellarindex_magic_link_token_table_growing)
- [`stellarindex_monthly_quota_fail_closed`](#stellarindex_monthly_quota_fail_closed)
- [`stellarindex_monthly_quota_fail_open`](#stellarindex_monthly_quota_fail_open)
- [`stellarindex_passkey_clone_warning`](#stellarindex_passkey_clone_warning)
- [`stellarindex_ratelimit_fail_closed`](#stellarindex_ratelimit_fail_closed)
- [`stellarindex_ratelimit_fail_open`](#stellarindex_ratelimit_fail_open)
- [`stellarindex_scam_gate_fail_open`](#stellarindex_scam_gate_fail_open)
- [`stellarindex_sdex_orderbook_advance_held`](#stellarindex_sdex_orderbook_advance_held)
- [`stellarindex_sdex_orderbook_crossed_book`](#stellarindex_sdex_orderbook_crossed_book)
- [`stellarindex_sdex_orderbook_maintain_failing`](#stellarindex_sdex_orderbook_maintain_failing)
- [`stellarindex_sdex_orderbook_reload_failing`](#stellarindex_sdex_orderbook_reload_failing)
- [`stellarindex_tls_cert_expiring_soon`](#stellarindex_tls_cert_expiring_soon)
- [`stellarindex_usage_rollup_failing`](#stellarindex_usage_rollup_failing)
- [`stellarindex_usage_write_failing`](#stellarindex_usage_write_failing)

## stellarindex_admin_audit_write_failing

**Trigger.** `sum by (surface) (increase(stellarindex_admin_audit_write_failures_total[1h])) > 0`, `for: 5m`, P3 (ticket). Rules: `configs/prometheus/rules.r1/api.yml` (r1), `deploy/monitoring/rules/api.yml` (multi-host). A single occurrence is enough (rare, bursty events).

**Impact.** Compliance/forensics, not availability. Audit appends are best-effort: the mutation already committed, the failure is logged and swallowed. Nothing is broken for customers. MTTR 15-60 min; recovery is time-boxed by log retention, so start it before fixing the cause.

**Surfaces** (`surface` label; what is now unrecorded):

| `surface` | Mutation | Unrecorded |
| --- | --- | --- |
| `account_override` | `PATCH /v1/admin/accounts/{id}` | Tier / rate-limit / quota override, reason, before-values |
| `key_mint` | `POST /v1/admin/keys`, `POST /v1/account/keys`, `POST /v1/dashboard/keys` | A live credential and who minted it for whom |
| `key_revoke` | Admin, account and dashboard key revoke | Which credential was revoked, by whom |
| `passkey_register` | `POST /v1/auth/passkey/finish-register` | New first-factor credential, session and IP that added it |
| `passkey_delete` | `DELETE /v1/auth/passkey/credentials/{id}` | Which passkey was removed, by which session |
| `passkey_clone_warning` | Refused passkey sign-in (refusal) | Which credential tripped the clone check; [passkey-clone-warning](api.md#stellarindex_passkey_clone_warning) still fires from the metric |
| `passkey_login_replay` | Refused passkey sign-in (refusal) | Which credential a replayed finish-login presented |
| `status_notice` | `POST /v1/admin/status-notices` (+ resolve) | A change to the public status page |
| `staff_customer_lookup` | `GET /v1/account/admin/lookup` (read) | That staff read a customer's billing email, tier/status and every user's email + last-login |
| `admin_account_read` | `GET /v1/admin/accounts/{id}` (read) | That an operator credential read an account's billing email, tier/status, overrides |

The two reads change nothing; the gap is the missing record of who looked at whose data. It cannot be rebuilt from the target object, only from the API request log.

**Diagnose** (at most 5 min).

1. Which surface: `{{ $labels.surface }}`; use the table.
2. Why the append failed (log line carries cause and target id):

   ```sh
   journalctl -u stellarindex-api --since '-2h' | grep 'audit append failed'
   ```

   Expect: Postgres unreachable/in recovery, `audit_log` disk full, a permissions regression on the table (`migrations/README.md` rule 7: object created as superuser instead of the `stellarindex` app role), or a statement timeout.
3. Still failing? `sum by (surface) (increase(stellarindex_admin_audit_write_failures_total[15m]))`. Zero = store recovered; only recovery remains.

**Fix.** Recovery first (it has the deadline).

1. Reconstruct missing rows from application logs before journal retention closes. Each site logs the target id (`minted_key_id`, `account_id`, `notice_id`, `event_id`); with the request log line (actor key id, identifier, `X-Reason`) that rebuilds the entry. `staff_customer_lookup`: line `staff customer lookup: audit append failed (best-effort)` carries `actor` (staff email) + `account_id`. `admin_account_read`: line `admin account read: audit append failed (best-effort)` carries `actor_key_id` + `account_id`.
2. Fix the store (Postgres availability / disk / permissions). Confirm writes land: `SELECT count(*) FROM audit_log WHERE created_at > now() - interval '1 day';`
3. Alert clears after an hour with no counted failure.

**Do not.** Roll back the mutation (it committed; reverting a tier override or revoking a minted key is itself an unaudited mutation). Do not make the append blocking: that turns an audit-store blip into a failed admin operation; best-effort is deliberate and this alert makes the gap visible.

**Related.** [monthly_quota_fail_open](api.md#stellarindex_monthly_quota_fail_open), the other best-effort path.

## stellarindex_after_response_tasks_dropping

**Trigger.** `sum(rate(stellarindex_after_response_tasks_dropped_total[5m])) > 0`, `for: 10m`, P3 (ticket). MTTR 5-30 min.

**Meaning.** `AfterResponse` (`internal/api/v1/middleware/after_response.go`) hands `UsageTracker` counter writes and `TouchUsage` debounced touches to a shared bounded worker pool. If the queue is full the task is dropped (never blocks the request) and the counter increments. Impact: lost usage-counter increments and last-seen updates; client responses unaffected, but a sustained rate is a silent revenue/observability leak.

**Diagnose.**

1. Correlated with a Redis/Postgres slowdown? Check `stellarindex_usage_units_dropped_total` and Redis/Postgres latency: a slow store keeps workers busy so the fixed pool backs up under normal load.
2. Request volume elevated? A spike alone can saturate the pool.

**Fix.**

- Slow/wedged store: follow the Redis/Postgres recovery path; the pool self-heals once tasks drain faster than they arrive.
- Sustained high volume: raise `afterResponseWorkers` / `afterResponseQueueSize` in `after_response.go`. This is a capacity decision, not an incident fix.

**Do not** treat a brief spike as an incident (`for: 10m` already filters it).

**Related.** [usage_write_failing](api.md#stellarindex_usage_write_failing), the store-write-failure signal for the same counters.

## stellarindex_api_error_rate_critical

SLA: availability budget (99.9 % non-5xx over 30 d) is the published target. Fast-burn trip point is 14.4 × 0.001 = 1.44 % 5xx, so a sustained 1.5 % error rate for 1h trips the burn alert just after `error_rate_high` (P3, > 1 %) and well before this > 5 % page; treat the urgency as budget exhaustion.

**Trigger.** `sum(rate(http_requests_total{job="stellarindex-api",status=~"5.."}[5m])) / sum(rate(http_requests_total{job="stellarindex-api"}[5m])) > 0.05`, `for: 2m`, P1 (`severity: page`). Rules: `configs/prometheus/rules.r1/api.yml` + `configs/prometheus/rules.r1/slo.yml` (r1, `job="stellarindex-api"`, loaded from `/etc/prometheus/rules.r1/*.yml`); multi-host templates `deploy/monitoring/rules/{api,slo}.yml`. Sibling: [stellarindex_api_error_rate_high](#stellarindex_api_error_rate_high) (>1%, P3).

This section also serves the SLO availability burn alerts `stellarindex_slo_availability_burn_{fast,medium,slow}` (P1 fast/medium, P3 slow); their `runbook_url` lands here. Per-tier detail: [slo-availability-burn-fast](slo-availability-burn-fast.md), [slo-availability-burn-medium](slo-availability-burn-medium.md), [slo-availability-burn-slow](slo-availability-burn-slow.md).

**Impact.** Clients see request failures. Affects the availability SLA (99.9% non-5xx over 30 d) and p95/p99 latency (5xx adds timeout retries). MTTR 5-15 min for a bad-deploy revert, 30-60 min for a latent-bug forward fix. Concurrent alerts likely: `stellarindex_api_latency_p95_high`, possibly `stellarindex_api_price_stale` if Timescale is the cause.

**Topology.** The `api-01..03` / HAProxy / keepalived shape ([ADR-0008](../../adr/0008-ha-topology.md) Phase 1) is inert on r1. Today: Cloudflare -> Caddy (`configs/caddy/Caddyfile.api`, TLS + health-checked `reverse_proxy localhost:3000`) -> one `stellarindex-api.service`. Multi-host equivalents are bracketed below for the eventual rollout.

**Burn-rate vs direct threshold.** Direct alerts trip on any sustained spike, including a deploy hiccup. Burn alerts fire on error-budget consumption: the 99.9% SLA gives a monthly budget of 0.1% of requests; both a short and a long window must agree (Google SRE workbook multi-window, ADR-0009). Fast (5m AND 1h, 14.4x) exhausts the 30-day budget in about 2 days; medium (30m AND 6h, 6x) about 5 days; slow (6h AND 24h, 1x) exactly 30 days. The fast trip point is 14.4 x 0.001 = 1.44%, so 1.5% errors for 1h pages: it lands after `error_rate_high` (>1%) and well before the >5% page. A `_burn_fast` page is a real availability emergency even if `error_rate_critical` has not fired; diagnose as below but treat urgency as budget exhaustion, not transient.

**Diagnose** (at most 5 min; first signal that flags non-zero wins).

1. What is failing:

   ```sh
   # Top status/route combinations by count in the last 5 min
   # (on r1, Prometheus is local on :9090)
   curl -s 'http://127.0.0.1:9090/api/v1/query' \
     --data-urlencode 'query=topk(5, sum by (route, status) (rate(http_requests_total{job="stellarindex-api",status=~"5.."}[5m])))' \
     | jq .data.result
   # (or click the Grafana link in the alert annotation)
   ```

   One dominant `{route, status="500"|"503"}` pair = specific handler. Errors across every route = shared infra (DB, Redis, upstream RPC): go to step 3.
2. Recent deploy:

   ```sh
   # Running version (r1: one host)
   curl -sf http://localhost:3000/v1/version | jq -r .data.version

   # When did the unit last (re)start?
   systemctl show stellarindex-api -p ActiveEnterTimestamp --value

   # Or: `r1-deployment-state.md` records the running tag.
   git log --oneline -1 docs/operations/r1-deployment-state.md

   # [Multi-host: loop the same two commands over api-01..03 via
   #  `ssh root@api-0X "..."` and compare versions across backends.]
   ```

   A release within about 1 h before the rise: revert (Fix A).
3. Dependency root cause, via readyz:

   ```sh
   curl -sSf https://api.stellarindex.io/v1/readyz | jq '.data'
   # Expected:
   # { "status": "ok" | "degraded",
   #   "checks": [ { "name": "postgres", "ok": true }, ... ] }
   ```

   - `postgres.ok == false`: [timescale-primary-down](timescale-primary-down.md).
   - `redis.ok == false`: [redis-master-down](redis-master-down.md).
   - All OK but 5xx elevated: handler bug (Fix B).
4. Log patterns. Access-log lines carry method/path/status/latency_ms/request_id but no `.level` and no `.err`; the ERROR-level lines with the underlying error are separate lines. This filter selects the latter:

   ```sh
   journalctl -u stellarindex-api --since '15 min ago' --no-pager -o cat \
     | jq -r 'select(.level=="ERROR") | [.msg, .err] | @tsv' \
     | sort | uniq -c | sort -rn | head
   ```

   - Panic stack trace: handler bug, Fix B.
   - `dial tcp ... connection refused`: upstream (DB/Redis/RPC), back to step 3.
   - `context deadline exceeded`: slow dependency; check dependency latency dashboards.
   - Handler error like `ErrPriceNotFound` at a higher-than-normal rate: data issue, not a production incident; suppress if sustained.

**Fix** (pick by diagnosis; not sequential).

- **A. Recent deploy: revert.** Fastest path. On r1 redeploy the previous tag:

  ```sh
  gh workflow run deploy.yml -f region=r1 -f version=<previous-tag> \
    -f binaries=stellarindex-api
  ```

  The workflow does stage, backup, atomic install, restart, health probe, and automatic rollback on probe failure (`deploy-workflow.md`); the auto-rollback may already have run, so check before redeploying. Manual fallback per [`release-process.md`](../release-process.md) -> Rollback: swap back to `/usr/local/bin/stellarindex-api.prev-<previous-tag>` (the deploy keeps the last 5; `/var/lib/stellarindex/deployed-versions/stellarindex-api` tracks the running version; installs go to `/usr/local/bin/`, not `/opt/stellarindex/release-<tag>/`) and restart the unit. [Multi-host: rolling version: drain one host from HAProxy via the admin socket (`disable server api_pool/api-01`), swap binary, restart, re-enable, repeat for `api-02`, `api-03`.]

  Verify: `error_rate_critical` clears within 3 min; `/v1/healthz` returns 200; `/v1/readyz` returns `status=ok` on at least 3 consecutive polls; `curl -sf http://localhost:3000/v1/version | jq -r .data.version` reports the previous tag [multi-host: on every backend]. After containment, file a postmortem item on why CI + rolling deploy missed it.
- **B. Handler bug, no recent deploy: gate + fix forward.** Panics are usually a nil-dereference on an unexpected input shape; Recoverer returns 500 problem+json so the process survives but 5xx climbs. If isolated to one endpoint:
  1. Caddy path gate (r1, at most 2 min). Add a block ABOVE the catch-all `reverse_proxy` in the `api.stellarindex.io` site of `/etc/caddy/Caddyfile` (repo source `configs/caddy/Caddyfile.api`), validate, reload (`systemctl reload caddy` is graceful):

     ```caddyfile
     handle /v1/history* {
         respond `{"type":"about:blank","title":"endpoint temporarily disabled","status":503}` 503 {
             close
         }
     }
     ```

     ```sh
     caddy validate --config /etc/caddy/Caddyfile && systemctl reload caddy
     ```

     Codify it in `configs/caddy/Caddyfile.api` in the same PR as the fix (r1 is ansible-managed; hand fixes page Monday morning) and remove the gate after the fixed deploy. [Multi-host: HAProxy equivalent: `http-request return status 503 content-type "application/problem+json" string "..." if { path_beg /v1/history }` in backend `api_pool`, `haproxy -c` to validate, `systemctl reload haproxy`, on both `lb-01` and `lb-02`.]
  2. Feature-flag deny (if a flag exists for the endpoint): edit `/etc/stellarindex.toml` (via the ansible overlay), then `systemctl restart stellarindex-api`.

  If the bug hits every handler (e.g. middleware panic), treat as A even if the deploy is not recent: roll back to last-known-good; you cannot path-gate around middleware.
- **C. Dependency failure.** Follow the dependency's runbook: [timescale-primary-down](timescale-primary-down.md), [redis-master-down](redis-master-down.md), [all-ingestion-down](all-ingestion-down.md). This alert auto-resolves once the dep recovers.
- **D. Load-induced** (viral traffic, DDoS): error rate climbs with no deploy, dep failure or log pattern; `stellarindex_api_latency_p99_high` fires in tandem; `http_requests_total` rate sharply above baseline. Bare metal does not auto-scale and r1 is fixed capacity [multi-host: fixed per ADR-0008 section 4], so shed load:
  1. Tighten edge rate limits: Cloudflare WAF short-TTL per-IP rule (the API is CF-fronted; the Caddyfile `trusted_proxies` block exists for this).
  2. Drop the heaviest non-essential paths (SSE `/v1/price/stream`, batch reads) with a temporary Caddy `handle`/`respond 503` gate as in B.1 [multi-host: HAProxy 503 equivalent].
  3. Promote AWS DR only if the colo is genuinely saturated and 1+2 do not clear within 10 min. SEV-1 escalation, heavyweight DNS flip: follow [`dr-activation.md`](dr-activation.md) (full R1 -> R2/R3 cutover); warm-standby bring-up is [`ha-plan.md`](../../architecture/ha-plan.md) section 2.2.

**Root cause evidence** (postmortem, section 6 of [sev-playbook](../sev-playbook.md)).

- `journalctl -u stellarindex-api --since '1h ago' --no-pager` [multi-host: over `ssh root@api-0X` on each host].
- Grafana screenshot of the 1 h window; `git log -n 20 main` for a deploy trigger.
- `systemctl status stellarindex-api --no-pager` for restarts; `dmesg | grep -i oom` for OOM kills.
- Caddy log for the window: `journalctl -u caddy --since '1h ago' --no-pager -o cat` (upstream health transitions, Caddy-generated 502/503) [multi-host: `/var/log/haproxy.log` per backend].
- Recovered panics: stack traces + request_ids for fixtures. Timescale involved: slow-query log for the window.

Common patterns:

1. Nil-pointer in a handler on a new input shape: validate input earlier, add a test.
2. Timescale primary down: every `/v1/price` falling through to `LatestTradesForPair` returns 500. Fix per the dependency runbook; consider a short-term Redis-only fallback with `reduced_redundancy=true` in the envelope.
3. OOM on a batch endpoint (e.g. `asset_ids=<1000 assets>`): hard-cap batch size in the handler. `openapi/stellar-index.v1.yaml` `/price/batch` commits to max 100 ids for GET and 1000 for POST; verify enforcement.
4. Context deadline on a slow CAGG query (first request of the day hits a cold CAGG partition): keep-warm job querying each CAGG every few minutes.

**False positives.** Synthetic monitoring 4xx to unknown assets is not 5xx and does not trigger this. Minute-zero after a release: the restarted host briefly serves 503 until `/v1/readyz` is green (on r1 Caddy's 10s `/v1/healthz` active check bounds it; Caddy-generated 502/503 are not in `http_requests_total`). The 2 min window means a normal release does not trip it [multi-host: HAProxy 10s `slowstart` + readyz bound it to seconds per host]. If it fires during a planned rollout, the deploy script should silence the alert.

**Related.** [api_down](#stellarindex_api_down) (every backend down, not just erroring); [api_latency_p95_high](#stellarindex_api_latency_p95_high) (parallel when 5xx is timeouts); [timescale-primary-down](timescale-primary-down.md); [release-process.md](../release-process.md) -> Rollback; [sev-playbook](../sev-playbook.md) sections 3 (detection), 4 (response flow), 5 (public-comms templates); [alerts-catalog](../alerts-catalog.md); [ha-plan.md](../../architecture/ha-plan.md) section 9 (degradation flags `stale`, `reduced_redundancy`).

## stellarindex_api_error_rate_high

**Trigger.** Same expression as [stellarindex_api_error_rate_critical](#stellarindex_api_error_rate_critical) with threshold `> 0.01` (5xx share over 1%), `for: 2m`, P3 (`severity: ticket`, not a page).

**Differences.** Lower severity and threshold; early warning that usually lands before the >5% page and before `slo_availability_burn_fast` (trip point 1.44%). A sustained rate at or above 1.44% (the `_burn_fast` trip point) for 1h is real budget burn: treat it with the same urgency as a `_burn_fast` page. A brief deploy hiccup can trip it.

**Diagnose and fix.** Identical to the critical section: top 5xx routes, deploy check, `/v1/readyz`, ERROR-log grouping; then revert (A), gate + fix forward (B), dependency runbook (C), or shed load (D). Verify that `error_rate_high` clears and the rate falls well below 1%. Known false positives (minute-zero after release) as in the critical section.

## stellarindex_api_down

**Trigger.** `sum(up{job="stellarindex-api"}) == 0` for `for: 60s` (on r1 one scrape target, `localhost:3000`, label `host=r1`, `configs/prometheus/prometheus.r1.yml`, so one unit down is the whole fleet down), OR `absent_over_time(up{job="stellarindex-api"}[5m]) == 1` (no `up` series for 5 min: Prometheus no longer scrapes the API; `sum()` over an empty vector is empty, so without this arm a dropped target would silence the page). Rule: `configs/prometheus/rules.r1/api.yml` (r1 overlay loaded via `prometheus.r1.yml` -> `/etc/prometheus/rules.r1/*.yml`; `deploy/monitoring/rules/api.yml` is the multi-host copy with underscored job names and does not fire on r1). P1 (page, SEV-1). MTTR 2-15 min.

**Impact.** Complete public API outage (`/v1/price`, `/v1/history`, Explorer/lake routes such as `/v1/search`, everything). Customers see connection errors or Caddy 502/503.

**Symptoms.** `/v1/healthz` and `/v1/readyz` non-200 or timing out under Caddy's active health check; `caddy_reverse_proxy_upstreams_healthy` on `localhost:2019/metrics` shows 0 healthy. If fired via the absent arm, diagnose the SCRAPE layer first: the API may be serving fine while the outage detector is blind, which is itself pageable.

**Topology.** One host, `r1-01.stellarindex.io`. Edge: Cloudflare -> Caddy (`api.stellarindex.io`, `:443`, unit `caddy`) -> `reverse_proxy localhost:3000` -> `stellarindex-api.service` (`configs/ansible/roles/archival-node/templates/Caddyfile.j2`). The API listens on loopback only (`stellarindex_api_listen_addr: 127.0.0.1:3000`), so external probes to `:3000` are expected to fail. The HAProxy/keepalived/three-host topology ([ADR-0008](../../adr/0008-ha-topology.md), [ha-plan.md](../../architecture/ha-plan.md)) is not deployed (the `haproxy` role exists, no playbook applies it).

**Diagnose** (at most 5 min, all on r1-01).

```sh
# Is the unit even running / parked in `failed`?
systemctl status stellarindex-api --no-pager | head -10

# Why did it stop? Last 100 log lines.
journalctl -u stellarindex-api -n 100 --no-pager

# Edge: is Caddy up, and is a maintenance-mode file left behind?
systemctl is-active caddy && ls -l /etc/caddy/MAINTENANCE_MODE
journalctl -u caddy -n 50 --no-pager

# Caddy's view of the upstream.
curl -s localhost:2019/metrics | grep caddy_reverse_proxy_upstreams_healthy

# Fired via the absent_over_time arm (no `up` series at all)? Then the
# fault is in Prometheus, not the API: the job is gone from the scrape
# config or the target never resolved.
curl -s localhost:9090/api/v1/targets | jq '.data.activeTargets[]
  | select(.labels.job == "stellarindex-api")
  | {health, lastError, scrapeUrl}'
grep -n 'stellarindex-api' /etc/prometheus/prometheus.yml

# If the unit is RUNNING but NOT serving: probe /v1/readyz directly.
# The response is an Envelope: {data:{status,uptime,checks:[{name,ok,error}]},as_of,flags}
curl -sS http://127.0.0.1:3000/v1/readyz | jq '.data.status, .data.checks'
```

`/v1/readyz` (`internal/api/v1/server.go::handleReadyz`, checkers registered in `cmd/stellarindex-api/main.go`):

| check | critical? | effect when red |
| ----- | --------- | --------------- |
| `postgres` (`storeChecker`) | yes | 503 |
| `schema` (`v1.NewSchemaVersionChecker`) | yes | 503: applied schema (dirty rollbacks resolved to their pre-attempt version) is below what the binary was built against, or the dirty migration is the non-atomic exception (`nonAtomicMigrationVersions`, currently only 0030) whose state cannot be inferred |
| `schema-dirty` (`v1.NewSchemaDirtyChecker`) | no | 200 + `status="degraded"`: `schema_migrations` dirty but rollback was atomic and applied schema still satisfies the binary; needs an operator `force`, API keeps serving |
| `nonstandard_decimals` (`nonstandardDecimalsChecker`) | yes | 503: cache never loaded, so confirmed non-7-decimal assets would serve raw prices; see [`dex-nonstandard-decimals.md`](dex-nonstandard-decimals.md) |
| `closed_buckets` (`v1.NewClosedBucketChecker`) | yes | 503: a continuous aggregate outside the real-time allowlist has `materialized_only = false`, so unguarded readers serve its open bucket; see [`dependency-down.md`](dependency-down.md#if-dependencyclosed_buckets) |
| `redis` (`redisChecker`) | no | 200 + `status="degraded"` |
| `clickhouse` (`clickhouseChecker`, only when `storage.clickhouse_addr` is set) | no | 200 + degraded; lake routes 503 separately via their own readiness probe |

Caddy's active health check probes `/v1/healthz` only (`health_uri /v1/healthz`, Caddyfile.j2), so a readyz 503 does not drop the upstream; customers get the real 5xx from the API. If readyz is red:

- `postgres`: [`timescale-primary-down.md`](timescale-primary-down.md).
- `schema`: migrations vs binary mismatch. Compare applied migration head with the deployed tag (`cat /var/lib/stellarindex/deployed-versions/stellarindex-api`, `curl -sf http://127.0.0.1:3000/v1/version`); run `stellarindex-migrate` for the deployed tag (or roll the binary back) before restarting.
- `redis`: API serves fail-open for rate limiting and degraded envelope for price; this should not take the host out of Ready. If it does, file a bug.

**Typical root causes.**

1. Bad release: new binary fails `config.Validate()` (`internal/config/validate.go`) at startup and exits non-zero; systemd marks the unit `failed` after `StartLimitBurst=10` retries within `StartLimitIntervalSec=5min` (`RestartSec=10s` on r1). Reason is in `journalctl -u stellarindex-api`.
2. Schema migration drift: binary ahead of (or dirty vs) the DB 503s readyz via the critical `schema` checker (`checks[].name == "schema"`). If readyz is 200 and traffic 500s, this is not this alert: see [stellarindex_api_error_rate_critical](#stellarindex_api_error_rate_critical).
3. Credential/config rotation without a unit restart: the API reads `/etc/stellarindex.toml` plus secrets (`STELLARINDEX_POSTGRES_DSN`, ClickHouse serving password, SEP-10 seed / JWT if set) from `/etc/default/stellarindex` via `EnvironmentFile=` (ansible `14-stellarindex-services.yml`, `stellarindex.env.j2`). `/etc/default/stellarindex-ops` is the ops/verify-archive env, not the API's. Rotating Redis or Postgres credentials without restart gives `authentication failed` spam and a red `postgres` check. The unit has only `After=`/`Wants=` on postgresql/redis (no `Requires=`), so a Postgres stop fails readyz rather than stopping the API.
4. Whole host / Hetzner network down: everything including Prometheus goes; expect `stellarindex_deadmansswitch` / heartbeat to fire instead.
5. Caddy / Cloudflare / DNS, or a stale `/etc/caddy/MAINTENANCE_MODE` (503 to every request, evaluated per request, no reload needed): customers are down while `up==1` and this alert does NOT fire. Cross-check `journalctl -u caddy`, sla-probe textfile metrics, Cloudflare dashboard.
6. StartLimit exhaustion after a crash loop: unit parked in `failed`; after fixing the cause run `systemctl reset-failed stellarindex-api` before restarting.

**Fix** (at most 15 min).

1. Declare SEV-1.
2. Find the root cause first. Do not blindly `systemctl restart stellarindex-api` on a binary crashing at startup: it burns the `StartLimitBurst` budget faster.
3. Last release is the cause: roll back per [`release-process.md`](../release-process.md) -> Rollback. Preferred:

   ```sh
   gh workflow run deploy.yml -f region=r1 -f version=<prev-tag> -f binaries=stellarindex-api
   ```

   (health-probes `/v1/readyz`, auto-rolls back on failure; `configs/ansible/tasks/deploy-one-binary.yml`, `api_health_path` in `deploy-binary.yml`). Manual fallback:

   ```sh
   systemctl stop stellarindex-api && cp /usr/local/bin/stellarindex-api.prev-<prev-tag> /usr/local/bin/stellarindex-api && echo <prev-tag> > /var/lib/stellarindex/deployed-versions/stellarindex-api && systemctl start stellarindex-api
   ```

   One API instance: rollback is stop/swap/start, itself a brief full outage; no rolling rollback.
4. Config or secret cause: fix the source (commit to `configs/` or rotate in vault), run the relevant ansible playbook, then `systemctl restart stellarindex-api`.
5. Repair will take a while: serve a clean 503 instead of connection errors with `touch /etc/caddy/MAINTENANCE_MODE` (keeps `/v1/healthz` and `/metrics` alive, unlike stopping the unit). `rm` it afterwards; a forgotten file is a customer-visible outage no alert catches (cause 5).
6. Caddy/edge cause: `systemctl status caddy`, `journalctl -u caddy`, `caddy validate --config /etc/caddy/Caddyfile`, confirm in Cloudflare that `api.stellarindex.io` still proxies to r1's IP.
7. Verify: `up{job="stellarindex-api"}` returns to 1; `curl -sI https://api.stellarindex.io/v1/healthz` returns 200 from outside the host; alert clears within 5 min.

**Root cause evidence.**

- `journalctl -u stellarindex-api --since "30 min ago"`; Caddy log `journalctl -u caddy --since "30 min ago"` (JSON to journald; no `/var/log/haproxy.log`).
- Prometheus: `up{job="stellarindex-api"}`, `http_requests_total`, `process_start_time_seconds` (restart proxy).
- Release tag before vs during: [`deployed-versions.md`](../deployed-versions.md), `/var/lib/stellarindex/deployed-versions/stellarindex-api`, `/v1/version`, `git tag` (`r1-deployment-state.md` is a historical snapshot; do not read versions from it). `stellarindex_binary_version_skew` / `stellarindex_binary_version_probe_degraded` (`rules.r1/binary-version-skew.yml`) show if the running binary differed from the deployed tag.
- Whether this alert fired alone or was a symptom of Timescale / Redis / host / DC.

**False positives.**

- Any API restart over 60 s: single host, so every deploy/restart briefly drives `up == 0` and `for: 60s` means a restart exceeding 60 s (StartLimit exhaustion, slow startup) pages. Check `journalctl -u stellarindex-api` for a deploy-workflow restart before treating it as an incident.
- Scrape-path breakage: if Prometheus cannot reach `127.0.0.1:3000`, `up == 0` looks like a real outage. Cross-check Caddy's journald access log (real traffic) and sla-probe metrics (`stellarindex_sla_probe_availability_pct`, `stellarindex_sla_probe_unit_failed`, `rules.r1/sla-probe.yml`); `stellarindex_prometheus_scrape_failing` (`rules.r1/meta.yml`) discriminates the scrape path. Caddy serving 200s while Prometheus says `up==0` = scrape path, not the API.

**Related.** [stellarindex_api_error_rate_critical](#stellarindex_api_error_rate_critical) (handlers erroring, hosts healthy); [stellarindex_api_latency_p95_high](#stellarindex_api_latency_p95_high); [`timescale-primary-down.md`](timescale-primary-down.md), [`redis-master-down.md`](redis-master-down.md); [`binary-version-skew.md`](binary-version-skew.md); [HA plan section 9](../../architecture/ha-plan.md); [`release-process.md`](../release-process.md) -> Rollback.

## stellarindex_api_latency_p95_high

**Trigger.** `histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{job="stellarindex-api"}[5m]))) > 0.5`, `for: 10m` (not 2m: the p95 is already 5m-windowed), `severity: ticket`. Rule: `configs/prometheus/rules.r1/api.yml` (group `stellarindex.api`, the file r1 loads); multi-host twin `deploy/monitoring/rules/api.yml`. Sibling: [stellarindex_api_latency_p99_high](#stellarindex_api_latency_p99_high) (p99 > 2 s).

This section also serves `stellarindex_slo_latency_burn_{fast,medium,slow}` (`configs/prometheus/rules.r1/slo.yml`, group `stellarindex.slo.latency`; twin `deploy/monitoring/rules/slo.yml`). Per-tier detail: [slo-latency-burn-fast](slo-latency-burn-fast.md), [slo-latency-burn-medium](slo-latency-burn-medium.md), [slo-latency-burn-slow](slo-latency-burn-slow.md). Severity: fast = `page` (`for: 2m`), medium = `page` (`for: 5m`), slow = `ticket` (`for: 30m`). Every burn tier carries a min-signal guard `stellarindex:api_slow_request_count:1h > 5` (absolute count of bad, slow-or-error, requests over the trailing hour) so one cold-cache outlier in a near-empty window cannot trip the budget; it does not depend on total traffic volume.

**Burn vs direct.** Direct alerts are an immediate "something changed" but noisy (one bad bucket can trip them). Burn alerts need both a short and a long window to agree: fast (5m AND 1h, 14.4x) exhausts budget in hours, medium (30m AND 6h, 6x) in days, slow (6h AND 24h, 1x) in about weeks. A `_burn_fast` page uses the same diagnosis but is SEV-1-worthy burn: aim for a real fix, not tolerating the symptom until the alert clears.

**Impact.** Requests complete but slowly; clients with tight timeouts may give up and retry. Not an outage but breaches the SLA (p95 <= 200 ms, p99 <= 500 ms); thresholds are 2.5x / 4x the SLA for lead time. MTTR 15-60 min.

**Symptoms.** Per-endpoint panel on the API -> latency dashboard shows the route with the tail, usually `/v1/vwap` or `/v1/history` (CAGG-backed), not `/v1/price` (Redis hot path). Client-facing: higher 499 rate as downstreams time out.

**Diagnose** (at most 5 min).

```sh
# Which endpoint is slow? (Prometheus listens on localhost:9090 on r1)
ssh root@136.243.90.96 "curl -s http://localhost:9090/api/v1/query --data-urlencode \
  'query=histogram_quantile(0.95, sum by (route, le) (rate(http_request_duration_seconds_bucket{job=~\"stellarindex[_-]api\"}[5m])))'" | \
  jq -r '.data.result[] | "\(.metric.route): \(.value[1])s"' | sort -k2 -rn | head

# Is Redis healthy? Cache-miss storms are the common trigger.
# (r1's Redis is local — there is no `redis` hostname.)
ssh root@136.243.90.96 'redis-cli --latency-history'  # in another pane
# The API serves /metrics on its public listener (:3000) — :9464 is
# the INDEXER's metrics port. The relevant cache metric is the
# in-memory API cache counter:
ssh root@136.243.90.96 "curl -s http://localhost:3000/metrics | grep 'stellarindex_api_cache_ops_total.*miss'"

# Is Timescale the bottleneck?
ssh root@136.243.90.96 'runuser -u postgres -- psql -d stellarindex -c "
  SELECT state, count(*), max(now()-query_start) AS oldest
  FROM pg_stat_activity WHERE application_name LIKE '"'"'stellarindex%'"'"'
  GROUP BY state;"'
```

**Typical causes** (roughly by frequency).

1. Redis cache-miss storm: a popular asset's price key is evicted/TTL'd, every `/v1/price?asset=X` becomes a Timescale query, Timescale saturates, pileup. Signal: `stellarindex_api_cache_ops_total{result="miss"}` rate jumps per `(cache, op)` (see [stellarindex_api_cache_miss_rate_high](#stellarindex_api_cache_miss_rate_high)); Redis `keyspace_misses` / `evicted_keys` climbing in `INFO stats`. Fix: warm the cache or scale Redis memory; `redis-memory.md`.
2. Timescale contention: a long-running query (manual backfill, an unbounded exploratory `SELECT`) holds locks or fills the pool. Signal: `pg_stat_activity` shows a query older than 30 s. Fix: `SELECT pg_cancel_backend(pid)` on the offender after confirming it is not production traffic.
3. CAGG not refreshed: `/v1/vwap` / `/v1/twap` fall back to raw-trades aggregation (O(trades), seconds). Signal: `stellarindex_timescale_cagg_stale` usually fires too. Fix: refresh the CAGG manually; `cagg-stale.md`.
4. Noisy neighbor on the host pegging CPU or IO. Signal: `stellarindex_host_cpu_high` on the same instance. Fix: scale horizontally or move to a dedicated node.
5. GC pressure from a runaway allocation pattern (code issue). Signal: `go_gc_duration_seconds` quantile rises, latency tracks GC pauses. Fix: profile + code PR.

**Fix.**

1. Narrow to the slow endpoint (above); walk causes in order, each has a faster check than the next.
2. Redis-driven: `redis-memory.md`. Timescale-driven: `pg-conns-saturated.md` or `replica-lag.md` (replica-lag is multi-host only; inert on r1, no replica).
3. Scale the API up only if the hot path is CPU-bound and other causes are ruled out; it is a bandaid, file a follow-up.
4. Verify: p95 back under 200 ms (SLA target, not the 500 ms alarm threshold) for 15 min; do not leave it oscillating between 200 and 500 ms.

**Root cause evidence.** Per-route latency histograms for the window; `pg_stat_statements` top-N by `total_exec_time`; Redis `INFO stats` delta (`keyspace_misses`, `evicted_keys`); Timescale disk/IO and API host CPU dashboards.

**False positives.**

- Midnight UTC: the 24h-trade-count window rolls over and the first request each hour for a rarely queried asset pays cold-cache cost; `for: 10m` absorbs it.
- Large `limit=500` `/v1/markets` scan after a fresh deploy with cold Timescale buffers; warms within a minute.
- `/v1/markets` baseline above 200 ms: it does `GROUP BY base_asset, quote_asset` across the 14-day chunk window of the trades hypertable. Baseline about 540 ms cold / 50 ms warm; during a concurrent heavy backfill cold balloons to about 7 s and warm settles about 400 ms (backfill writes evict recent chunks from shared buffers; columnstore-compress policy lags). Per-route SLA carve-out p95 <= 300 ms / p99 <= 1 s; during backfill it is exceeded and the global p95 > 500 ms alert may fire on the first request after a deploy or buffer churn. Transient load artefact, not a route regression; warm returns to about 50 ms once the backfill completes.

**Related.** [stellarindex_api_error_rate_critical](#stellarindex_api_error_rate_critical) (errors, not slowness); `redis-memory.md`, `cagg-stale.md`, `pg-conns-saturated.md`.

## stellarindex_api_latency_p99_high

**Trigger.** `histogram_quantile(0.99, sum by (le) (rate(http_request_duration_seconds_bucket{job="stellarindex-api"}[5m]))) > 2`, `for: 10m` (rides out transient tail bursts such as deploy cold start or a one-off heavy scan; fires only on sustained p99 regression), `severity: ticket`. SLA target p99 <= 500 ms (threshold is 4x).

**Differences.** Tail-only: p95 may be fine, so a few slow routes or a periodic heavy query is likelier than a global slowdown. Burn-rate variants and the `stellarindex:api_slow_request_count:1h > 5` guard are described in [stellarindex_api_latency_p95_high](#stellarindex_api_latency_p95_high).

**Diagnose and fix.** Same as p95: the per-route query (use `histogram_quantile(0.99, ...)` to rank), Redis cache-miss check, `pg_stat_activity` long-query check, then the cause list (cache-miss storm, Timescale contention, stale CAGG, noisy neighbor, GC). Same false positives, notably the `/v1/markets` cold-scan baseline during backfill. Verify with p99 back under 500 ms (SLA) for 15 min.

## stellarindex_api_cache_miss_rate_high

**Trigger.** Per `(cache, op)`: `sum by (cache, op) (rate(stellarindex_api_cache_ops_total{result="miss"}[5m])) / sum by (cache, op) (rate(stellarindex_api_cache_ops_total{result=~"hit|miss|stale"}[5m])) > 0.5`, gated by a read-rate floor `> 0.1 req/s`, `for: 10m`, P2 (`severity: ticket`). Rule: `configs/prometheus/rules.r1/api.yml` (group `stellarindex.api`); twin in `deploy/monitoring/rules/api.yml`. The denominator counts READ outcomes only: `result="evicted"` and `result="refresh_error"` are side-events (leaving them in made the alert unfirable on a bounded cache under key enumeration, one `evicted` per `miss` capping the ratio at 0.5). The floor stops low-traffic caches with 100% miss from flapping; total request volume is not part of the signal. MTTR 30-60 min (mostly diff-and-deploy time).

**Impact.** One in-memory cache serves over half its requests cold (prewarm-key drift or a real load spike on an un-prewarmed surface). Cold requests pay the 5-10 s underlying SQL scan, so the surface using that cache is slow and `http_request_duration_seconds` p95 climbs (see [stellarindex_api_latency_p95_high](#stellarindex_api_latency_p95_high)).

**Diagnose** (at most 10 min).

1. Which cache + op: labels `{cache, op}`. The seven `cache` values (grep `APICacheOpsTotal.WithLabelValues` under `internal/api/v1/`): `coins`, `issuers`, `markets`, `network_stats`, `observations`, `oracle`, `sources_stats`. `markets` ops:
   - `distinct_pairs`: `/v1/markets` (no source filter)
   - `source_markets`: `/v1/markets?source=<x>`
   - `asset_markets`: `/v1/markets?asset=<x>`
   - `all_pools`: `/v1/pools`
2. Prewarm code: `cmd/stellarindex-api/main.go`, `prewarmCaches` (around main.go:5235). Two tiers: `prewarmHeavy` (`sources_stats` family, 5-min cadence) and `prewarmLight` (markets/pools/coins/native, 60 s). Find the call for the alerted op and compare every argument with what the handler in `internal/api/v1/markets.go` passes.
3. Diff the cache keys: key is a `fmt.Sprintf` of the args (`internal/api/v1/markets_cache.go` `fetchPairs` / `fetchPools`). Prewarm `Order=0` vs handler `Order=1` yields different keys. Known drift dimensions so far: Order, Sources (unfiltered `/v1/pools` prewarm), Limit; same family.
4. TTL vs cadence: `v1.NewCachedMarketsReader(...)` uses `2*time.Minute`; heavy tier runs every 5 min, light every 60 s. If a tier's cadence exceeds its caches' TTL the cache expires before refresh and looks like a miss storm.
5. Rule out key churn before blaming prewarm: `observations` and `oracle` are size-capped (`historyCacheMaxEntries` / `oracleCacheMaxEntries`, both 4096) because their key is caller-controlled on a public unauthenticated route. If `rate(stellarindex_api_cache_ops_total{cache="<alerted>",result="evicted"}[5m])` is also non-zero the map is at capacity and the miss rate is churn, not drift: either the working set exceeds the cap (raise it) or a caller is walking the asset/pair key space (rate-limit it). No prewarm change helps.

**Fix.**

1. Fix the drifted dimension in the prewarm. If prewarm and handler share a derivation function (e.g. `v1.DexSourceNames()`), use it instead of recomputing: one source of truth.
2. Cut a release and deploy (observability fixes do not backport; ship a binary with the fixed prewarm AND the metric).
3. Verify: miss ratio `< 0.1` within one cycle: at most 60 s post-deploy on r1 for the LIGHT tier; allow up to 5 min for `sources_stats` (heavy tier).

**False positives.**

- Cold start: cache empty for the first prewarm cycle after a restart; `for: 10m` covers it but a long boot delay can trip it.
- TTL > prewarm cadence: if someone bumps the TTL without the cadence, the alert is legitimate but the fix is the cadence, not the prewarm code.
- Ops the prewarm does not cover: `asset_markets` is un-prewarmed (cached on first user request, then served from cache); high miss there is expected. `source_markets` is prewarmed per registered CEX (`v1.CexSourceNames()`, limit=200, volume-desc: the `/exchanges/{name}` shape); `all_pools` is prewarmed per-limit `{5,25,100,200}` on the DEX-source filter plus one pass per DEX (`soroswap`/`phoenix`/`aquarius`/`sdex`/`comet`, limit=100). Suppress with a per-op exception or extend the prewarm.

**Related.** [stellarindex_api_latency_p95_high](#stellarindex_api_latency_p95_high); metric `stellarindex_api_cache_ops_total` in [docs/reference/metrics/README.md](../../reference/metrics/README.md).

## stellarindex_api_cache_refresh_failing

**Trigger.** `sum by (cache, op) (increase(stellarindex_api_cache_ops_total{result="refresh_error"}[15m])) > 0`, `for: 30m`, `severity: ticket` (failures persist in every 15 min window for 30 min). `result="refresh_error"` counts failed background stale-while-revalidate refreshes; the cache keeps serving the last good value with `flags.stale`, so only this alert shows it to operators.

**Diagnose.** `{cache, op}` names the wrapper (cache list in [stellarindex_api_cache_miss_rate_high](#stellarindex_api_cache_miss_rate_high)). Grep the API log for that op's refresh error:

```sh
journalctl -u stellarindex-api --since -1h | grep -i refresh
```

**Fix.** The cause is the upstream read (Postgres/ClickHouse timeout, missing relation after a migration, statement cancel). Fix that, not the cache; the next successful refresh clears the alert.

## stellarindex_api_price_stale

**Trigger.** `stellarindex_price_staleness_seconds{asset,quote} > 120` sustained 5 min (`for: 5m`), OR the series ABSENT for 10 min (`absent_over_time(stellarindex_price_staleness_seconds[10m])`); in the absent case the alert has no `asset` label (aggregator tick wedged, gauge no longer emitted): go straight to `aggregator.md#stellarindex_aggregator_silent`. P2 (ticket). Rule: `configs/prometheus/rules.r1/api.yml` (source of truth for r1); mirror in `deploy/monitoring/rules/api.yml`. MTTR 15-60 min.

`quote` is the configured quote that stopped publishing. `asset` is one of the aggregator's configured pair bases (`native`, `crypto:XLM`, `crypto:BTC`, `crypto:ETH`; the `defaultPairs` list in `cmd/stellarindex-aggregator/main.go`), not the `CODE-ISSUER` form passed to `/v1/price`. `native` and `crypto:XLM` always report the same value (gauge emits MIN over both forms).

**Impact.** `/v1/price?asset=<X>` returns 200 with a real but old price (`observed_at` well behind wall-clock). Envelope `stale=true` is set when the fallback chain (last-trade / stablecoin proxy / triangulation) served the answer; the gauge captures staleness even on the happy path.

**Diagnose** (at most 5 min).

```sh
# Which branch fired? No `asset` label => absent branch => aggregator.md#stellarindex_aggregator_silent
curl -s http://localhost:9090/api/v1/alerts |
  jq '.data.alerts[] | select(.labels.alertname=="stellarindex_api_price_stale") | {labels, value, activeAt}'

# Which asset and quote are stale? (aggregator metrics endpoint, 127.0.0.1:9465)
curl -s http://127.0.0.1:9465/metrics |
  awk '/^stellarindex_price_staleness_seconds/ && $2 > 120 {print}'

# Is it one asset or many?
#   One asset → that asset's source is stopped / paused, or the pair
#     isn't clearing the VWAP publication gate (root cause 3).
#   Many assets on one source → the source is stopped.
#   Many sources → the aggregator isn't writing (or isn't running).
#   Note: `native` and `crypto:XLM` always report the SAME value
#   (the gauge emits MIN over both forms for both labels).

# Which sources quote this asset? (on-chain XLM rows are stored as
# `native`, CEX rows as `crypto:XLM`; the dual-form alias
# accepts both at the API.)
psql -d stellarindex -c "SELECT source, max(ts) AS most_recent
         FROM trades WHERE base_asset IN ('native', 'crypto:XLM')
                        OR quote_asset IN ('native', 'crypto:XLM')
         GROUP BY source ORDER BY most_recent DESC;"

# Is the aggregator binary running, ticking, and writing VWAPs?
ssh root@<host> "systemctl status stellarindex-aggregator --no-pager | head -10"
ssh root@<host> "curl -s http://127.0.0.1:9465/metrics | grep -E '^stellarindex_aggregator_(vwap_writes_total|ticks_total|empty_windows_total)'"
ssh root@<host> "journalctl -u stellarindex-aggregator -n 200 --output=cat | grep -i 'tick\|error' | tail -30"
```

**Typical causes.**

1. Source quoting the asset is stopped: no new trade, aggregator has nothing fresh, API serves the last trade with aging `observed_at`. Signal: `stellarindex_source_last_event_unix{source=<X>}` frozen; `stellarindex_ingestion_source_stopped` may fire (it uses `for: 15m`, so it can lag this alert). Also compare `stellarindex_source_last_insert_unix` (both on the indexer at `127.0.0.1:9464`): events advancing while inserts are frozen is the stuck-cursor / duplicate-flood signature, see `cursor-stuck.md`, `ingestion-duplicate-flood.md` and the `stellarindex_serving_insert_frozen` alert. Fix: `source-stopped.md`.
2. Aggregator running but not writing CAGGs / hot cache (CAGG refresh jobs failing: schedule misfire, SQL error in the window function). Signal: `stellarindex_timescale_cagg_stale`. Fix: `cagg-stale.md`.
3. Aggregator running but the pair has had no VWAP write for over 120 s. The gauge is emitted at the end of every tick for every configured pair (`internal/aggregate/orchestrator/orchestrator.go` `emitStalenessGauges`, reset on each VWAP cache write); not request-driven, no `change()` in the rule, so a reading > 120 s means the pair genuinely was not published. Causes: pair not clearing the `min_usd_volume` publication gate (`$10k`/window in `/etc/stellarindex.toml`), empty windows, outlier filtering dropping everything, anomaly freeze engaged. Signal: `stellarindex_aggregator_empty_windows_total` climbing; freeze alerts (`anomaly.md#stellarindex_anomaly_freeze_engaged`); `fx-feed-stale.md` for fiat legs.
4. Pair that no longer trades on-chain (long-tail asset, last trade days ago): data reality, not a bug. Only configured pair bases carry this gauge, so long-tail classic assets can never fire this alert; their staleness shows via the API `stale` flag and the sla-probe / served-value-drift alerts. Consider de-listing or flagging `stale=true`.
5. Binary version skew: aggregator and API/indexer on different builds after a partial deploy (`stellarindex_binary_version_skew`). Signal: `-version` of `/usr/local/bin/stellarindex-aggregator` differs from `/usr/local/bin/stellarindex-api`. Fix: `binary-version-skew.md`.

**Fix.**

1. No `asset` label (absent branch): `aggregator.md#stellarindex_aggregator_silent`.
2. Confirm which sources quote the asset and which stopped (diagnosis above).
3. One source dead: `source-stopped.md`. Aggregation pipeline problem: `cagg-stale.md`.
4. Genuinely no on-chain activity: decide with product whether to de-list or keep the stale number with `stale=true`.
5. Verify: `stellarindex_price_staleness_seconds{asset=<X>,quote=<Q>}` back under 120 s and the alert clears (`for: 5m` lets you confirm it is not a flap).

**Root cause evidence.** Which assets were stale (pattern: one issuer, one source's pairs?); aggregator logs for the window (errors, refresh lag); orchestrator source `health()` reports (soft errors / decode-errors up while still running?).

**False positives.**

- Aggregator restart: `lastWriteAt` resets so every pair reports about 0 then climbs; a newly configured pair is stamped "just observed" on first sighting. If no VWAP write lands within 2 min of restart, the alert is real.
- Chain halt: if Stellar mainnet stops producing ledgers every asset goes stale at once; correlates with `core-lag.md` / `rpc-lag.md`, which are the real alerts.

**Related.** `aggregator.md#stellarindex_aggregator_silent` (absent branch); `source-stopped.md`; `cagg-stale.md`; `oracle-stale.md`; `sla-probe-freshness-breach.md` (customer-facing freshness: `/v1/price/tip` > 30 s, other endpoints > 180 s); `data-source-stale.md`, `served-value-drift.md`; `binary-version-skew.md`; HA plan section 9: `docs/architecture/ha-plan.md`.

## stellarindex_api_price_stream_not_delivering

**Trigger.** `sum(rate(stellarindex_aggregator_stream_publish_total{outcome="ok"}[15m])) > 0 unless on() sum(rate(stellarindex_api_stream_subscribe_total{outcome="ok"}[15m])) > 0`, `for: 15m`, P3 (`severity: ticket`). Rule: `configs/prometheus/rules.r1/api.yml`; multi-host twin `deploy/monitoring/rules/api.yml`. Aggregator publishes while the API's subscribe `ok` counter is flat or absent. MTTR 15-60 min. Impact: `/v1/price/stream` answers 200 and sends keepalives but no `price_update` events.

**Diagnose** (at most 5 min).

```sh
curl -s http://localhost:9090/api/v1/query --data-urlencode \
  'query=sum by (outcome) (increase(stellarindex_api_stream_subscribe_total[15m]))' | \
  jq -r '.data.result[] | "\(.metric.outcome): \(.value[1])"'
```

- `future_observed_at` rising: the API host clock lags the aggregator's by more than 5 min and every event is rejected as future-dated. Check `timedatectl` / chrony on both hosts.
- `stale_observed_at` rising: events older than 24 h (a replay, or the aggregator clock far behind).
- `malformed` / `decode_error` rising: wire-format drift between aggregator publisher and API subscriber (version skew).
- Nothing rising: the API receives no messages. go-redis retries a dead pubsub connection internally with no app-level log; check `redis-cli PUBSUB NUMSUB stellarindex:closed-bucket:v1` and the API stderr for go-redis reconnect lines.

**Fix** (at most 15 min).

1. Clock skew: resync NTP on the lagging host.
2. No messages: confirm Redis is reachable from the API host, then restart the API to force a fresh SUBSCRIBE.
3. Verify `outcome="ok"` rising again for 15 min.

**False positive.** API restart in the window resets the counter; `rate()` handles it and one scrape gap does not hold for 15 min.

**Related.** `aggregator.md#stellarindex_aggregator_silent` (aggregator not publishing at all; this alert cannot fire then); [stellarindex_api_error_rate_critical](#stellarindex_api_error_rate_critical) (general API triage).

## stellarindex_auth_reaper_stalled

**Trigger.** `time() - stellarindex_auth_reaper_last_sweep_unix{reaper}` exceeds `3 * stellarindex_auth_reaper_interval_seconds{reaper}` for 15 min (also fires if the interval series exists with no last-sweep series). Ticket (P3): table growth is slow, hours not minutes. Component: `api` (reapers run inside `stellarindex-api`). First action: `journalctl -u stellarindex-api --since -6h | grep -iE "reaper|panic"`.

**Meaning.** Background reapers bound tables an unauthenticated caller can grow: `login_code_lockouts` (keyed by attacker-chosen email; [login_code_lockout_table_growing](api.md#stellarindex_login_code_lockout_table_growing)), `magic_link_tokens` ([stellarindex_magic_link_token_table_growing](#stellarindex_magic_link_token_table_growing); gauge [`stellarindex_magic_link_token_rows`](../../reference/metrics/README.md#stellarindex_magic_link_token_rows)) and speculative-account orphans (`internal/signupreaper`). A dead reaper freezes its rows gauge and errors counter at the last healthy values, so the `_table_growing` alerts (rows > 10000) fire only after the table has filled; this alert is the liveness signal. `stellarindex_auth_reaper_last_sweep_unix{reaper}` is stamped at the end of every COMPLETED sweep, including failed ones (the errors counter covers failures); the ctx-cancelled early return does not stamp it. `stellarindex_auth_reaper_interval_seconds{reaper}` is the configured cadence, so the threshold follows the deployment's interval.

**Blast radius.** One of `login_code_lockouts` / `magic_link_tokens` / speculative-account orphans / ended `sessions` / finished `webhook_deliveries` is no longer bounded; its rows gauge (where it has one) is frozen, not healthy.

**Symptoms.**

- One `reaper` label: `login_code`, `magic_link`, `signup`, `session`, `webhook_delivery`. The last two are `internal/retentionreaper` sweeps of ended `sessions` (90-day retention) and finished `webhook_deliveries` (30-day retention); no rows gauge, check `stellarindex_retention_reaper_errors_total{reaper}`.
- The matching rows gauge (`stellarindex_login_code_lockout_rows`, `stellarindex_magic_link_token_rows`) is perfectly flat since the last sweep timestamp; flat is the symptom, not reassurance.
- `stellarindex_worker_panics_total{worker}` may have incremented at the same moment if the reaper died by panic.

**Diagnose** (at most 5 min).

1. Last sweep and cadence:

   ```promql
   stellarindex_auth_reaper_last_sweep_unix
   stellarindex_auth_reaper_interval_seconds
   ```

2. Dead or stuck goroutine:

   ```sh
   journalctl -u stellarindex-api --since -6h | grep -iE "reaper|panic|recovered"
   ```

   A `worker panicked` line names the reaper; nothing at all means the goroutine is blocked (almost always a Postgres call: check `pg_stat_activity` for a long-running `DELETE FROM login_code_lockouts` / `magic_link_tokens` and what it waits on).
3. Disabled? A reaper off in config publishes NO series and cannot fire this alert; if the series exists, it was constructed and running at some point.

**Fix** (at most 15 min).

- Dead goroutine does not return on its own: `systemctl restart stellarindex-api`. Reapers sweep immediately on start, which clears the alert; no customer impact beyond the restart.
- Stuck Postgres call: find and terminate the blocking backend (`pg_terminate_backend`) before restarting, or the restart blocks on the same lock.
- Table already large: one manual sweep is safe; reapers only delete settled/expired rows (see each reaper's retention doc comment), never live lockouts or unexpired links.

**Root cause.** Panic: recovered stack is in the API log next to the `stellarindex_worker_panics_total` increment; file against the reaper package (fix the bug, not the restart). Hang: a lock held by a long transaction (bulk restore, migration); reapers use no explicit lock timeout (known gap). Never started: wiring regression in `cmd/stellarindex-api/main.go`; the guard test there checks reaper goroutines are supervised.

**False positives.** Right after a deploy with a NEW interval (gauge carries the old cadence until the next restart stamps the new one): self-clears within one sweep. Prometheus down or API scrape failing: `time() - gauge` keeps growing on stale samples; check `up{job="stellarindex-api"}` first.

**Related.** [login_code_lockout_table_growing](api.md#stellarindex_login_code_lockout_table_growing); [worker-panicked](worker-panicked.md); [`stellarindex_auth_reaper_last_sweep_unix`](../../reference/metrics/README.md#stellarindex_auth_reaper_last_sweep_unix).

## stellarindex_magic_link_token_table_growing

**Trigger.** `stellarindex_magic_link_token_rows > 10000` for 30 min, ticket. The sweep is alive but outpaced, or failing. This alert and [stellarindex_auth_reaper_stalled](#stellarindex_auth_reaper_stalled) are the only signals for `magic_link_tokens`.

**Diagnose, in order.**

1. `rate(stellarindex_magic_link_token_rows_deleted_total[1h])` against the gauge's growth: deletes near zero = sweep failing (read the API log for `magiclinkreaper`); deletes high = a flood of mints.
2. A flood: `sum(rate(http_requests_total{route="/v1/auth/login"}[15m]))` and the per-IP throttle counters; block the source at the edge.
3. If the sweep is dead, `stellarindex_auth_reaper_stalled` fires too; fix that first.

## stellarindex_ch_schema_probe_absent

**Trigger.** `stellarindex_ch_schema_probe_present{probe="..."} == 0` for 30 min on one API instance, P3 (ticket). Rules: `deploy/monitoring/rules/api.yml` and r1 overlay `configs/prometheus/rules.r1/api.yml`. MTTR 5-30 min.

**Meaning.** The explorer reader (`internal/storage/clickhouse/explorer_reader.go`) asks ClickHouse whether each optional lake object exists before using it. A "no such table/column" answer is kept until the process restarts, so an API that started before the lake DDL was applied stays on the fallback path indefinitely. Impact: one explorer surface is served from its slow fallback (full-table or bloom-index scans) or 503s; slower or unavailable, not wrong.

The gauge moves only on an answer from the lake: `0` = object absent, or (row-requiring probe) present but empty; `1` = present and usable. A lake that does not answer at all does not move it; that shows as rising `stellarindex_ch_schema_probe_unanswered_total` and usually `stellarindex_clickhouse_server_down`.

| `probe` | Lake object |
| ------- | ----------- |
| `tx_hash_index` | `stellar.tx_hash_index` |
| `tx_hash_index_coverage` | `stellar.tx_hash_index_coverage` (marker; apply `deploy/clickhouse/tx_hash_index_coverage.sql`, then backfill or insert the row) |
| `contract_active_ledgers` | `stellar.contract_active_ledgers` |
| `contract_instance_changes` | `stellar.contract_instance_changes` |
| `contract_instance_changes_tx_key` | `tx_hash` + `intra_ledger_seq` on `stellar.contract_instance_changes` |
| `contracts_census_daily` | `stellar.contracts_census_daily` |
| `accounts_stats` | `stellar.accounts_stats` |
| `account_creators_rollup` | `stellar.account_creators_rollup` |
| `account_sponsors_rollup` | `stellar.account_sponsors_rollup` |
| `account_creator_edges` | `stellar.account_creator_edges` |
| `account_sponsor_edges` | `stellar.account_sponsor_edges` |
| `asset_holders_rollup` | `stellar.asset_holders_rollup` |
| `ops_by_source` | `stellar.ops_by_source` |
| `account_activity` | `stellar.account_activity` |
| `ledger_entries_current_version` | `version` column on `stellar.ledger_entries_current` |

**Diagnose** (at most 5 min).

```sh
# 1. Which probes read 0 on this API?
ssh r1 'curl -s http://localhost:3000/metrics | grep ^stellarindex_ch_schema_probe'

# 2. Does the object exist, and does it hold rows? (substitute the table)
clickhouse-client --port 9300 -q "EXISTS TABLE stellar.tx_hash_index"
clickhouse-client --port 9300 -q "SELECT count() FROM stellar.tx_hash_index"

# 3. For a column probe, is the column there?
clickhouse-client --port 9300 -q "SELECT name FROM system.columns
  WHERE database = 'stellar' AND table = 'ledger_entries_current' AND name = 'version'"
```

- Object missing: DDL not applied (see `deploy/clickhouse/`); fix A.
- Present and populated, gauge still `0`: verdict latched before the DDL landed; fix B.
- Present but empty: backfill not run or table truncated; fix C.

**Fix** (at most 15 min).

- **A. Object missing.** Apply its DDL from `deploy/clickhouse/` (the file named after the table, or the one adding the column), then do B: a process that answered "absent" earlier does not pick the object up by itself.
- **B. Verdict latched.** `ssh r1 'sudo systemctl restart stellarindex-api'`. Verify the gauge reads `1` after the first request that exercises that surface and the alert clears.
- **C. Object empty.** Run the backfill documented beside the object's DDL. A row-requiring probe re-asks every few seconds under traffic, so no restart is needed once rows exist.

**False positives.** No traffic since the object was repopulated: probes run only when a request needs them, so the gauge keeps the last answer until one request to the affected route settles it. A deployment that deliberately omits an optional object: fallback is correct; the alert is a standing ticket until built, so silence it with an expiry, not indefinitely.

**Related.** [clickhouse-server-health](clickhouse-server-health.md) (lake down or failing queries); `docs/reference/metrics/README.md` for `stellarindex_ch_schema_probe_present` and `stellarindex_ch_schema_probe_unanswered_total`.

## stellarindex_customer_webhook_delivery_failing

**Severity** P3 (ticket). Rules: `configs/prometheus/rules.r1/api.yml` (r1 overlay) and the multi-host twin `deploy/monitoring/rules/api.yml`. MTTR 5-30 min for a single-customer outage; longer if the worker is the problem.

**Impact** One or more customers are not receiving the webhook callbacks they registered for, including SEV-1 incident pings. The API itself is unaffected.

**Trigger** `sum(rate(stellarindex_customer_webhook_delivery_attempts_total{outcome=~"server_error|network_error"}[5m])) > 0.05` for 15m. One customer endpoint is sustained-down (5xx, TCP/TLS errors) and we retry with exponential backoff (30s to 1h cap, 15-attempt budget; the last retry lands ~4–8 h after the first failure, depending on jitter).

**Diagnose**

```sh
# Which webhook is failing? Group recent failures by webhook_id.
ssh r1 'sudo -u postgres psql stellarindex -c "
  SELECT webhook_id,
         COUNT(*)                              AS attempts,
         MAX(last_response_status)             AS worst_status,
         MAX(last_error)                       AS sample_error,
         BOOL_OR(delivered_at IS NOT NULL)     AS any_delivered_recently
    FROM webhook_deliveries
   WHERE created_at > now() - interval '\''30 min'\''
     AND (last_response_status >= 500 OR last_error LIKE '\''POST%'\'')
   GROUP BY webhook_id
   ORDER BY attempts DESC
   LIMIT 10;
"'

# Look up the customer + URL for the top offender:
ssh r1 'sudo -u postgres psql stellarindex -c "
  SELECT w.id, w.account_id, a.name, a.billing_email, w.url, w.enabled
    FROM customer_webhooks w JOIN accounts a ON a.id = w.account_id
   WHERE w.id = '\''<webhook_id from above>'\'';
"'

# Confirm the worker itself is healthy (other webhooks succeeding):
ssh r1 'curl -s http://localhost:3000/metrics | grep stellarindex_customer_webhook_delivery_attempts_total | head -10'

# Latency posture (per-outcome p95/p99): `delivered` p99 climbing without
# the failing alert means a customer endpoint is going slow, not failing.
ssh r1 'curl -s http://localhost:3000/metrics | grep stellarindex_customer_webhook_delivery_duration_seconds | head -20'
```

Decision:

- One webhook accounts for all failures and other webhooks deliver normally: single-customer outage; contact the account.
- One webhook failing and zero `delivered` outcomes anywhere: the worker may be the problem; `journalctl -u stellarindex-api -g "customer-webhook"`.
- Many webhooks failing: investigate the worker and R1 egress; try a sample URL from the box (`curl -X POST https://example.com/hook -d '{}'`).

**Fix**

1. Capture webhook ID, account ID, `billing_email`, URL and the latest `last_error` from the SQL above.
2. Single customer: contact them via `billing_email` or dashboard owner. Template: "our webhook delivery worker has been unable to reach `<URL>` since `<timestamp>` (HTTP `<status>`). After our 15-attempt retry budget (~8h) we mark the delivery permanently failed; missed events include SEV-1 incident pings. Updating the URL via `PATCH /v1/dashboard/webhooks/{id}` re-arms delivery."
3. Worker itself failing (zero `outcome="delivered"` over the window):

```sh
# Inspect logs for the underlying error (timeout / TLS / DNS).
ssh r1 'journalctl -u stellarindex-api --since "30 min ago" \
  | grep -E "customer-webhook|delivery (delivered|failed)"'
# Restart only as a last resort — the worker is in-process
# with the API; restart paths cycle every cached state.
ssh r1 'systemctl restart stellarindex-api'
```

**Seal key (`STELLARINDEX_WEBHOOK_SEAL_KEY`)** Signing keys (current and rotation-previous) are sealed at rest under `vault_webhook_seal_key` (migration 0204). An unset key on the API logs a startup warning and every delivery to a sealed webhook counts `outcome="lookup_error"` and waits; set the vault value and redeploy to drain them. A changed key is terminal (`no_secret`) for every sealed webhook: there is no in-place seal-key rotation. To rotate, restore the old value, or accept that customers must recreate their webhooks (edit and delete work without the key).

**Related** `internal/customerwebhook/worker.go`; [anomaly-freeze-engaged](anomaly.md#stellarindex_anomaly_freeze_engaged) (the upstream event that fires SEV-1 and then the customer webhook); [platform-spec](../../architecture/platform-spec.md) (this runbook covers OUTBOUND deliveries, us to customer; the Stripe billing bridge is the INBOUND surface, and a degraded bridge can leave dashboards stale while this worker is green).

## stellarindex_customer_webhook_delivery_exhausted

**Severity** P3 (ticket).

**Trigger** `sum(rate(stellarindex_customer_webhook_delivery_attempts_total{outcome="exhausted"}[1h])) > 0`, `for: 0m`. A delivery hit the retry budget and was marked terminally failed: the customer did NOT receive the event at all (for a SEV-1 notification they may not know an incident was declared).

**Diagnose** Same queries as [stellarindex_customer_webhook_delivery_failing](#stellarindex_customer_webhook_delivery_failing) (identify the webhook, account, URL, `last_error`).

**Fix**

- The row stays in `webhook_deliveries` for forensics; the customer sees the failed attempt in their dashboard.
- No retry is automatic; the operator decides whether to re-enqueue manually.
- Cleanest path: ask the customer to `PATCH` the webhook URL (if broken) or trigger a fresh event from their side.

## stellarindex_customer_webhook_mark_errors

**Severity** P3 (ticket). Read this one first when it fires alongside the other two.

**Trigger** `increase(stellarindex_customer_webhook_delivery_attempts_total{outcome="mark_error"}[30m]) > 0` for 15m. The POST reached a decision (2xx/4xx/5xx/network fault) but the store write recording it failed. `MarkDelivered` / `MarkAttemptFailed` is the only write that advances `attempt_count`, so the row keeps the 5-minute claim lease set by `ListPendingDeliveries` and the SAME payload is re-POSTed to the customer on every lease, indefinitely, with no retry budget to end it. The customer sees duplicate deliveries; we see a delivery that never completes.

The counter advances at three sites in `internal/customerwebhook/worker.go` (`classifyResponse`, `handleFailure`, `markTerminal`); each logs a WARN naming the `delivery_id`.

**Diagnose**

```sh
# Which deliveries are wedged? A high attempt-lease churn with a
# STATIC attempt_count is the signature.
ssh r1 'sudo -u postgres psql stellarindex -c "
  SELECT id, webhook_id, event_type, attempt_count, next_attempt_at, created_at
  FROM webhook_deliveries
  WHERE next_attempt_at IS NOT NULL AND next_attempt_at < now() + interval '"'"'10 min'"'"'
  ORDER BY next_attempt_at DESC LIMIT 20"'

# The worker's own account of it.
ssh r1 'journalctl -u stellarindex-api --since -2h | grep -i "customer-webhook.*Mark"'
```

**Fix** Almost always Postgres: unreachable, in recovery, or the statement is being cancelled. Restore the database; the next lease writes the outcome and the loop ends by itself.

The attempt's own HTTP deadline is NOT a cause: the outcome write shares neither it nor the worker's shutdown signal; `Worker.mark` writes on `context.WithoutCancel(ctx)` bounded by its own `markWriteTimeout` (1s), so a POST that uses the whole attempt budget still gets the full 1s. If Postgres is healthy and the counter still advances, the single-row UPDATE is not landing inside that second: look for lock contention or a slow statement on `webhook_deliveries` (`pg_stat_activity`, `pg_locks`). Such a row keeps its claim lease and is retried at lease expiry with a fresh budget.

## stellarindex_dex_tvl_refresh_failing

**Severity** ticket. Rules: `deploy/monitoring/rules/api.yml` and `configs/prometheus/rules.r1/api.yml`. MTTR 5-20 min (almost always ClickHouse or served-tier reachability, shared with louder alerts).

**Impact** `/v1/protocols`, `/v1/protocols/{name}/tvl` and per-protocol pages keep serving TVL, but as a carried-forward snapshot growing stale behind a healthy-looking page: no 5xx, no flag beyond the snapshot's own `as_of`. A protocol whose read has failed on every cycle since API start has nothing to carry: it is absent, named in `tvl_total.excluded` (which sets `lower_bound`), and its `/tvl` drill-down 404 says the read failed.

**Trigger** `sum(rate(stellarindex_dex_tvl_refresh_total{outcome="error"}[15m])) > 0 and sum(rate(stellarindex_dex_tvl_refresh_total{outcome="ok"}[15m])) == 0` for 30m (it requires zero `ok` outcomes, so it cannot see a single-protocol failure). Symptoms: `outcome="error"` rising with `outcome="ok"` flat; `journalctl -u stellarindex-api | grep "dex tvl"` shows the joined per-protocol error every ~10 min (`soroswap tvl: ...`, `phoenix tvl: ...`); explorer DEX pages show a TVL whose `as_of` (DEXTVLCache snapshot time) stopped advancing.

**Diagnose**

```sh
# Which protocol is failing? The refresh error is per-protocol:
journalctl -u stellarindex-api --since -30min | grep "dex tvl cache refresh" | tail -3
# Lake-side reads (soroswap/phoenix/comet reserves) -> ClickHouse:
clickhouse-client --port 9300 -q "SELECT 1"
# Served-tier reads (aquarius reserve snapshots, prices_1m USD legs) -> Postgres:
sudo -u postgres psql stellarindex -c "SELECT max(bucket) FROM prices_1m"
```

The refresh is one lake reserve lookup per protocol plus a bounded set of `prices_1m` point reads under a 3-minute timeout; sustained failure has so far only meant backend reachability. If backends are healthy and it still errors, chart `stellarindex_dex_tvl_refresh_duration_seconds`: an `error` histogram pinned at 180 s means reads are timing out (the 40x read-amplification class: a scan lost its `max_threads`/`max_memory_usage` pin).

**Fix**

1. Self-healing: fix the failing backend and the next 10-min tick repopulates every protocol. No restart.
2. If only ONE protocol errors (the alert will not fire), it carries its previous entry forward; treat it as that protocol's reader breaking (e.g. a lake schema change) and file it rather than restarting.
3. Restarting `stellarindex-api` clears the snapshot; the TVL field is then OMITTED until the first successful refresh. Restart only if the process is wedged.

**False positive** A ClickHouse restart or heavy merge can fail one or two refreshes; the 30m `for:` absorbs it. If it fires during a known heavy one-shot on r1, verify the job finished and the next tick succeeded, then close.

**Related** [stellarindex_dex_tvl_total_divergent](#stellarindex_dex_tvl_total_divergent) (a carried-forward protocol is REFUSED admission to the headline `tvl_total`, so that alert catches single-protocol failure); [stellarindex_sdex_orderbook_maintain_failing](#stellarindex_sdex_orderbook_maintain_failing) (same binary, same lake dependency); `internal/api/v1/dex_tvl_cache.go`; metrics in `docs/reference/metrics/README.md`.

## stellarindex_dex_tvl_total_divergent

**Severity** ticket. Rules: `deploy/monitoring/rules/api.yml` and `configs/prometheus/rules.r1/api.yml`. MTTR 5-20 min (usually the same backend reachability as [stellarindex_dex_tvl_refresh_failing](#stellarindex_dex_tvl_refresh_failing)).

**Impact** The headline `tvl_total` on `/v1/protocols` (and the explorer's "Total value locked") UNDERSTATES: it excludes at least one protocol whose per-protocol figure is still published beside it; if nothing was admitted `tvl_total` is omitted and the explorer shows no headline. No 5xx, no wrong number: every refusal is named in `excluded[]`. Invisible otherwise: the refresh alert cannot fire while other protocols still refresh.

**Trigger** `sum(rate(stellarindex_dex_tvl_reconcile_total{outcome="divergent"}[30m])) > 0 and sum(rate(stellarindex_dex_tvl_reconcile_total{outcome="ok"}[30m])) == 0` for 1h (both children zero-seeded at start, so flat zero is a real reading).

`tvl_total` is the exact sum of the per-protocol `tvl_usd` strings on the same response (`internal/api/v1/dex_tvl_total.go`, `dexTVLAdmit`). A protocol is admitted only if its figure is not carried forward, its `as_of` is this snapshot's, its pool counts balance, and its decimal parses; otherwise it is dropped and named in `excluded[]`.

**Diagnose** The response names the protocol and the reason.

```sh
# Which protocol was refused, and under which rule?
curl -s localhost:3000/v1/protocols \
  | jq '.data.tvl_total | {protocols, lower_bound, as_of, excluded}'
# Compare against which rows DO carry a per-protocol figure: the
# difference is the refused set.
curl -s localhost:3000/v1/protocols \
  | jq -r '.data.protocols[] | select(.tvl) | .name'
# A carried-forward figure means that protocol's reserve read failed:
journalctl -u stellarindex-api --since -60min | grep "dex tvl" | tail -5
# The refused protocol's figure and pools are still served, labelled,
# on the drill-down — carried_forward: true (flags.stale too):
curl -s localhost:3000/v1/protocols/<name>/tvl \
  | jq '{carried: .data.carried_forward, tvl: .data.tvl, pools: (.data.pools | length)}'
```

`excluded[]` always holds STANDING scope entries (classic CAP-38 pools, SDEX order book, lending supplied-value, vault AUM): those are not refusals. A refusal is a protocol NAME with a reason such as "carried forward from an earlier refresh".

Almost every refusal is a failed reserve read (`DEXTVLCache.Refresh` keeps the previous entry), so it tracks `stellarindex_dex_tvl_refresh_total{outcome="error"}`. Two code-level defects are possible, pointing at `internal/api/v1/dex_tvl_cache.go` / `dex_tvl_total.go`: pool counts not balancing (`pools_priced + unpriced_pools != pools_total`, accumulator drift), or `tvl_usd` not parsing as a decimal (should be unreachable).

**Fix**

1. Fix the failing backend. Stateless and self-healing: the next 10-min refresh readmits the protocol. No restart.
2. If ClickHouse or the served tier is unhealthy expect [stellarindex_dex_tvl_refresh_failing](#stellarindex_dex_tvl_refresh_failing) or a louder backend alert; treat that as primary.
3. Verify: `stellarindex_dex_tvl_reconcile_total{outcome="ok"}` increments within 10 min and `tvl_total.protocols` lists every protocol whose row has a `tvl` object.
4. Do NOT loosen admission: a total that absorbs a carried-forward figure is a wrong number under a fresh `as_of`.

**False positive** A merge window or dependency restart can refuse one or two cycles; the 30m window plus 1h `for:` absorbs it. Right after an API restart no reconciliation has run, both children read zero and the alert cannot fire (`tvl_total` is absent, not wrong).

**Related** Methodology: `docs/methodology/dex-tvl.md`; metric `stellarindex_dex_tvl_reconcile_total` in `docs/reference/metrics/README.md`.

## stellarindex_directory_sync_unflagged

**Severity** P3 (ticket). Rules: `deploy/monitoring/rules/api.yml` and `configs/prometheus/rules.r1/api.yml`. MTTR 15-60 min (review the upstream change behind each cleared tag); clears on its own when the next daily sync commits with no un-flags.

**Impact** Trust. Each un-flagged issuer's price is no longer withheld by the scam-pricing gate and is served on every gated surface.

**Trigger** `max by (instance, source) (stellarindex_directory_sync_rows_changed{kind="unflagged"}) > 0`. `stellarindex-ops directory-sync` mirrors the stellar-expert public directory into `account_directory`; a scam-class tag makes `internal/pricingguard.ScamGate` withhold the issuer's price. A committed sync that removes such a tag or prunes a flagged row counts an un-flag and writes it to the node_exporter textfile `directory_sync.prom`. The gauge holds the last committed run's count, so the alert stays until the next daily run commits with none; a failed or refused run writes nothing. One run may clear at most `max(10, ceil(5 % of the addresses flagged before the run))` tags; a larger snapshot is refused with `ErrDirectoryChurnExceeded` unless run with `-accept-churn`.

**Diagnose**

1. What did the run do? `journalctl -u directory-sync | grep 'Synced:'` (upserted, pruned, newly-flagged, un-flagged counts).
2. Which addresses? Compare the upstream repository's history for the run's day with the tags expected. A tag removed in a reviewed upstream commit is a genuine correction.
3. Was the run pinned? `grep EXTRA_FLAGS /etc/default/directory-sync`; with no `-sha256` the run synced whatever the upstream branch held at fetch time.

**Fix**

- Genuine upstream correction: no action; clears after the next daily run.
- Not genuine: pin to the last good upstream commit. Set `EXTRA_FLAGS=-url https://github.com/stellar-expert/public-directory/archive/<commit>.tar.gz -sha256 <hex digest of that tarball>` in `/etc/default/directory-sync`, then `systemctl start directory-sync`. The pinned snapshot restores the tags; the churn ceiling still applies.
- Do NOT re-run with `-accept-churn` to get past a refusal unless the upstream mass change is confirmed genuine (it removes the per-run ceiling on prunes, new flags and un-flags). Do NOT silence this alert: it is the only signal that a flagged issuer is priced again.

**Related** [stellarindex_scam_gate_fail_open](#stellarindex_scam_gate_fail_open); [systemd-unit-failed](systemd-unit-failed.md) (a failed or refused `directory-sync` run).

## stellarindex_failed_auth_rate_high

**Severity** P3 (ticket). Rules: `deploy/monitoring/rules/api.yml` and `configs/prometheus/rules.r1/api.yml`. MTTR 15-60 min to identify the sources.

**Impact** Someone may be guessing credentials. No request got through: the Auth middleware rejected all of them.

**Trigger** `sum(rate(stellarindex_failed_auth_total{outcome="rejected"}[5m])) > 1` for 15m. The failed-auth throttle caps failures per client IP and per presented API-key prefix at `api.failed_auth_rate_limit_per_min` (default 20/min); guessing spread over enough IPs and prefixes stays under every cap and never gets a 429. One source contributes at most 20/min, so a sustained 1/s means more than three sources at cap or many under it.

**Diagnose**

```promql
sum by (outcome) (rate(stellarindex_failed_auth_total[5m]))
```

```sh
journalctl -u stellarindex-api --since '30 min ago' --no-pager -o cat \
  | jq -r 'select(.status == 401 or .status == 403) | .remote_ip' \
  | sort | uniq -c | sort -rn | head -20
```

- A few IPs each near 20/min: misconfigured clients (revoked or rotated key still deployed) or a small guessing run; already capped.
- Many IPs each well under 20/min: distributed guessing, the case this alert exists for.

**Fix**

- Few IPs belonging to a known customer: tell them about the stale key; no platform action.
- Distributed guessing: block the source ranges at the edge proxy. API keys carry a 224-bit secret after the display prefix, so guessing does not threaten a key directly; the cost is validator load.
- Verify: rejected rate below 1/s; the alert clears 15 min later.
- Do NOT set `api.failed_auth_rate_limit_per_min` to zero (disables the throttle). A burst of `throttled` alone is not this alert (excluded from the expression).

**Related** [stellarindex_ratelimit_fail_closed](#stellarindex_ratelimit_fail_closed): the failed-auth throttle shares the rate limiter's Redis; in a sustained Redis outage it fails closed and the `throttled` series climbs for every failed request.

## stellarindex_login_code_lockout_table_growing

**Severity** P3 (ticket). Rules: `deploy/monitoring/rules/api.yml` and `configs/prometheus/rules.r1/api.yml`. MTTR minutes: repair the sweep or apply the lagging migration.

**Impact** Arm 1 (rows): a remote unauthenticated caller is filling a table on a disk-fixed host. Arm 2 (`status_check`): the brute-force bound on the 6-digit sign-in code is silently not enforced.

**Trigger** `login_code_lockouts` (migration 0122) is the durable per-email failed-verify counter bounding guesses of the dashboard's 6-digit email code. Its primary key is attacker-chosen: unauthenticated `POST /v1/auth/verify-code {"email":"<random>@example.com","code":"000000"}` inserts a row for an address nobody owns, and only a successful sign-in for that exact address deletes one. `internal/logincodereaper` (hourly, 48 h retention, live locks exempt) bounds the table. Rule (`for: 30m`), two arms:

- rows: `stellarindex_login_code_lockout_rows > 10000` (healthy is low tens): sweep not keeping up, not running, or a fill faster than retention drains.
- fail-open: `increase(stellarindex_login_code_lockout_errors_total{op="status_check"}[1h]) > 0`: the pre-match lockout read errored and the handler failed open; the response looked normal.

**Diagnose**

1. Which arm?

```promql
stellarindex_login_code_lockout_rows
sum by (op) (increase(stellarindex_login_code_lockout_errors_total[1h]))
```

2. Fail-open arm: is migration 0122 applied on every API node?

```sh
ssh r1 'sudo -u postgres psql stellarindex -c "\d login_code_lockouts"'
ssh r1 "journalctl -u stellarindex-api --since '-2h' | grep 'login code lockout status unavailable'"
```

3. Rows arm: is the sweep running?

```sh
ssh r1 "journalctl -u stellarindex-api --since '-6h' | grep 'login-code-lockout reaper'"
```

Expect a start line at boot plus a `deleted settled rows` line whenever it removes anything. `op="sweep"` failures in the metric point at Postgres (statement timeout, lock contention, permissions).

4. Rows arm: fill or real traffic? A fill is many distinct addresses with tiny `failed_count`:

```sh
ssh r1 'sudo -u postgres psql stellarindex -c "
  SELECT failed_count, count(*) AS addresses, min(created_at), max(created_at)
    FROM login_code_lockouts GROUP BY 1 ORDER BY 2 DESC LIMIT 10;"'
```

Thousands of addresses at `failed_count = 1` in a tight window is a fill. A handful at `failed_count >= 10` is a targeted grinder: the control working.

**Fix**

1. Fail-open arm: apply migration 0122 (or fix whatever made the read fail). Until then the durable lockout is off; the per-token `maxCodeAttempts = 5` cap still bounds guesses per mint but not across mints.
2. Rows arm, sweep broken: fix the Postgres cause and confirm the next pass drains. A manual pass is safe and uses the same predicate:

```sql
DELETE FROM login_code_lockouts
 WHERE updated_at < now() - interval '48 hours'
   AND (locked_until IS NULL OR locked_until <= now());
```

Keep BOTH predicates: the second protects a live lock, which must never be deleted at any age.

3. Rows arm, active fill: bounded by `api.anon_rate_limit_per_min` (default 60). Check it is non-zero on the deployment; block the source IP at the edge if single-origin. Rows are harmless once retention catches up.

**Do NOT**

- Gate the INSERT on "address has live tokens": a grinder on a real address always has live tokens, the counter must bound guessing across mints, and an existence gate is an enumeration oracle. Retention is the lever.
- Shorten retention below 24 h: it must exceed `dashboardauth.durableCodeFailureWindow` or the sweep truncates live counting windows.
- Turn the fail-open into fail-closed: it would make one table's unavailability a dashboard-wide login outage, including during the migration-lag window that causes it.

**Related** [stellarindex_admin_audit_write_failing](api.md#stellarindex_admin_audit_write_failing); [stellarindex_ratelimit_fail_open](#stellarindex_ratelimit_fail_open).

## stellarindex_monthly_quota_fail_closed

**Severity** P1 (page).

**Trigger** `sum(rate(stellarindex_monthly_quota_fail_closed_total[5m])) > 0` for 2m. The fail-open window ([stellarindex_monthly_quota_fail_open](#stellarindex_monthly_quota_fail_open)) is capped at `DefaultMonthlyQuotaDwellTime` (30s, `api.monthly_quota_dwell`). After counter reads fail continuously longer than that, the gate fails closed: every metered request gets `429` (`errors/monthly-quota-unavailable`, `Retry-After`) and `stellarindex_monthly_quota_fail_closed_total` increments instead of the fail-open counter. A sustained outage therefore shows here, not on the fail-open alert: paying customers are denied, not uncapped. It clears once the counter answers without error for the same dwell time.

**Diagnose and fix** Identical to [stellarindex_monthly_quota_fail_open](#stellarindex_monthly_quota_fail_open), treating Redis recovery as urgent. It normally moves with [stellarindex_ratelimit_fail_closed](#stellarindex_ratelimit_fail_closed) (shared backing store); if that fires, handle the Redis outage first. Do NOT shorten the dwell time.

## stellarindex_monthly_quota_fail_open

**Severity** P3 (ticket); the fail-closed twin pages. MTTR 5-30 min: clears the moment the usage counter is readable.

**Impact** Revenue. Every metered key is uncapped for the duration. Usage still meters (still billed) but the monthly ceiling is not enforced, and overage served in the window cannot be reclaimed.

**Trigger** `sum(rate(stellarindex_monthly_quota_fail_open_total[5m])) > 0` for 10m. `internal/api/v1/middleware/monthly_quota.go` deliberately fails open when the month-to-date counter read errors (the cap is billing fairness, not a security boundary; a Redis blip must not 429 paying customers). The counter increments once per bypassed request. The 10m `for:` absorbs failovers. Twin of [stellarindex_ratelimit_fail_open](#stellarindex_ratelimit_fail_open) (same backing store); the open window costs money here, not abuse headroom.

**Diagnose**

1. Redis up? `systemctl status redis` on r1, then `redis-cli -a "$REDIS_PASSWORD" ping`; `/readyz` redis checker should be red if unreachable.
2. `stellarindex_ratelimit_fail_open` also firing? Then a plain Redis availability incident: treat that as primary.
3. Firing ALONE (Redis healthy)? Failure is in the usage-counter read path, in order of likelihood:
   - Key namespace / eviction: month-to-date counters live under the `UsageKeyForSubject` namespace; `maxmemory-policy allkeys-lru` evicting them mid-month errors this path while limiter keys survive.
   - AUTH drift: password rotated on one side only; `journalctl -u stellarindex-api | grep -i 'monthly-quota'` shows `NOAUTH` / `WRONGPASS` in the WARN `err` field.
   - Connection-pool exhaustion (`context deadline exceeded` in the same line).
4. Exposure: `sum(rate(stellarindex_monthly_quota_fail_open_total[5m]))` is requests/sec bypassing the cap (only keys with `monthly_quota > 0` reach this code).

**Fix**

- Redis down/degraded: Redis recovery path; the middleware self-heals on the first successful read; the alert clears ~10 min after the last bypass.
- AUTH drift: the Redis password is the `STELLARINDEX_REDIS_PASSWORD` env override (config `[storage] redis_password`), not a hand-edited TOML value. Re-sync it in the unit's `EnvironmentFile` (`/etc/default/stellarindex`) to Redis's `requirepass` (ansible `redis_password`), then `systemctl restart stellarindex-api`.
- Eviction: confirm `maxmemory-policy`; usage counters must not be in an evictable class (Redis config fix).
- After recovery: decide whether any customer materially exceeded their cap. Metering is a separate write path and the usage rows are intact, so reconcile from the usage rollup, not this counter.
- Do NOT shorten the dwell time to fail closed sooner (a 429 on a read error hard-denies every metered customer, including those far under cap). Do NOT silence while Redis is down.

**Related** [stellarindex_usage_write_failing](#stellarindex_usage_write_failing) (write-side twin); [stellarindex_admin_audit_write_failing](api.md#stellarindex_admin_audit_write_failing); [metrics-registry-absent](metrics-registry-absent.md).

## stellarindex_passkey_clone_warning

**Severity** P3 (ticket). MTTR 15-60 min to contact the account owner.

**Impact** One account's passkey may have been copied. The tripping sign-in was refused; no session was issued.

**Trigger** `sum(increase(stellarindex_passkey_login_refusals_total{reason="clone_warning"}[1h])) > 0` for 5m. `POST /v1/auth/passkey/finish-login` refuses an assertion whose signature verifies but whose counter is at or below the stored one (two copies of the key signing independently), increments the counter and appends a `passkey.clone_warning` row to `audit_log`. Authenticators that report counter 0 forever (iCloud, most synced passkeys) are exempt, so this can only come from a hardware or device-bound key.

**Diagnose**

```sql
-- Which account and credential, from where.
SELECT ts, account_id, target_id AS credential_row_id, ip, user_agent, metadata
FROM audit_log
WHERE action = 'passkey.clone_warning'
ORDER BY ts DESC
LIMIT 20;
```

`metadata` has `credential_owner_user_id`, `credential_name`, `stored_sign_count`, `presented_sign_count`. The account's other passkey rows show when the credential was added and whether it was replayed:

```sql
SELECT ts, action, actor_kind, actor_user_id, target_id, ip, metadata
FROM audit_log
WHERE account_id = '<account_id>' AND action LIKE 'passkey.%'
ORDER BY ts DESC;
```

(`passkey.register`, `passkey.delete`, `passkey.login_replay`.)

**Fix**

- Contact the owner (`credential_owner_user_id`): remove the named passkey in dashboard security settings and register a new one.
- Owner unreachable and IPs look hostile: delete the credential row (`webauthn_credentials.id = target_id`) and revoke the user's sessions. Both are account changes: record who and why.
- Verify: no new `passkey.clone_warning` rows for that credential; the alert clears an hour after the last refusal.
- Do NOT treat it as a platform outage (the refusal is the control working) and do NOT reset the stored sign count (hides the signal and lets the copied key sign in).

**Related** [stellarindex_admin_audit_write_failing](api.md#stellarindex_admin_audit_write_failing) (fires with `surface="passkey_clone_warning"` when this event's audit row failed to land, leaving the metric as the only record); [ADR-0049](../../adr/0049-anonymous-access-and-passkey-auth.md).

## stellarindex_ratelimit_fail_closed

**Severity** Page.

**Impact** Every request hitting the affected bucket gets a 503: outright API unavailability.

**Trigger** `sum(rate(stellarindex_ratelimit_fail_closed_total[5m])) > 0` for 2m. Past `ratelimit.DefaultDwellTime` (30s) of continuous Redis errors on a bucket, the limiter fails closed: 503 (`errors/throttle-unavailable`, `Retry-After: 30`, `writeThrottleUnavailableProblem`). This is what a hard sustained Redis outage looks like, with a red Redis readiness check (the fail-open counter goes flat ~30s in). It clears after the same dwell time of unbroken Redis successes; a flapping Redis keeps it armed. Anonymous and authenticated tiers are separate buckets with separate clocks.

**Diagnose and fix** Same as [stellarindex_ratelimit_fail_open](#stellarindex_ratelimit_fail_open); Redis recovery is urgent. Calls that still fail keep answering 503 until Redis has answered without error for `DefaultDwellTime`. Do NOT disable the fail-closed switch (a negative `WithDwellTime`): an attacker who can degrade Redis would then get unlimited request volume.

**Related** [api-down](api.md#stellarindex_api_down) (where a sustained Redis outage lands once the limiter has failed closed).

## stellarindex_ratelimit_fail_open

The window is bounded: once a bucket's Redis calls have failed for longer than `ratelimit.DefaultDwellTime` (30s), the middleware fails **closed** with `503` (`errors/throttle-unavailable`, `Retry-After: 30`) and this counter stops moving ([stellarindex_ratelimit_fail_closed](#stellarindex_ratelimit_fail_closed)). The closed state clears only after the same dwell time of unbroken Redis successes. Anonymous and authenticated tiers are separate buckets with separate clocks.

**Severity** P3 (ticket). MTTR 5-30 min: usually resolves itself when Redis is reachable.

**Impact** The per-key rate limit is NOT enforced; requests are served unlimited. Metering/billing still records usage, so this is a throughput/abuse exposure, not revenue loss.

**Trigger** `sum(increase(stellarindex_ratelimit_fail_open_total[15m])) > 100`, `for: 0m` (more than 100 bypassed requests in 15 min). The limiter is a Redis fixed-window counter (atomic `INCRBY` + `EXPIRE` per key per minute, `internal/ratelimit`). On a Redis error `internal/api/v1/middleware/ratelimit.go` fails open for a bounded window (30s dwell, then fails closed; see [stellarindex_ratelimit_fail_closed](#stellarindex_ratelimit_fail_closed)) and increments the counter at both per-key and per-IP gates. Because the window is capped, this alert sees repeated short error episodes only, which is why it counts with `increase` rather than `rate ... for: 10m` (that form can never fire).

**Diagnose**

1. Redis up? `systemctl status redis` on r1, then `redis-cli -a "$REDIS_PASSWORD" ping`; check `/readyz` (redis checker).
2. Firing ALONE (Redis healthy)? Failure is in the limiter's own path, in order:
   - AUTH: `STELLARINDEX_REDIS_PASSWORD` rotated on one side; `journalctl -u stellarindex-api | grep -i 'ratelimit\|redis'` shows `NOAUTH` / `WRONGPASS`.
   - Key namespace / eviction: `maxmemory-policy allkeys-lru` evicting counters mid-window.
   - Connection-pool exhaustion (`context deadline exceeded`); `rate(http_requests_total[1m])` at an unusual peak.
3. Exposure: `sum(rate(stellarindex_ratelimit_fail_open_total[5m]))` is requests/sec bypassing; equal to `sum(rate(http_requests_total[5m]))` means NO request is being limited.

**Fix**

- Redis down: Redis recovery path. Successful calls are limited normally at once; failing calls answer 503 until Redis is clean for `DefaultDwellTime`. The alert clears ~10 min after the last bypass.
- AUTH drift: the password is the `STELLARINDEX_REDIS_PASSWORD` env override (config `[storage] redis_password`), not a hand-edited TOML value. Re-sync it in the unit's `EnvironmentFile` (`/etc/default/stellarindex`) to Redis's `requirepass` (ansible `redis_password`), then `systemctl restart stellarindex-api`.
- Sustained abuse while open: the limiter cannot help; block the offending source at the edge (Caddy/HAProxy) per the traffic-shedding section of [api-latency](api.md#stellarindex_api_latency_p99_high).
- Do NOT remove the fail-open window (zero dwell time turns every Redis blip into an outage). Do NOT silence while Redis is down.

**Related** [metrics-registry-absent](metrics-registry-absent.md).

## stellarindex_scam_gate_fail_open

**Severity** P3 (ticket). MTTR 5-30 min: clears once `account_directory` is readable.

**Impact** Trust. Every price surface consulting the scam-pricing gate serves directory-flagged issuers' prices at 200 while lookups fail.

**Trigger** `sum by (surface) (rate(stellarindex_scam_gate_lookup_failures_total[5m])) > 0` for 5m. `internal/pricingguard.ScamGate` withholds the price of any pair whose issuer carries a scam-class tag in `account_directory`. On a lookup error it deliberately fails open (failing closed would blank every price on a DB blip); a failed lookup is not cached. Each fail-open consultation increments `stellarindex_scam_gate_lookup_failures_total{surface}` and logs `scam pricing gate: directory lookup failed — serving unguarded` (cancelled client requests are not counted). `surface` is the serving path (`price_read`, `tip`, `oracle`, `price_at`, `asset_headline`, `vwap`, `twap`, `chart`, `price_stream`, `dex_tvl`, ...); `price_alert` is the aggregator's price-alert evaluator, all others are the API. One surface alone points at that path's context budget; all together point at the database.

**Diagnose**

1. Postgres reachable? Check the API `/readyz`; Timescale checker red means a database incident (primary).
2. Error cause: `journalctl -u stellarindex-api | grep 'scam pricing gate'` (`-u stellarindex-aggregator` for `price_alert`); the WARN `err` names it (lock wait, `relation does not exist` mid-migration, pool exhaustion, `context deadline exceeded`).
3. Table readable? `psql "$DATABASE_URL" -c 'SELECT count(*) FROM account_directory'`; a hang means a lock from a long `directory-sync` transaction or a migration.

**Fix**

- Database down/degraded: database recovery path; the gate self-heals on the next good lookup (verdicts cached 60 s per issuer).
- Lock from `directory-sync`: let it finish or cancel it (safe to re-run).
- Pool exhaustion: the gate shares the API's Postgres pool; relieve the load.
- Do NOT fail closed (a directory error would withhold every classic asset's price everywhere).

**Related** [stellarindex_ratelimit_fail_open](#stellarindex_ratelimit_fail_open) and [stellarindex_monthly_quota_fail_open](#stellarindex_monthly_quota_fail_open) (other API fail-open paths); `stellarindex_price_serve_scam_withheld_total` in the [metrics reference](../../reference/metrics/README.md) (success-side counter; a drop to zero while this fires is the same outage seen from the other side); [stellarindex_directory_sync_unflagged](#stellarindex_directory_sync_unflagged).

## stellarindex_sdex_orderbook_maintain_failing

**Severity** ticket. Rules: `deploy/monitoring/rules/api.yml` and `configs/prometheus/rules.r1/api.yml`. MTTR 5-30 min (ClickHouse reachability, or the initial load exceeding its 30-min cap). The four sdex_orderbook alerts share this section's diagnosis; check WHICH outcome or alert is firing.

**Impact by mode**

- `load_error`: `/v1/sdex/orderbook` serves a 503 warming problem (endpoint outage).
- `advance_error`: increasingly stale depth, honestly timestamped (`as_of_ledger` stops advancing).
- `verify_error`: the version-tie quarantine stops draining; its offers stay out of every served book (visible as `ask_offers_withheld` / `bid_offers_withheld`).

**Trigger** `sum(rate(stellarindex_sdex_orderbook_maintain_total{outcome=~"load_error|advance_error|verify_error"}[15m])) > 0` for 30m. Evidence: `journalctl -u stellarindex-api | grep "sdex order book"` shows the lake error per attempt (load retries ride the 60s advance ticker); `curl localhost:3000/v1/sdex/orderbook?selling=native&buying=USDC-G...` returns 503 (load never landed), a stale `as_of_ledger` (advance failing) or non-zero `*_offers_withheld`; `stellarindex_sdex_orderbook_pending_offers` flat and non-zero while `verify_error` rises means the quarantine is wedged.

**Diagnose**

```sh
# Which mode? load_error / advance_error / verify_error:
curl -s localhost:9464/metrics | grep sdex_orderbook_maintain_total

# Quarantine size and crossed pairs:
curl -s localhost:9464/metrics | grep -E 'sdex_orderbook_(pending_offers|crossed_pairs) '

# Which pairs are crossed (logged on every change of the count):
journalctl -u stellarindex-api --since -2h | grep "crossed-pair count changed" | tail -3

# Lake health — both paths read ClickHouse:
clickhouse-client --port 9300 -q "SELECT 1"

# If load_error: how long are attempts running before dying?
curl -s localhost:9464/metrics | grep 'sdex_orderbook_maintain_duration_seconds.*load_error' | tail -3
```

The book loads once per process start from the lake's live-offer slice and advances with partition-pruned incremental reads keyed by `version` (`internal/storage/clickhouse/sdex_offer_book_reader.go`). Failures are lake reads only (no served-tier or Redis dependency). A creeping `load_ok` duration across deploys predicts hitting the cap.

**Fix**

1. `advance_error` with ClickHouse healthy: transient; the next 60s tick retries from the same cursor (idempotent, versioned). Investigate the specific error only if sustained.
2. `load_error` repeating: the initial full-slice FINAL load (minutes of streaming IO, 30-min hard cap) keeps dying. Check for a heavy one-shot job saturating ClickHouse (one-heavy-job rule); it retries every 60s tick and self-heals once the lake frees up.
3. Load consistently hits the 30-min cap on a healthy lake: the live-offer slice outgrew the load's work shape; a code/schema issue. File it; do not raise the cap ad hoc.
4. `verify_error` sustained: the removal probe (`OfferRemovedAt`, an `IN (...)` point read on `ledger_entry_changes`) is failing. The book stays honest (suspects withheld, never served) but is thinner than the chain. Treat as a ClickHouse read failure; the journal line `sdex order book verify` carries the error.
5. Restart re-runs the full load; worthwhile only if the maintainer goroutine is wedged (no load/advance observations for several minutes). A restart re-quarantines every `intra_ledger_seq == 0` offer, so expect `pending_offers` to jump and `*_offers_withheld` non-zero for hours.

**False positive** A deploy restarting the API mid-ClickHouse-merge can fail the first load; the 60s retry lands and the 30m `for:` absorbs it. If it fired across a deploy, confirm `load_ok` was observed afterwards and close.

**Related** Metrics `stellarindex_sdex_orderbook_maintain_total` / `_maintain_duration_seconds` in `docs/reference/metrics/README.md`; [stellarindex_dex_tvl_refresh_failing](#stellarindex_dex_tvl_refresh_failing) (same binary, same lake dependency); `internal/api/v1/sdex_orderbook.go`.

## stellarindex_sdex_orderbook_advance_held

**Severity** ticket.

**Impact** Same staleness as `advance_error` (`as_of_ledger` stops advancing) but never an error: a lake hole or ingest halt holds the cursor at a fixed ledger.

**Trigger** `sum(rate(stellarindex_sdex_orderbook_maintain_total{outcome="advance_held"}[15m])) > 0 unless sum(rate(stellarindex_sdex_orderbook_maintain_total{outcome="advance_ok"}[15m])) > 0` for 30m, with NO error outcome incrementing (so [stellarindex_sdex_orderbook_maintain_failing](#stellarindex_sdex_orderbook_maintain_failing) stays silent).

**Diagnose** The cursor is held below an unhealed `ledger_entry_changes` hole, or the lake stopped receiving ledgers. The API journal says which via "advance held below a lake hole" (cursor / contiguous_tip / lake_max). Check `ch-live-catchup` and `stellarindex_ingestion_cursor_stuck` / `stellarindex_ingestion_ledger_stalled` first: a genuine lake-wide halt pages there too, and this is the order book's consumer-facing signal for the same root cause.

**Fix** Self-heals the moment the hole is healed or ingest resumes; no ops action beyond confirming that. Shared diagnosis commands: [stellarindex_sdex_orderbook_maintain_failing](#stellarindex_sdex_orderbook_maintain_failing).

## stellarindex_sdex_orderbook_reload_failing

**Severity** ticket.

**Trigger** `sum by (instance) (increase(stellarindex_sdex_orderbook_maintain_total{outcome="load_error"}[3h])) > 0 unless sum by (instance) (increase(stellarindex_sdex_orderbook_maintain_total{outcome="load_ok"}[3h])) > 0` for 2h. `load_error` rises about once per hour with no `load_ok` for 2+ h: the daily self-heal re-load keeps failing its hourly retry (too sparse for `maintain_failing`).

**Impact** This is the same load failing AFTER the book first landed. The previous book keeps serving and advancing, so the endpoint answers 200. What is lost is the self-heal: changes that landed below the cursor stay wrong until a re-load lands.

**Diagnose and fix** Same checks as the `load_error` case in [stellarindex_sdex_orderbook_maintain_failing](#stellarindex_sdex_orderbook_maintain_failing) (ClickHouse reachability, heavy one-shot saturation, 30-min cap). It clears on the first `load_ok`.

## stellarindex_sdex_orderbook_crossed_book

**Severity** ticket.

**Impact** A served pair has best bid > best ask: phantom offers on that market.

**Trigger** `max(stellarindex_sdex_orderbook_crossed_pairs) > 0` for 30m. A touching book (best bid == best ask) is NOT counted (a PASSIVE offer legally rests at exactly the inverse price); anything counted is strictly crossed and not a false positive.

**Diagnose** Take pair ids from the journal (`journalctl -u stellarindex-api --since -2h | grep "crossed-pair count changed" | tail -3`) and check the served book (`/v1/sdex/orderbook?selling=A&buying=B`). A crossed resting book is impossible on-chain, so one side carries an offer whose removal the lake never ingested. Verification cannot disprove it and the daily re-load reloads it.

**Fix** No ops-side mitigation short of healing the lake: chase the missing `ledger_entry_changes` removal with the completeness tooling.

## stellarindex_tls_cert_expiring_soon

**Severity** P2 (ticket). Rules: `deploy/monitoring/rules/api.yml` and `configs/prometheus/rules.r1/api.yml`. MTTR 15-60 min.

**Impact** The TLS handshake fails at expiry: `api.stellarindex.io` would fail every request and customer integrations break. The 14-day head room leaves time to renew manually.

**Trigger** `for: 1h`, three arms:

```promql
(
  stellarindex_tls_cert_not_after_unix - time() < 14 * 24 * 3600
  and stellarindex_tls_cert_not_after_unix > 0
)
or absent_over_time(stellarindex_tls_cert_not_after_unix[1h]) == 1
or (
  sum by (host) (increase(stellarindex_tls_cert_probe_total{outcome!="ok"}[13h])) > 0
  unless on (host)
  sum by (host) (increase(stellarindex_tls_cert_probe_total{outcome="ok"}[13h])) > 0
)
```

The `> 0` arm is a defensive floor (a zero sample would fire permanently). A failing probe KEEPS the last-known gauge value (`internal/obs/metrics.go`), so the probe-failure arms are the liveness signal. The `absent_over_time` arm means the gauge has no series at all (the prober is down or never succeeded). The probe-outcome arm means a host had failed probes and no `ok` probe in 13 h (unreachable, or a cert failing verification); its alert value is a failure count, not a timestamp. The raw counter query is `sum by (host, outcome) (rate(stellarindex_tls_cert_probe_total{outcome!="ok"}[1h]))`. The probe runs from the API binary every 6 h (`TLSCertProbeInterval`, `internal/api/v1/tls_probe.go::RunTLSCertProbe`, plus one at startup); the 14-day threshold gives 56 successful probes' head room. Caddy's journal (`journalctl -u caddy`) may show renewal errors.

**Diagnose**

```sh
ssh root@<host>

# 1. Confirm the gauge value vs. NOW.
curl -sS localhost:3000/metrics | grep stellarindex_tls_cert_not_after_unix

# 2. Read the actual on-disk cert Caddy is serving.
openssl x509 -in /var/lib/caddy/.local/share/caddy/certificates/acme-v02.api.letsencrypt.org-directory/api.stellarindex.io/api.stellarindex.io.crt -noout -enddate

# 3. Check Caddy's renewal log for the most recent attempt.
journalctl -u caddy --since "30d ago" --no-pager | grep -iE "renew|certificate"
```

Likely causes:

1. ACME rate limit (Let's Encrypt: 5 duplicate certs/week, 50 certs/account/week): look for `429` / `tooManyCertificatesPerName` in Caddy's journal.
2. DNS-01 challenge failing (not default; TXT propagation stuck).
3. HTTP-01 failing: port 80 unreachable (firewall, Caddy not on :80, Cloudflare proxying).
4. Caddy disk full: `/var/lib/caddy` is on the root partition; if `/` is full Caddy cannot write the cert.
5. Caddy stopped/crashed (renewal needs Caddy alive during the 30-day pre-expiry window).

**Fix**

The live config is `/etc/caddy/Caddyfile`, rendered by `configs/ansible/roles/archival-node/templates/Caddyfile.j2`. `Caddyfile.api` is a repo filename (`configs/caddy/Caddyfile.api`) not present on the host.

```sh
# Trigger renewal without touching the cert (admin endpoint):
curl -X POST 'http://localhost:2019/load' --data-binary @/etc/caddy/Caddyfile -H 'Content-Type: text/caddyfile'

# Or reload/restart (renewals attempt at startup):
caddy validate --config /etc/caddy/Caddyfile && systemctl reload caddy

# Watch the renewal attempt:
journalctl -u caddy -f
```

If Caddy cannot renew (rate limited etc.), fall back to certbot standalone or ZeroSSL via an alternate ACME directory. The directive is `acme_ca` inside the site's `tls` block (`acme_ca_root` is a different directive, a trusted root PEM for a private ACME endpoint):

```caddyfile
tls {
    acme_ca https://acme.zerossl.com/v2/DV90
}
```

Codify any such change in `Caddyfile.j2`, not just on the host. Full TLS provisioning sequence: `docs/operations/r1-deployment-state.md`.

Verify:

```sh
# Probe runs every 6h; force one by restarting the API binary:
systemctl restart stellarindex-api
sleep 30

curl -sS localhost:3000/metrics | grep stellarindex_tls_cert_not_after_unix
# Should show a NotAfter ~90 days in the future for Let's Encrypt.
```

The alert clears after `for: 1h` with the new gauge value. Metric reference: `docs/reference/metrics/README.md#stellarindex_tls_cert_not_after_unix`.

## stellarindex_usage_rollup_failing

**Severity** informational (ticket). Rules: `deploy/monitoring/rules/api.yml` and `configs/prometheus/rules.r1/api.yml`. MTTR 5-15 min (almost always Redis or Postgres reachability, shared with louder alerts).

**Impact** Dashboard per-endpoint usage analytics stop advancing; `/v1/account/usage` degrades to endpoint-less legacy per-day rows. No customer pricing impact. Redis counters keep accumulating (35-day TTL). After recovery the worker re-folds every day no successful sweep covered, oldest first, 7 days per sweep (a restarted API walks the whole 35-day window once). A day older than 35 days when the outage ends is lost.

**Trigger** `sum(rate(stellarindex_usage_rollup_sweeps_total{outcome=~"scan_error|sink_error"}[15m])) > 0` for 30m (5-min sweep cadence, so at least 6 consecutive failures; `outcome="ok"` flat separates a failing rollup from a quiet counter). `journalctl -u stellarindex-api | grep "usage rollup sweep failed"` shows the Redis/Postgres error every ~5 min; `/dashboard/usage` per-endpoint table freezes at the last good sweep while daily totals may still move (Redis fallback path). The worker is thin (one SCAN + HGETALLs in Redis, one batched upsert in Postgres), so failure with healthy `/v1/price` traffic usually means the host lost ONE backend; check what else fired.

**Diagnose**

```sh
# Which half is failing?
curl -s localhost:3000/metrics | grep stellarindex_usage_rollup_sweeps_total
# scan_error -> Redis. Check reachability + the counter keys:
redis-cli -n 0 --scan --pattern 'usage:ep:*' | head
# sink_error -> Postgres. Check the table exists (migration 0071):
sudo -u postgres psql stellarindex -c '\d usage_daily'
# The worker's own log line carries the wrapped error:
journalctl -u stellarindex-api --since -30min | grep -i "usage rollup"
```

**Fix**

- `sink_error` + missing table: migration 0071 did not apply. Run the migrator (deploy.yml auto-applies; manual: `stellarindex-migrate -migrations /usr/local/share/stellarindex/migrations up`).
- Redis/Postgres down: follow the infra runbook; the alert clears on the next good sweep. The worker retries forever and re-folds skipped days (7 per 5-minute sweep, oldest first; GREATEST-merged upsert so replays are safe). Check the days landed:

```sh
sudo -u postgres psql stellarindex -c "SELECT day, count(*) FROM usage_daily WHERE day >= now() - interval '7 days' GROUP BY day ORDER BY day"
```

- To fold a range immediately, or when no API process runs the rollup, use `usage-rollup-backfill` (same grouping code as the live worker, safe to re-run):

```sh
# Size the recovery first (dry run, no writes):
stellarindex-ops usage-rollup-backfill -config /etc/stellarindex.toml \
  -from 2026-07-19 -to 2026-07-21
# Then apply it:
stellarindex-ops usage-rollup-backfill -config /etc/stellarindex.toml \
  -from 2026-07-19 -to 2026-07-21 -write
```

The Redis detail counters carry a 35-day TTL, so this only works inside that window; beyond it unswept days are gone (note in the incident log, no re-derive path).

**Related** Metrics [`stellarindex_usage_rollup_sweeps_total`](../../reference/metrics/README.md#stellarindex_usage_rollup_sweeps_total) and [`stellarindex_usage_rollup_sweep_duration_seconds`](../../reference/metrics/README.md#stellarindex_usage_rollup_sweep_duration_seconds); worker `internal/usage/rollup.go` (wired in `cmd/stellarindex-api/main.go`); `cmd/stellarindex-ops/usage_rollup_backfill.go`; table `migrations/0071_create_usage_daily.up.sql`; [alerts-catalog.md](../alerts-catalog.md).

## stellarindex_usage_write_failing

**Severity** P3 (ticket). MTTR 5-30 min: clears once usage-counter writes succeed.

**Impact** Revenue. Each dropped billable unit is unmetered traffic. Because the WRITE failed (not the read), `MonthToDate` sees a real, low Redis value, so `MonthlyQuota` never fails open and nothing else signals the ceiling is uncapped.

**Trigger** `sum(rate(stellarindex_usage_units_dropped_total{counter="billable"}[5m])) > 0` for 10m. `UsageTracker` (`internal/api/v1/middleware/usage.go`) best-effort increments per-subject Redis usage counters per metered request; `stellarindex_usage_units_dropped_total{counter="billable"|"detail"}` grows by the request's unit count on each failed write (this rule watches `billable`, which feeds `MonthlyQuota`). Write-side twin of [stellarindex_monthly_quota_fail_open](#stellarindex_monthly_quota_fail_open) (that fires on read failure; the two are mutually exclusive per request and point at opposite ends of the pair, so confirm which is firing before paging).

**Diagnose**

1. Redis up? Same first check as [stellarindex_monthly_quota_fail_open](#stellarindex_monthly_quota_fail_open): `redis-cli -a "$REDIS_PASSWORD" ping`, `/readyz`.
2. Firing ALONE (Redis healthy)? Suspect the usage key namespace, an ACL change scoped to `usage:*` / `usage:ep:*`, or a wedged `TxPipeline`: `journalctl -u stellarindex-api | grep -i 'usage: increment failed\|usage: detail increment failed'` for the `err` field (Debug level; raise the unit's log level temporarily if needed).
3. Exposure: `sum(rate(stellarindex_usage_units_dropped_total{counter="billable"}[5m]))` is request-units/sec going unmetered.

**Fix**

- Redis down/degraded: Redis recovery path; the tracker self-heals on the first successful write.
- Namespace/ACL drift: confirm the API's Redis credential can write the `usage:` prefix. It is the same credential `monthly_quota.go` reads with, so if only writes fail suspect a permission asymmetry (e.g. a read-replica ACL on the wrong connection).
- After recovery: the durable `usage_daily` rollup (same counters, 5-min cadence) is a lower bound on what was served; reconcile from it, not this alert's rate.
- Do NOT silence because "metering is best-effort": that covers one request's fate, not a sustained silent revenue leak.

**Related** [stellarindex_ratelimit_fail_open](#stellarindex_ratelimit_fail_open) (same backing store, different counter family).

## Related

- SLO burn alerts (`stellarindex_slo_*`) link to the `stellarindex_api_error_rate_critical` and `stellarindex_api_latency_p95_high` sections.
