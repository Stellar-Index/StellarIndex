---
title: Runbook — API 5xx rate elevated
last_verified: 2026-08-29
status: ratified
severity: P1 at >5% / P3 at >1% / P1 at SLO burn-rate fast+medium
---

# Runbook — `stellarindex_api_error_rate_{high,critical}` (+ SLO availability burn-rate variants)

> **R1 REALITY (2026-08-29).** The `api-01..03` / HAProxy / keepalived
> topology below is the ADR-0008 Phase-1 multi-host shape — it is
> **INERT on r1**. Today there is ONE host: Cloudflare → Caddy
> (`configs/caddy/Caddyfile.api`, TLS termination + health-checked
> `reverse_proxy localhost:3000`) → `stellarindex-api.service`. Where a
> section below drains HAProxy backends or loops over `api-0X` hosts,
> use the r1-real equivalent given inline; the multi-host text is kept
> [bracketed] for the eventual rollout, not deleted.

## At a glance

| Field | Value |
| ----- | ----- |
| Direct-threshold alerts | `stellarindex_api_error_rate_high` (>1 % for 2 min) → **P3** (`severity: ticket`)<br>`stellarindex_api_error_rate_critical` (>5 % for 2 min) → P1 (`severity: page`) |
| SLO burn-rate alerts | `stellarindex_slo_availability_burn_{fast,medium,slow}` (per ADR-0009 multi-window pattern) |
| Severity | **P1 at critical**, **P3 at high**; **P1** for fast/medium burn, P3 for slow burn |
| Detected by | `configs/prometheus/rules.r1/api.yml` + `configs/prometheus/rules.r1/slo.yml` (r1 overlays, `job="stellarindex-api"`, loaded from `/etc/prometheus/rules.r1/*.yml`; multi-host templates: `deploy/monitoring/rules/{api,slo}.yml`) — rules on `http_requests_total{status=~"5.."}` rate |
| Typical MTTR | 5–15 min for a bad-deploy revert; 30–60 min for a latent-bug forward fix |
| Impact | Clients seeing request failures. Affects both the API availability SLA (99.99 % non-5xx over 30 d) and the p95/p99 latency targets — every 5xx adds timeout retries that inflate queue time. |

## Burn-rate vs direct-threshold pages

This runbook handles two alert families with different semantics:

- **Direct-threshold** (`api_error_rate_{high,critical}`):
  "instantaneous 5xx rate just crossed 1 % / 5 %." Trips on
  any sustained spike, including a brief deploy hiccup.
- **SLO burn-rate** (`slo_availability_burn_{fast,medium,slow}`):
  "we're consuming SLO error budget too quickly." The 99.99 %
  availability target gives a small monthly budget; the
  multi-window pattern (per Google SRE workbook) requires a short
  AND a long window to both agree before firing, so brief blips
  don't trip it. Fast tier (5m AND 1h, 14.4× burn) means the whole
  30-day budget is gone in ~2 days; medium (30m AND 6h, 6×) gives
  ~5 days; slow (6h AND 24h, 1×) spends it in exactly 30 days —
  zero slack.

A `_burn_fast` page is a real availability emergency even if the
direct-threshold P1 (`error_rate_critical`, > 5 %) hasn't fired:
the 99.99 % budget is so small that **0.15 %** errors sustained for
1h is enough (the fast trip point is 14.4 × 0.0001 = 0.144 %). Use
the diagnosis below as usual; treat the urgency as
budget-exhaustion, not transient.

## Symptoms

- Pager fires on `stellarindex_api_error_rate_{high|critical}`.
- Grafana "Golden Signals" dashboard shows an error-rate cliff-edge
  or sawtooth pattern.
- Concurrent alerts likely: `stellarindex_api_latency_p95_high`
  (timeouts inflate latency), possibly `stellarindex_api_price_stale`
  if Timescale is the root cause.

## Quick diagnosis (≤ 5 min)

[Multi-host: the API tier runs as `stellarindex-api.service` on three
hosts (`api-01..03`) behind two HAProxy hosts sharing a keepalived VIP,
per [ADR-0008](../../adr/0008-ha-topology.md) §1, no Kubernetes.] On
r1 today: one `stellarindex-api.service` behind Caddy on the same box.
Three signals in order. The first that flags non-zero wins; skip
the rest.

### 1. What's actually failing?

