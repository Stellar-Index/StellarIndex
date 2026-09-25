---
title: Runbook — api-cors-wildcard
last_verified: 2026-09-25
status: living
severity: P3
---

# Runbook — `stellarindex_api_cors_wildcard_in_prod`

## At a glance

| Field | Value |
| ----- | ----- |
| Alert | `stellarindex_api_cors_wildcard_in_prod` |
| Severity | P3 (ticket) |
| Detected by | `configs/prometheus/rules.r1/api-security.yml` (mirror: `deploy/monitoring/rules/api-security.yml`) |
| Emitted by | `internal/api/v1/middleware` CORS decision path, counted in `stellarindex_api_cors_decisions_total{outcome="allowed_wildcard"}` (internal/obs/metrics.go) |
| Typical MTTR | 10–30 min (config change + redeploy) |
| Impact | `STELLARINDEX_ALLOWED_ORIGINS=*` is configured on a production api instance and is actively matching cross-origin requests. If `auth_mode` is credentialed, any origin can read authenticated responses (F-1244). |

## Background

`stellarindex_api_cors_decisions_total` is labelled by outcome:
`no_origin`, `allowed_origin`, `allowed_wildcard`, `denied`. A startup
warning (`warnOpenCORS`, `cmd/stellarindex-api/main.go`) already fires
once at boot when the wildcard policy is configured, but it is easy to
miss in a deploy log; this alert is the same guard, continuous, and
gated on the wildcard actually being EXERCISED by real traffic rather
than merely configured.

## Diagnosis

```sh
# Confirm the running config
curl -fs http://localhost:9464/debug/config 2>/dev/null | grep -i allowed_origins || true
echo "$STELLARINDEX_ALLOWED_ORIGINS"

# Confirm which outcome is firing and at what rate
curl -fs http://localhost:9464/metrics | grep stellarindex_api_cors_decisions_total
```

## Resolution

1. If credentials (cookies / API keys tied to a session) are in play for
   browser-originated traffic, replace the wildcard with an explicit
   allow-list of the known frontend origin(s) and redeploy.
2. If the API is genuinely meant to be publicly, anonymously readable
   with no credentialed cross-origin flow, the wildcard may be
   intentional — downgrade this to an acknowledged exception rather than
   silencing the alert (do not widen `.gitleaksignore`-style suppression
   lists for this; adjust the alert's severity/routing instead if the
   policy is confirmed correct for this deployment).
3. Verify the fix by confirming `allowed_wildcard` stops incrementing
   and (if origins were pinned) `allowed_origin` picks up the same
   traffic.
