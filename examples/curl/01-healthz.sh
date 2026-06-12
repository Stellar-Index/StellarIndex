#!/usr/bin/env bash
# GET /v1/healthz — liveness probe.
#
# Returns 200 with `{"status":"ok"}` when the API process is up
# and reachable. Doesn't validate dependencies (use /readyz for
# that). Useful for load-balancer health checks.
set -euo pipefail
BASE="${API_BASE_URL:-https://api.stellarindex.io}"
curl -sS --fail "$BASE/v1/healthz"
echo