```sh
# Top status/route combinations by count in the last 5 min
# (on r1, Prometheus is local on :9090)
curl -s 'http://127.0.0.1:9090/api/v1/query' \
  --data-urlencode 'query=topk(5, sum by (route, status) (rate(http_requests_total{job="stellarindex-api",status=~"5.."}[5m])))' \
  | jq .data.result
# (or click the Grafana link in the alert annotation)
```

Expect a single dominant `{route="…", status="500"}` or `"503"`
pair. If errors spread across every route, the root cause is
shared infrastructure (DB, Redis, upstream RPC) — skip to §3.

### 2. Is it a recent deploy?

```sh
# Running version (r1: one host)
curl -sf http://localhost:3000/v1/version | jq -r .data.version

# When did the unit last (re)start?
systemctl show stellarindex-api -p ActiveEnterTimestamp --value

# Or: `r1-deployment-state.md` records the running tag.
git log --oneline -1 docs/operations/r1-deployment-state.md

# [Multi-host: loop the same two commands over api-01..03 via
#  `ssh root@api-0X "…"` and compare versions across backends.]
```

Correlate the last unit-restart timestamp against the error-rate
lift. If a release within the last ~1 h precedes the rise →
revert per §A (Mitigation).

### 3. Is a dependency the root cause?

Deps exposed via /v1/readyz. Does it report degraded?

```sh
curl -sSf https://api.stellarindex.io/v1/readyz | jq '.data'
# Expected:
# { "status": "ok" | "degraded",
#   "checks": [ { "name": "postgres", "ok": true }, ... ] }
```

- `postgres.ok == false` → [timescale-primary-down](timescale-primary-down.md).
- `redis.ok == false` → [redis-master-down](redis-master-down.md).
- All OK but 5xx still elevated → handler-level bug, §B Mitigation.

### 4. Is there a visible pattern in the logs?

```sh
# Pull the last N ERROR-level log lines and group (r1: run locally).
# NOTE the two log shapes: access-log lines carry
# method/path/status/latency_ms/request_id but NO .level and NO .err;
# the ERROR-level lines with the underlying error are SEPARATE log
# lines. This filter selects the latter.
journalctl -u stellarindex-api --since '15 min ago' --no-pager -o cat \
  | jq -r 'select(.level=="ERROR") | [.msg, .err] | @tsv' \
  | sort | uniq -c | sort -rn | head
```

Patterns to look for:
- Panic stack trace → handler bug; go to §B.
- `dial tcp … connection refused` → upstream issue (DB/Redis/RPC);
  return to §3.
- `context deadline exceeded` → slow dependency; check dependency
  latency dashboards.
- Handler-specific error like `ErrPriceNotFound` at a higher than
  normal rate → data issue, not a production incident; suppress
  alert if sustained.

## Mitigation

Pick by diagnosis; don't work through sequentially.

### A. Recent deploy is the cause — **revert**

Fastest path. Ship-and-revert is cheaper than production-debug.

On r1 today, roll back by re-deploying the previous tag:

```sh
gh workflow run deploy.yml -f region=r1 -f version=<previous-tag> \
  -f binaries=stellarindex-api
```

The workflow does stage → backup → atomic install → restart → health
probe → **automatic rollback on probe failure** (`deploy-workflow.md`)
— so if the bad deploy just happened, the auto-rollback may already
have run; check before deploying again. Manual fallback: the
single-host binary swap per [`release-process.md`](../release-process.md)
→ Rollback — swap back to
`/usr/local/bin/stellarindex-api.prev-<previous-tag>` (the deploy
task keeps the last 5; `/var/lib/stellarindex/deployed-versions/stellarindex-api`
tracks the running version; installs go to `/usr/local/bin/`, not
`/opt/stellarindex/release-<tag>/`) and restart the unit.

[Multi-host: the rolling version — drain one host out of HAProxy via
the admin socket (`disable server api_pool/api-01`), swap that host's
binary to the previous tag, restart the unit, re-enable in HAProxy,
repeat for `api-02` and `api-03`. The two undrained hosts carry
traffic during each swap.]

Verification:
- [ ] `stellarindex_api_error_rate_critical` clears within 3 min.
- [ ] /v1/healthz returns 200.
- [ ] /v1/readyz returns `status=ok` on at least 3 consecutive polls.
- [ ] `curl -sf http://localhost:3000/v1/version | jq -r .data.version`
      reports the previous tag [multi-host: on every backend].

