#!/usr/bin/env bash
# POST /v1/register — open registration, the one-curl onboarding path.
#
# Creates a free-tier platform account and mints its first API key in
# a single unauthenticated POST. No email required (an optional
# contact email + display name may be sent as JSON). The plaintext
# api_key is returned exactly once — store it; if lost, register
# again. Per-IP throttled (shared budget with /v1/signup).
#
# Production-safety: a casual `bash 16-register.sh` would create a
# real account row in prod every time, so the same
# CONFIRM_PROD_SIGNUP=1 gate as 02-signup.sh applies. Local /
# staging deployments (any non-prod API_BASE_URL) bypass the gate.
set -euo pipefail
BASE="${API_BASE_URL:-https://api.stellarindex.io}"
NAME="${1:-curl-example}"

if [[ "$BASE" == *"api.stellarindex.io"* ]] && [[ "${CONFIRM_PROD_SIGNUP:-}" != "1" ]]; then
  cat >&2 <<EOF
Refusing to POST to production without confirmation.

This would create a real account row at $BASE named '$NAME' —
a side-effect that's not what most "smoke-test the example"
runs intend.

If you actually want to register against production, run:
  CONFIRM_PROD_SIGNUP=1 bash $0 [name]
EOF
  exit 1
fi

curl -fsSL -X POST "$BASE/v1/register" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"$NAME\"}" | jq '{
    account_id: .data.account_id,
    api_key: .data.api_key,
    tier: .data.tier,
    limits: .data.limits
  }'
