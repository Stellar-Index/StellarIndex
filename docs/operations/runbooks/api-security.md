---
title: Runbook — api-security alerts
last_verified: 2026-10-05
status: draft
---
# API security alerts

Alerts from `configs/prometheus/rules.r1/api-security.yml` (mirror: `deploy/monitoring/rules/api-security.yml`).

## At a glance

- [`stellarindex_api_cors_wildcard_in_prod`](#stellarindex_api_cors_wildcard_in_prod)

## stellarindex_api_cors_wildcard_in_prod

**Severity** P3 (ticket). MTTR 10-30 min (config change + redeploy).
**Trigger** `sum(increase(stellarindex_api_cors_decisions_total{outcome="allowed_wildcard"}[15m])) > 0` for 5m. `STELLARINDEX_ALLOWED_ORIGINS=*` is set on a production api instance and is matching real cross-origin requests. If `auth_mode` is credentialed, any origin can read authenticated responses. Outcomes: `no_origin`, `allowed_origin`, `allowed_wildcard`, `denied`. Boot-time twin: `warnOpenCORS` in `cmd/stellarindex-api/main.go`; decision code in `internal/api/v1/middleware/cors.go`.

**Diagnose**

```sh
# Confirm the running config
curl -fs http://localhost:9464/debug/config 2>/dev/null | grep -i allowed_origins || true
echo "$STELLARINDEX_ALLOWED_ORIGINS"

# Confirm which outcome is firing and at what rate
curl -fs http://localhost:9464/metrics | grep stellarindex_api_cors_decisions_total
```

**Fix**

1. If credentials (cookies / API keys tied to a session) are in play for browser traffic, replace the wildcard with an explicit allow-list of the known frontend origin(s) and redeploy.
2. If the API is meant to be publicly, anonymously readable with no credentialed cross-origin flow, the wildcard may be intentional. Downgrade to an acknowledged exception by adjusting the alert's severity/routing; do not silence it or widen `.gitleaksignore`-style suppression lists.
3. Verify: `allowed_wildcard` stops incrementing and (if origins were pinned) `allowed_origin` picks up the same traffic.

## Related

- [api](api.md): API-side alerts.