Only after the incident is contained: file a postmortem action
item to explain why CI + the rolling deploy didn't catch it.

### B. Handler bug (no recent deploy) — **gate + fix forward**

Panics in a handler usually indicate a nil-dereference on an
unexpected input shape. Recoverer catches them (returns 500
problem+json), so we don't crash — but the 5xx rate climbs.

If the bug is **isolated to one endpoint**, two options:

1. **Caddy path gate** (r1, fastest, ≤ 2 min). Add a block ABOVE the
   catch-all `reverse_proxy` in the `api.stellarindex.io` site of
   `/etc/caddy/Caddyfile` (repo source: `configs/caddy/Caddyfile.api`),
   validate, then reload — `systemctl reload caddy` is graceful:

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

   Codify the change in `configs/caddy/Caddyfile.api` in the same PR
   as the fix (r1 is ansible-managed — hand fixes page Monday
   morning), and remove the gate after the fixed deploy.

   [Multi-host: the HAProxy equivalent — `http-request return status
   503 content-type "application/problem+json" string "…" if
   { path_beg /v1/history }` in backend `api_pool`, `haproxy -c` to
   validate, `systemctl reload haproxy`, on both `lb-01` and `lb-02`.]

2. **Feature-flag deny in the binary** (if a flag exists for the
   endpoint): edit `/etc/stellarindex.toml` (via the ansible overlay),
   then `systemctl restart stellarindex-api`.

Then fix, test, deploy. Remove the block after deploy.

If the bug affects **every handler** (e.g. middleware panic):
treat as §A even if the deploy isn't recent — roll back to the
last-known-good binary. You can't path-gate around middleware.

### C. Dependency failure — chase the real alert

If /v1/readyz points at a dep being down, the dependency's runbook
is the one to follow:
- [timescale-primary-down](timescale-primary-down.md)
- [redis-master-down](redis-master-down.md)
- [all-ingestion-down](all-ingestion-down.md)

This alert will auto-resolve once the dep recovers.

### D. Load-induced — **shed + rate-limit at the edge**

Rare but possible (e.g. viral traffic, DDoS), characterised by:
- Error rate climbs WITHOUT a deploy, a dep failure, or a log
  pattern.
- `stellarindex_api_latency_p99_high` fires in tandem.
- `http_requests_total` rate is sharply higher than baseline.

Bare metal does not auto-scale. r1 is one fixed-capacity host
[multi-host: the three API hosts are fixed capacity per
[ADR-0008](../../adr/0008-ha-topology.md) §4], so the answer is
**shed load**, not "add hosts":

1. **Tighten edge rate-limits.** Cloudflare WAF → short-TTL
   per-IP rate-limit rule. The API is CF-fronted (the Caddyfile's
   `trusted_proxies` block exists exactly for this), so this drops
   the abusive traffic before it reaches the box at all.
2. **Drop the heaviest non-essential paths.** SSE clients
   (`/v1/price/stream`) and batch reads are higher-cost per
   request than tip lookups. Temporary Caddy `handle`/`respond 503`
   gate on those paths buys headroom for the serving-tier
   endpoints; same procedure as §B option 1 [multi-host: the
   HAProxy `http-request return status 503` equivalent].
3. **Promote AWS DR if the colo is genuinely capacity-saturated.**
   This is a SEV-1 escalation — the cloud DR pool exists for
   exactly this case but flipping DNS to it is heavyweight; only
   do it if §1 + §2 don't clear within 10 min. Follow the
   end-to-end procedure in
   [`dr-activation.md`](dr-activation.md) (status: ratified;
   covers the full SEV-1 cutover from R1 → R2/R3). The AWS-
   side warm-standby bring-up sequence is the
   [`ha-plan.md`](../../architecture/ha-plan.md) §2.2
   ("DR — cloud — AWS primary") reference the dr-activation
   runbook builds on.

## Root cause analysis

For the postmortem (§6 of sev-playbook.md):

- `journalctl -u stellarindex-api --since '1h ago' --no-pager`
  → full log dump [multi-host: over `ssh root@api-0X` on each of
  the three hosts].
- Grafana screenshot of the 1 h window around the alert.
- `git log -n 20 main` — was there a deploy-time trigger?
- `systemctl status stellarindex-api --no-pager` —
  recent restarts, OOM-killer activity (also `dmesg | grep -i
  oom`).
