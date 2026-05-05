#!/usr/bin/env bash
# GET /v1/account/me — your account info.
#
# Requires Authorization: Bearer <key>. Returns key_id, label, tier,
# rate_limit_per_min, created_at. Anonymous callers get 401.
set -euo pipefail
BASE="${API_BASE_URL:-https://api.ratesengine.net}"
KEY="${RATESENGINE_API_KEY:?set RATESENGINE_API_KEY first (see 02-signup.sh)}"

curl -sS --fail "$BASE/v1/account/me" \
  -H "Authorization: Bearer $KEY"
echo