- Caddy JSON access log for the same window
  (`journalctl -u caddy --since '1h ago' --no-pager -o cat`) —
  upstream health-check transitions, Caddy-generated 502/503
  attribution [multi-host: the HAProxy access log,
  `/var/log/haproxy.log`, per-backend].
- If Recoverer caught panics: the stack traces + request_ids
  needed to build fixtures.
- If Timescale was involved: slow-query log around the incident
  window.

Common root-cause patterns:
1. **Nil-pointer in a handler on a new input shape** — Recoverer
   catches it → 500. Fix: validate input earlier, add a test for
   the pathological shape.
2. **Timescale primary down** — every /v1/price call that falls
   through to LatestTradesForPair returns 500. Fix: dependency's
   runbook; handler-side, consider a short-term Redis-only
   fallback with `reduced_redundancy=true` in the envelope.
3. **Out-of-memory on a batch endpoint** — a client sent
   `asset_ids=<1000 assets>` and the in-memory result triggered
   OOM. Fix: hard cap batch size in the handler (the OpenAPI spec,
   `openapi/stellar-index.v1.yaml` `/price/batch`, commits to max
   100 ids for GET and 1000 for the POST body form — verify
   enforcement).
4. **Context-deadline exceeded on slow CAGG query** — first
   request of the day hits a cold CAGG partition. Fix: keep-
   warm job that queries each CAGG every few minutes.

## Known false-positive patterns

- **Synthetic monitoring sends 4xx to unknown assets** — not
  5xx, doesn't trigger this alert. Safe to ignore.
- **Minute-zero after release** — a restart briefly serves
  503 from the host that just (re)started before its `/v1/readyz`
  flips green (on r1, Caddy's 10s-interval `/v1/healthz` active
  health check bounds the window; Caddy-generated 502/503 are not
  in `http_requests_total` anyway). Alert window is 2 min so a
  normal release won't trip it. [Multi-host: the 10s `slowstart`
  in HAProxy + the readyz check bound this to a few seconds per
  host in a rolling release.] If you see it during a planned
  rollout, the deploy script should silence this alert for the
  window.

## Related

- [api-down](api-down.md) — every backend Down rather than just
  errored.
- [api-latency](api-latency.md) — runs in parallel when the 5xx
  is from timeouts.
- [timescale-primary-down](timescale-primary-down.md) — likely
  cause when 5xx is global + readyz shows postgres down.
- [release-process.md](../release-process.md) → Rollback — the
  binary-swap procedure cited in §A.
- [sev-playbook](../sev-playbook.md) §3 — detection channels;
  §4 — response flow; §5 — public-comms templates.
- [alerts-catalog](../alerts-catalog.md) — the rules this
  runbook serves.
- [ha-plan.md](../../architecture/ha-plan.md) §9 — degradation
  flags (`stale`, `reduced_redundancy`) the handler returns
  during partial outages.

## Changelog

- 2026-08-29 — re-verified against HEAD: `error_rate_high` is P3
  (`severity: ticket`), not P2; availability SLA 99.9 % → 99.99 %; the
  "1.5 % for 1h" burn example was a 10× decimal slip → 0.15 % (fast
  trip point 0.144 %); burn-tier budget arithmetic (≈2 d / ≈5 d /
  exactly 30 d); R1-REALITY banner — the api-01..03 / HAProxy /
  keepalived topology is the inert ADR-0008 Phase-1 shape; r1-real
  equivalents added (local `/v1/version` check, `gh workflow run
  deploy.yml` rollback with auto-rollback semantics, Caddy
  `handle`/`respond` endpoint gate + `systemctl reload caddy`, Caddy
  JSON access log); multi-host procedures bracketed, not deleted;
  `promql` CLI → local Prometheus curl; journalctl|jq pipelines get
  `-o cat` + the access-log-vs-ERROR-line split documented; dead
  `api-design.md` pointer → the OpenAPI spec's `/price/batch` limits;
  Detected-by → dual-tree (r1 primary).
- 2026-04-23 — initial draft. @ash.
- 2026-04-30 — runbook now also covers the SLO multi-window
  availability burn-rate alerts shipped in #313 (per ADR-0009),
  which route here.
- 2026-05-02 — converted from kubectl/Istio commands to
  systemd / journalctl / HAProxy admin socket, reflecting the
  bare-metal deployment ratified in ADR-0008. §D scale guidance
  rewritten — bare metal doesn't autoscale; shed load + edge
  rate-limit is the real mitigation.
